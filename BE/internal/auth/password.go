package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

const passwordIterations = 600000

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("password must contain 12 to 1024 bytes")
	}
	return canonicalPasswordHash(password)
}

// Legacy accounts may have shorter passwords. Rehashing their verified secret
// strengthens its work factor without silently changing the user's password.
func canonicalPasswordHash(password string) (string, error) {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$600000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func CheckPassword(encoded, password string) bool {
	p := strings.Split(encoded, "$")
	iterations := passwordIterations
	var saltText, keyText string
	if len(p) == 4 && p[0] == "pbkdf2-sha256" && p[1] == "600000" {
		saltText, keyText = p[2], p[3]
	} else if len(p) == 5 && p[0] == "" && p[1] == "pbkdf2-sha256" && p[2] == "i=210000" {
		iterations = 210000
		saltText, keyText = p[3], p[4]
	} else {
		return false
	}
	if len(password) == 0 || len(password) > 1024 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(saltText)
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(keyText)
	if err != nil || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}
