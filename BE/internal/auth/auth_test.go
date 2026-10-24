package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

type failingSessionRevoker struct {
	Store
}

func (f failingSessionRevoker) RevokeSessionByToken(context.Context, []byte, time.Time) error {
	return errors.New("database unavailable")
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestPasswordHashing(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected short password to fail")
	}
	h1, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("hashes with random salt must differ")
	}
	if strings.Contains(h1, "correct-horse") {
		t.Fatal("plaintext leaked in hash")
	}
	ok, err := VerifyPassword("correct-horse-battery-staple", h1)
	if err != nil || !ok {
		t.Fatalf("valid password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong-password", h1)
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
	if _, err := VerifyPassword("correct-horse-battery-staple", "plaintext_not_hashed"); err == nil {
		t.Fatal("plaintext hash accepted")
	}
}

func TestLoginStopsWhenExistingSessionCannotBeRevoked(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(failingSessionRevoker{Store: store}, Config{})
	if _, err := svc.BootstrapAdmin(context.Background(), "admin", "supersecret123"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"supersecret123"}`))
	req.AddCookie(&http.Cookie{Name: CookieName, Value: "existing-session"})
	rr := httptest.NewRecorder()
	svc.HandleLogin(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the existing session cannot be revoked, got %d", rr.Code)
	}
}

func TestSessionLifecycleAndCSRF(t *testing.T) {
	clock := newFakeClock()
	store := NewMemoryStore()
	svc := NewService(store, Config{
		IdleTimeout:      30 * time.Minute,
		AbsoluteLifetime: 8 * time.Hour,
		RotationInterval: 15 * time.Minute,
		GracePeriod:      30 * time.Second,
		Now:              clock.Now,
	})

	if _, err := svc.BootstrapAdmin(context.Background(), "admin", "supersecret123"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", svc.HandleLogin)
	mux.Handle("POST /api/v1/auth/logout", svc.RequireAuth(http.HandlerFunc(svc.HandleLogout)))
	mux.Handle("GET /api/v1/auth/me", svc.RequireAuth(http.HandlerFunc(svc.HandleMe)))
	mux.Handle("GET /api/v1/auth/csrf", svc.RequireAuth(http.HandlerFunc(svc.HandleCSRF)))

	// Login gagal
	badLogin := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"wrongpassword"}`))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, badLogin)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong password, got %d", rr.Code)
	}

	// Login berhasil
	loginReq := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"supersecret123"}`))
	loginReq.Header.Set("X-Forwarded-Proto", "https")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, loginReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName {
		t.Fatalf("expected session cookie, got %v", cookies)
	}
	sessionCookie := cookies[0]
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags invalid: %+v", sessionCookie)
	}
	if strings.Contains(rr.Body.String(), sessionCookie.Value) || strings.Contains(rr.Body.String(), "supersecret123") || strings.Contains(rr.Body.String(), "pbkdf2") {
		t.Fatal("sensitive data leaked in login response body")
	}

	// Profil user aktif
	meReq := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	meReq.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, meReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me status=%d", rr.Code)
	}

	// Request mutating wajib CSRF token
	logoutNoCSRF := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	logoutNoCSRF.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, logoutNoCSRF)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", rr.Code)
	}

	// Ambil CSRF token
	csrfReq := httptest.NewRequest("GET", "/api/v1/auth/csrf", nil)
	csrfReq.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, csrfReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /csrf status=%d", rr.Code)
	}
	var csrfResp struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&csrfResp); err != nil || csrfResp.CSRFToken == "" {
		t.Fatalf("invalid csrf response: %v", err)
	}

	// Rotasi berkala dan grace period
	clock.Advance(15 * time.Minute)
	rotReq := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	rotReq.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, rotReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 during rotation, got %d", rr.Code)
	}
	newCookies := rr.Result().Cookies()
	if len(newCookies) != 1 || newCookies[0].Value == sessionCookie.Value {
		t.Fatalf("expected rotated session cookie, got %v", newCookies)
	}
	rotatedCookie := newCookies[0]

	// Token lama masih berlaku selama grace period 30 detik
	clock.Advance(10 * time.Second)
	graceReq := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	graceReq.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, graceReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("old token rejected during grace period: %d", rr.Code)
	}

	// Token lama kedaluwarsa setelah lewat grace period
	clock.Advance(21 * time.Second)
	expiredGraceReq := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	expiredGraceReq.AddCookie(sessionCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, expiredGraceReq)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old token accepted after grace period: %d", rr.Code)
	}

	// Token baru tetap valid
	validNewReq := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	validNewReq.AddCookie(rotatedCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, validNewReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("new token failed: %d", rr.Code)
	}

	// Tolak cross-origin CSRF
	badOriginReq := httptest.NewRequest("POST", "http://localhost:8080/api/v1/auth/logout", nil)
	badOriginReq.Host = "localhost:8080"
	badOriginReq.Header.Set("Origin", "https://evil.example.com")
	badOriginReq.Header.Set(CSRFHeader, csrfResp.CSRFToken)
	badOriginReq.AddCookie(rotatedCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, badOriginReq)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request not rejected: %d", rr.Code)
	}

	// Logout dengan CSRF token mencabut session
	logoutReq := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	logoutReq.Header.Set(CSRFHeader, csrfResp.CSRFToken)
	logoutReq.AddCookie(rotatedCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, logoutReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("logout failed: %d %s", rr.Code, rr.Body.String())
	}

	afterLogoutReq := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	afterLogoutReq.AddCookie(rotatedCookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, afterLogoutReq)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("session still active after logout: %d", rr.Code)
	}
}

func TestIdleAndAbsoluteExpirationAndConcurrency(t *testing.T) {
	clock := newFakeClock()
	store := NewMemoryStore()
	svc := NewService(store, Config{
		IdleTimeout:      30 * time.Minute,
		AbsoluteLifetime: 2 * time.Hour,
		RotationInterval: 15 * time.Minute,
		GracePeriod:      30 * time.Second,
		Now:              clock.Now,
	})
	user, err := svc.BootstrapAdmin(context.Background(), "admin", "supersecret123")
	if err != nil {
		t.Fatal(err)
	}

	handler := svc.RequireAuth(http.HandlerFunc(svc.HandleMe))
	loginHelper := func() *http.Cookie {
		body := bytes.NewBufferString(`{"username":"admin","password":"supersecret123"}`)
		rr := httptest.NewRecorder()
		svc.HandleLogin(rr, httptest.NewRequest("POST", "/api/v1/auth/login", body))
		if rr.Code != http.StatusOK {
			t.Fatalf("login failed: %d", rr.Code)
		}
		return rr.Result().Cookies()[0]
	}

	// Uji idle timeout
	c1 := loginHelper()
	clock.Advance(30 * time.Minute)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(c1)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected idle timeout 401, got %d", rr.Code)
	}

	// Uji konkurensi saat rotasi session
	c2 := loginHelper()
	clock.Advance(15 * time.Minute)

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	rotatedTokens := make(chan string, workers)
	failures := make(chan int, workers)

	for range workers {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
			r.AddCookie(c2)
			handler.ServeHTTP(rec, r)
			if rec.Code != http.StatusOK {
				failures <- rec.Code
				return
			}
			for _, ck := range rec.Result().Cookies() {
				if ck.Name == CookieName && ck.Value != "" {
					rotatedTokens <- ck.Value
				}
			}
		}()
	}
	wg.Wait()
	close(rotatedTokens)
	close(failures)

	for code := range failures {
		t.Fatalf("concurrent request failed with status %d", code)
	}
	var newToken string
	rotCount := 0
	for tok := range rotatedTokens {
		rotCount++
		newToken = tok
	}
	if rotCount != 1 {
		t.Fatalf("expected exactly 1 rotation across concurrent requests, got %d", rotCount)
	}
	currentCookie := &http.Cookie{Name: CookieName, Value: newToken}

	// Peningkatan privilege memicu rotasi pada request berikutnya
	clock.Advance(1 * time.Minute)
	if err := store.MarkUserSessionsForRotation(context.Background(), user.ID); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(currentCookie)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after privilege elevation, got %d", rr.Code)
	}
	if len(rr.Result().Cookies()) != 1 || rr.Result().Cookies()[0].Value == currentCookie.Value {
		t.Fatal("expected session rotation on privilege elevation")
	}
	currentCookie = rr.Result().Cookies()[0]

	// Rotasi berkala tidak memperpanjang batas absolute expiration
	for range 5 {
		clock.Advance(20 * time.Minute)
		rr = httptest.NewRecorder()
		req = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
		req.AddCookie(currentCookie)
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected active session before absolute expiry, got %d", rr.Code)
		}
		if cks := rr.Result().Cookies(); len(cks) == 1 && cks[0].Value != "" {
			currentCookie = cks[0]
		}
	}

	// Lewat dari batas absolute expiration (2 jam)
	clock.Advance(5 * time.Minute)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(currentCookie)
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 at absolute expiration despite activity and rotations, got %d", rr.Code)
	}
}
