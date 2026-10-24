package auth

import (
	"errors"
	"strings"
)

func ValidateUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if len(username) < 1 || len(username) > 100 || strings.ContainsAny(username, " \t\r\n\x00") {
		return "", errors.New("username must contain 1 to 100 bytes without whitespace or NUL")
	}
	return username, nil
}
