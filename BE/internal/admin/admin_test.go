package admin

import (
	"terserah/backend/internal/auth"
	"testing"
)

func TestAdminInputValidation(t *testing.T) {
	if name, err := auth.ValidateUsername(" user-name "); err != nil || name != "user-name" {
		t.Fatal("valid username rejected")
	}
	for _, name := range []string{"", "a b", "user\x00", "x\ny"} {
		if _, err := auth.ValidateUsername(name); err == nil {
			t.Fatal("invalid username accepted")
		}
	}
	for _, id := range []string{"0", "-1", "abc", "9223372036854775808"} {
		if _, err := parseID(id); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
}
