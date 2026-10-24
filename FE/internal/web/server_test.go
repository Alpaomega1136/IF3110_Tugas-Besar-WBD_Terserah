package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testHandler(t *testing.T, backendURL string) http.Handler {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(target)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestLoginPage(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Terserah Database Platform") {
		t.Fatalf("GET /login: status=%d body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `/static/js/login.js`) {
		t.Fatal("login script not included in page")
	}
}

func TestRootRedirectAndStatic(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard" {
		t.Fatalf("unexpected root response: %d %q", w.Code, w.Header().Get("Location"))
	}

	for _, asset := range []string{"/static/css/style.css", "/static/js/login.js", "/static/js/api.js", "/static/js/auth.js", "/static/js/dashboard.js", "/static/js/users.js"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, asset, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d", asset, w.Code)
		}
	}
}

func TestReverseProxyPreservesOriginHostAndCookies(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost:3000" || r.Header.Get("Origin") != "http://localhost:3000" {
			t.Errorf("backend received host=%q origin=%q", r.Host, r.Header.Get("Origin"))
		}
		if got := r.Header.Get("X-Forwarded-Proto"); got != "http" {
			t.Errorf("expected forwarded proto=http, got %q", got)
		}
		w.Header().Set("Set-Cookie", "terserah_session=opaque; Path=/; HttpOnly; SameSite=Lax")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"user":{"username":"admin"}}`)
	}))
	defer backend.Close()

	h := testHandler(t, backend.URL)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"not_logged"}`))
	r.Host = "localhost:3000"
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("X-Forwarded-Proto", "https") // Must not be trusted from the browser.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Set-Cookie"), "terserah_session=") {
		t.Fatalf("proxy status=%d cookie=%q", w.Code, w.Header().Get("Set-Cookie"))
	}
}

func TestDashboardPage(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")

	w := httptest.NewRecorder()
	h.ServeHTTP(
		w,
		httptest.NewRequest(http.MethodGet, "/dashboard", nil),
	)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /dashboard returned %d", w.Code)
	}

	body := w.Body.String()

	for _, expected := range []string{
		"Dashboard - Terserah",
		`id="dashboard-content" hidden`,
		`/static/js/dashboard.js`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard page missing %q", expected)
		}
	}

	if strings.Contains(body, `id="login-form"`) {
		t.Fatal("dashboard rendered login form: template content collision")
	}

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestAdminUsersPage(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")

	w := httptest.NewRecorder()
	h.ServeHTTP(
		w,
		httptest.NewRequest(http.MethodGet, "/admin/users", nil),
	)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/users returned %d", w.Code)
	}

	body := w.Body.String()

	for _, expected := range []string{
		"User Management - Terserah",
		`id="create-user-form"`,
		`id="create-password"`,
		`id="users-section"`,
		`/static/js/users.js`,
		`id="edit-user-form"`,
		`id="edit-user-submit"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("users page missing %q", expected)
		}
	}

	if strings.Contains(body, `id="login-form"`) {
		t.Fatal("users page rendered login content")
	}

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestUpdateUserProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", r.Method)
			}

			if r.URL.Path != "/api/v1/admin/users/12" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}

			if got := r.Header.Get("X-CSRF-Token"); got != "test-token" {
				t.Errorf("CSRF token = %q", got)
			}

			if got := r.Header.Get("Cookie"); got != "terserah_session=test-session" {
				t.Errorf("cookie = %q", got)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w,
				`{"user":{"id":"12","is_active":false}}`,
			)
		},
	))
	defer backend.Close()

	h := testHandler(t, backend.URL)

	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/admin/users/12",
		strings.NewReader(`{"is_active":false}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "test-token")
	req.Header.Set("Cookie", "terserah_session=test-session")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PATCH returned %d, want 200", w.Code)
	}
}
