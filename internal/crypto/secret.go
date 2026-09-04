package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"db-flashback/internal/config"
)

const prefixV1 = "enc:v1:"

var (
	keyOnce   sync.Once
	cachedKey []byte
	keyErr    error
)

func IsSealed(s string) bool {
	return strings.HasPrefix(s, prefixV1)
}

func ParseKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty data key")
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("data key must be 32-byte hex or standard base64")
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("data key must be 32 bytes, got %d", len(b))
	}
	return b, nil
}

func configuredKeyRaw() string {
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_DATA_KEY")); v != "" {
		return v
	}
	if cfg := config.TryGlobal(); cfg != nil {
		return strings.TrimSpace(cfg.Flashback.DataKey)
	}
	return ""
}

func Key() ([]byte, error) {
	keyOnce.Do(func() {
		raw := configuredKeyRaw()
		if raw == "" {
			keyErr = fmt.Errorf("未配置 FLASHBACK_DATA_KEY / flashback.data_key，无法加密敏感字段")
			return
		}
		cachedKey, keyErr = ParseKey(raw)
	})
	if keyErr != nil {
		return nil, keyErr
	}
	out := make([]byte, len(cachedKey))
	copy(out, cachedKey)
	return out, nil
}

func HasKey() bool {
	_, err := Key()
	return err == nil
}

func ResetKeyForTest() {
	keyOnce = sync.Once{}
	cachedKey = nil
	keyErr = nil
}

func Seal(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	key, err := Key()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return prefixV1 + base64.StdEncoding.EncodeToString(out), nil
}

func Open(val string) (string, error) {
	if val == "" || !IsSealed(val) {
		return val, nil
	}
	key, err := Key()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(val, prefixV1))
	if err != nil {
		return "", fmt.Errorf("decode sealed secret: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("sealed secret too short")
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}

func MustSeal(plain string) (string, error) {
	if strings.TrimSpace(plain) == "" {
		return "", nil
	}
	if IsSealed(plain) {
		return plain, nil
	}
	return Seal(plain)
}
