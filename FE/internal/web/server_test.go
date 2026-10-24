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

	for _, asset := range []string{"/static/css/style.css", "/static/js/login.js", "/static/js/api.js", "/static/js/auth.js", "/static/js/dashboard.js", "/static/js/users.js", "/static/js/groups.js", "/static/js/grants.js", "/static/js/drafts.js"} {
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
		`id="databases-table"`,
		`id="databases-body"`,
		`id="databases-empty"`,
		`/static/js/dashboard.js`,
		`<th scope="col">Draft Migration</th>`,
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

func TestAdminGroupsPage(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")

	w := httptest.NewRecorder()

	h.ServeHTTP(
		w,
		httptest.NewRequest(http.MethodGet, "/admin/groups", nil),
	)

	if w.Code != http.StatusOK {
		t.Fatalf(
			"GET /admin/groups returned %d",
			w.Code,
		)
	}

	body := w.Body.String()

	for _, expected := range []string{
		"Group Management - Terserah",
		`id="groups-section"`,
		`id="groups-body"`,
		`id="create-group-form"`,
		`id="create-group-name"`,
		`id="create-group-submit"`,
		`id="membership-form"`,
		`id="membership-group"`,
		`id="membership-user"`,
		`id="membership-submit"`,
		`/static/js/groups.js`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("groups page missing %q", expected)
		}
	}

	if strings.Contains(body, `id="login-form"`) {
		t.Fatal("group page rendered login form")
	}

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf(
			"Cache-Control = %q, want no-store",
			got,
		)
	}
}

func TestAddGroupMemberProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPut {
				t.Errorf("method = %s, want PUT", r.Method)
			}

			expected := "/api/v1/admin/groups/2/users/3"
			if r.URL.Path != expected {
				t.Errorf("path = %s, want %s",
					r.URL.Path, expected)
			}

			if r.Header.Get("X-CSRF-Token") != "test-token" {
				t.Error("missing CSRF token")
			}

			if r.Header.Get("Cookie") !=
				"terserah_session=test-session" {
				t.Error("missing session cookie")
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		},
	))
	defer backend.Close()

	h := testHandler(t, backend.URL)

	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/admin/groups/2/users/3",
		nil,
	)

	req.Header.Set("X-CSRF-Token", "test-token")
	req.Header.Set("Cookie", "terserah_session=test-session")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT returned %d, want 200", w.Code)
	}
}

func TestRemoveGroupMemberProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}

			expected := "/api/v1/admin/groups/2/users/3"
			if r.URL.Path != expected {
				t.Errorf("path = %s, want %s", r.URL.Path, expected)
			}

			if got := r.Header.Get("X-CSRF-Token"); got != "test-token" {
				t.Errorf("X-CSRF-Token = %q", got)
			}

			if got := r.Header.Get("Cookie"); got != "terserah_session=test-session" {
				t.Errorf("Cookie = %q", got)
			}

			if got := r.Header.Get("Origin"); got != "http://localhost:3000" {
				t.Errorf("Origin = %q", got)
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		},
	))
	defer backend.Close()

	h := testHandler(t, backend.URL)

	req := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/admin/groups/2/users/3",
		nil,
	)

	req.Host = "localhost:3000"
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("X-CSRF-Token", "test-token")
	req.Header.Set("Cookie", "terserah_session=test-session")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("DELETE returned %d, want 200", w.Code)
	}
}

