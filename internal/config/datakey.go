package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	dataKeyLineRe   = regexp.MustCompile(`(?m)^([ \t]*data_key:[ \t]*).*$`)
	flashbackHeadRe = regexp.MustCompile(`(?m)^(flashback:[ \t]*)\n`)
)

func NewDataKeyHex() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// EnsureDataKey 环境变量优先；配置为空则首次生成 32 字节 hex 并写回配置文件。
func EnsureDataKey(cfg *SvrConfig, path string) (generated bool, err error) {
	if cfg == nil {
		return false, fmt.Errorf("config is nil")
	}
	if v := strings.TrimSpace(os.Getenv("FLASHBACK_DATA_KEY")); v != "" {
		cfg.Flashback.DataKey = v
		return false, nil
	}
	if strings.TrimSpace(cfg.Flashback.DataKey) != "" {
		return false, nil
	}
	hexKey, err := NewDataKeyHex()
	if err != nil {
		return false, fmt.Errorf("generate data_key: %w", err)
	}
	if err := persistDataKey(path, hexKey); err != nil {
		return false, err
	}
	cfg.Flashback.DataKey = hexKey
	return true, nil
}

func persistDataKey(path, hexKey string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is empty, cannot persist data_key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config to persist data_key: %w", err)
	}
	body := string(raw)
	line := "data_key: \"" + hexKey + "\""
	switch {
	case dataKeyLineRe.MatchString(body):
		body = dataKeyLineRe.ReplaceAllString(body, "${1}\""+hexKey+"\"")
	case flashbackHeadRe.MatchString(body):
		body = flashbackHeadRe.ReplaceAllString(body, "${1}\n  "+line+"\n")
	default:
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "flashback:\n  " + line + "\n"
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode()
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		return fmt.Errorf("write data_key: %w", err)
	}
	return nil
}
