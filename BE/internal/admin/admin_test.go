package admin

import (
	"net/http/httptest"
	"strings"
	"terserah/backend/internal/auth"
	"testing"
)

func TestCreateGroupRejectsNULBeforeStore(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/admin/groups", strings.NewReader(`{"name":"bad\u0000group"}`))
	NewService(nil, nil).HandleCreateGroup(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_input") {
		t.Fatalf("invalid group status=%d", w.Code)
	}
}

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
