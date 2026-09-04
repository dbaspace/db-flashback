package crypto

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const passwordCost = bcrypt.DefaultCost

func HashPassword(plain string) (string, error) {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return "", fmt.Errorf("密码不能为空")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), passwordCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func CheckPassword(hash, plain string) bool {
	if strings.TrimSpace(hash) == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
