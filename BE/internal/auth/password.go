package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	pbkdf2Iterations = 210000
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 32
	MinPasswordLen   = 8
	MaxPasswordLen   = 1024
)

var (
	ErrInvalidPassword = errors.New("password must be between 8 and 1024 bytes")
	ErrMalformedHash   = errors.New("malformed password hash")
)

func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen || len(password) > MaxPasswordLen {
		return ErrInvalidPassword
	}
	return nil
}

// HashPassword membuat hash PBKDF2-HMAC-SHA256 dengan salt acak 16 byte.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$pbkdf2-sha256$i=%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// VerifyPassword mencocokkan password dengan hash PBKDF2 secara constant-time.
func VerifyPassword(password, encodedHash string) (bool, error) {
	if password == "" || len(password) > MaxPasswordLen {
		return false, nil
	}
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha256" {
		return false, ErrMalformedHash
	}
	iterStr, ok := strings.CutPrefix(parts[2], "i=")
	if !ok {
		return false, ErrMalformedHash
	}
	iterations, err := strconv.Atoi(iterStr)
	if err != nil || iterations < 10000 || iterations > 5000000 {
		return false, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < 8 {
		return false, ErrMalformedHash
	}
	expectedKey, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(expectedKey) < 16 {
		return false, ErrMalformedHash
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expectedKey))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(dk, expectedKey) == 1, nil
}
