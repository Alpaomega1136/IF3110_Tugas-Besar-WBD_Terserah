package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPreloginCSRFAndSecureCookie(t *testing.T) {
	now := time.Now()
	s := &Service{origin: "https://example.test", secure: true, csrfKey: []byte("test-only-signing-key"), now: func() time.Time { return now }}
	payload := randomToken() + "." + strconv.FormatInt(now.Add(time.Minute).Unix(), 10)
	token := payload + "." + s.sign("login:"+payload)
	r := httptest.NewRequest("POST", "https://example.test/api/v1/auth/login", nil)
	r.Header.Set("Origin", s.origin)
	r.Header.Set("X-CSRF-Token", token)
	r.AddCookie(&http.Cookie{Name: preloginCookie, Value: token})
	if !s.validPrelogin(r) {
		t.Fatal("valid prelogin CSRF rejected")
	}
	r.Header.Set("X-CSRF-Token", token+"tampered")
	if s.validPrelogin(r) {
		t.Fatal("tampered CSRF accepted")
	}
	r.Header.Set("X-CSRF-Token", token)
	r.Header.Set("Origin", "https://attacker.test")
	if s.validPrelogin(r) {
		t.Fatal("cross-origin CSRF accepted")
	}
	r.Header.Set("Origin", s.origin)
	now = now.Add(2 * time.Minute)
	if s.validPrelogin(r) {
		t.Fatal("expired prelogin CSRF accepted")
	}
	w := httptest.NewRecorder()
	s.cookie(w, CookieName, randomToken(), now.Add(time.Hour))
	value := w.Header().Get("Set-Cookie")
	for _, attribute := range []string{"Secure", "HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(value, attribute) {
			t.Fatalf("missing %s", attribute)
		}
	}
}