func TestAdminGrantsPage(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")

	w := httptest.NewRecorder()

	h.ServeHTTP(
		w,
		httptest.NewRequest(
			http.MethodGet,
			"/admin/grants",
			nil,
		),
	)

	if w.Code != http.StatusOK {
		t.Fatalf(
			"GET /admin/grants returned %d",
			w.Code,
		)
	}

	for _, expected := range []string{
		"Database Grants - Terserah",
		`id="grant-form"`,
		`id="grants-body"`,
		`/static/js/grants.js`,
	} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatalf(
				"grants page missing %q",
				expected,
			)
		}
	}

	if strings.Contains(
		w.Body.String(),
		`id="login-form"`,
	) {
		t.Fatal("grants page accidentally rendered login form")
	}

	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestDatabaseAndGrantProxy(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			"list accessible databases",
			http.MethodGet,
			"/api/v1/databases",
			"",
		},
		{
			"set database grant",
			http.MethodPut,
			"/api/v1/admin/groups/2/databases/3",
			`{"access_level":"view"}`,
		},
		{
			"revoke database grant",
			http.MethodDelete,
			"/api/v1/admin/groups/2/databases/3",
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.Method != tc.method ||
							r.URL.Path != tc.path {
							t.Errorf(
								"upstream got %s %s, want %s %s",
								r.Method, r.URL.Path,
								tc.method, tc.path,
							)
						}

						if r.Host != "localhost:3000" ||
							r.Header.Get("Origin") != "http://localhost:3000" {
							t.Errorf(
								"Host/Origin were not preserved: %q %q",
								r.Host,
								r.Header.Get("Origin"),
							)
						}

						if r.Header.Get("Cookie") != "terserah_session=test-session" {
							t.Error("session cookie missing")
						}

						if tc.method != http.MethodGet && r.Header.Get("X-CSRF-Token") != "test-token" {
							t.Error("CSRF header missing")
						}

						gotBody, _ := io.ReadAll(r.Body)

						if string(gotBody) != tc.body {
							t.Errorf("body = %q, want %q", gotBody, tc.body)
						}

						w.Header().Set("Content-Type",  "application/json")

						_, _ = io.WriteString(w, `{"status":"ok"}`)
					},
				),
			)
			defer backend.Close()

			h := testHandler(t, backend.URL)

			req := httptest.NewRequest(
				tc.method,
				tc.path,
				strings.NewReader(tc.body),
			)

			req.Host = "localhost:3000"
			req.Header.Set("Origin", "http://localhost:3000")
			req.Header.Set("Cookie", "terserah_session=test-session")

			if tc.method != http.MethodGet {
				req.Header.Set("X-CSRF-Token", "test-token")
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf(
					"proxy status = %d",
					w.Code,
				)
			}
		})
	}
}

func TestDraftsPageAndInvalidIDs(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")

	cases := []struct {
		path   string
		status int
	}{
		{"/databases/5/drafts", http.StatusOK},
		{"/databases/0/drafts", http.StatusBadRequest},
		{"/databases/nope/drafts", http.StatusBadRequest},
		{"/databases/9223372036854775808/drafts", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()

			h.ServeHTTP(
				w,
				httptest.NewRequest(http.MethodGet, tc.path, nil),
			)

			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d", w.Code, tc.status)
			}

			if tc.status != http.StatusOK {
				return
			}

			for _, part := range []string{
				"Migration Drafts - Terserah",
				`data-database-id="5"`,
				`id="draft-form"`,
				`id="draft-list-body"`,
				`id="draft-detail"`,
				`/static/js/drafts.js`,
			} {
				if !strings.Contains(w.Body.String(), part) {
					t.Errorf("missing %q", part)
				}
			}

			if strings.Contains(w.Body.String(), `id="login-form"`) {
				t.Error("rendered login content")
			}

			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("missing no-store")
			}
		})
	}
}

func TestDraftAPIProxy(t *testing.T) {
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/databases/5/drafts", ""},
		{http.MethodGet, "/api/v1/databases/5/drafts/12", ""},
		{http.MethodPost, "/api/v1/databases/5/drafts", `{"title":"init","sql":"SELECT 1;"}`},
		{http.MethodPatch, "/api/v1/databases/5/drafts/12", `{"sql":"SELECT 2;"}`},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			backend := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						if r.Method != tc.method || r.URL.Path != tc.path {
							t.Errorf("got %s %s", r.Method, r.URL.Path)
						}

						if r.Host != "localhost:3000" || r.Header.Get("Origin") != "http://localhost:3000" {
							t.Error("Host/Origin missing")
						}

						if r.Header.Get("Cookie") != "terserah_session=test-session" {
							t.Error("cookie missing")
						}

						if tc.method != http.MethodGet && r.Header.Get("X-CSRF-Token") != "test-token" {
							t.Error("csrf missing")
						}

						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Fatal(err)
						}

						if string(data) != tc.body {
							t.Errorf("body=%q want=%q", data, tc.body)
						}

						w.Header().Set("Content-Type", "application/json")

						_, _ = io.WriteString(
							w,
							`{"drafts":[]}`,
						)
					},
				),
			)
			defer backend.Close()

			h := testHandler(t, backend.URL)

			req := httptest.NewRequest(
				tc.method,
				tc.path,
				strings.NewReader(tc.body),
			)

			req.Host = "localhost:3000"
			req.Header.Set(
				"Origin",
				"http://localhost:3000",
			)
			req.Header.Set(
				"Cookie",
				"terserah_session=test-session",
			)

			if tc.method != http.MethodGet {
				req.Header.Set(
					"Content-Type",
					"application/json",
				)
				req.Header.Set(
					"X-CSRF-Token",
					"test-token",
				)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf(
					"proxy returned %d",
					w.Code,
				)
			}
		})
	}
}
