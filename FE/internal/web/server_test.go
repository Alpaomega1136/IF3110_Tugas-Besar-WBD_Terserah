package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

	for _, asset := range []string{"/static/css/style.css", "/static/js/login.js", "/static/js/api.js", "/static/js/auth.js", "/static/js/dashboard.js", "/static/js/users.js", "/static/js/groups.js", "/static/js/grants.js", "/static/js/drafts.js", "/static/js/history.js"} {
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
		_, _ = io.WriteString(w, `{"id":"1","username":"admin","is_platform_admin":true}`)
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
		`<th scope="col">Workspace</th>`,
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
		`aria-describedby="create-password-help create-password-error"`,
		`aria-describedby="edit-password-help edit-password-error"`,
		"12–1024 byte",
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

						w.Header().Set("Content-Type", "application/json")

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
				"Maksimum 48 KiB SQL dan total JSON permintaan 64 KiB.",
				`id="draft-list-body"`,
				`id="draft-detail"`,
				`id="draft-execution"`,
				`id="draft-execute"`,
				`id="draft-execution-result"`,
				`href="/databases/5/history"`,
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

func TestHistoryAPIProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		response string
	}{
		{
			"list",
			"/api/v1/databases/5/migrations",
			`{"migrations":[]}`,
		},
		{
			"detail by version",
			"/api/v1/databases/5/migrations/7",
			`{"migration":{"id":"3","database_id":"5","version":7,"status":"success","sql":"SELECT 1;"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != tc.path {
						t.Errorf("upstream received %s %s", r.Method, r.URL.Path)
					}

					if r.Host != "localhost:3000" ||
						r.Header.Get("Origin") != "http://localhost:3000" {
						t.Error("Host/Origin not preserved")
					}

					if got := r.Header.Get("Cookie"); got != "terserah_session=test-session" {
						t.Errorf("cookie=%q", got)
					}

					if got := r.Header.Get("X-CSRF-Token"); got != "" {
						t.Errorf("read-only GET unexpectedly sent CSRF token %q", got)
					}

					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.response)
				},
			))
			defer backend.Close()

			h := testHandler(t, backend.URL)

			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Host = "localhost:3000"
			r.Header.Set("Origin", "http://localhost:3000")
			r.Header.Set("Cookie", "terserah_session=test-session")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != http.StatusOK ||
				strings.TrimSpace(w.Body.String()) != tc.response {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestHistoryAPIProxyPreservesForbidden(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(
				w,
				`{"error":{"code":"forbidden","message":"Insufficient database permission"}}`,
			)
		},
	))
	defer backend.Close()

	h := testHandler(t, backend.URL)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet,
		"/api/v1/databases/5/migrations",
		nil,
	))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", w.Code)
	}

	if !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Fatal("upstream permission error not preserved")
	}
}

func TestHistoryPageAndInvalidIDs(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/databases/5/history", http.StatusOK},
		{"/databases/0/history", http.StatusBadRequest},
		{"/databases/nope/history", http.StatusBadRequest},
		{"/databases/9223372036854775808/history", http.StatusBadRequest},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if w.Code != tc.want {
				t.Fatalf("status=%d, want=%d", w.Code, tc.want)
			}

			if tc.want != http.StatusOK {
				return
			}

			for _, part := range []string{
				"Migration History - Terserah",
				`data-database-id="5"`,
				`id="history-list-body"`,
				`id="history-detail-sql"`,
				`id="history-refresh"`,
				`/static/js/history.js`,
			} {
				if !strings.Contains(w.Body.String(), part) {
					t.Errorf("history page missing %q", part)
				}
			}

			if strings.Contains(w.Body.String(), `id="login-form"`) {
				t.Error("history page rendered login form")
			}

			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control=%q, want no-store", got)
			}
		})
	}
}

func TestMergedAPIContractProxy(t *testing.T) {
	fixtures := map[string]struct {
		method, path, body string
		status             int
	}{
		"csrf":      {"GET", "/api/v1/auth/csrf", `{"csrf_token":"before-login"}`, 200},
		"login":     {"POST", "/api/v1/auth/login", `{"id":"31","username":"editor","is_platform_admin":false}`, 200},
		"me":        {"GET", "/api/v1/auth/me", `{"id":"31","username":"editor","is_platform_admin":false}`, 200},
		"databases": {"GET", "/api/v1/databases", `{"databases":[{"id":"12","name":"target","database_name":"app","access":"edit"}]}`, 200},
		"draft":     {"GET", "/api/v1/databases/12/drafts/9007199254740993", `{"id":"9007199254740993","database_id":"12","revision":"7","title":"New table","sql":"CREATE TABLE t(id int);"}`, 200},
		"patch":     {"PATCH", "/api/v1/databases/12/drafts/9007199254740993", `{"id":"9007199254740993","database_id":"12","revision":"8","title":"New table","sql":"CREATE TABLE t(id int);"}`, 200},
		"execute":   {"POST", "/api/v1/databases/12/drafts/9007199254740993/execute", `{"execution_id":"abcdef0123456789","version":"9","status":"success","error_code":null}`, 200},
		"pending":   {"POST", "/api/v1/databases/12/drafts/9007199254740994/execute", `{"execution_id":"abcdef0123456790","version":"10","status":"unknown","error_code":null}`, 202},
		"history":   {"GET", "/api/v1/databases/12/migrations/9007199254740993", `{"database_id":"12","version":"9007199254740993","username":"editor","execution_id":"abcd","sql":"CREATE TABLE t(id int);"}`, 200},
		"logout":    {"POST", "/api/v1/auth/logout", "", 204},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "localhost:3000" {
			t.Errorf("backend host %q", r.Host)
		}
		if r.Method != http.MethodGet {
			if r.Header.Get("Origin") != "http://localhost:3000" || r.Header.Get("X-CSRF-Token") != "client-token" {
				t.Errorf("missing Origin/CSRF for %s %s", r.Method, r.URL.Path)
			}
		}
		if r.Header.Get("Cookie") != "terserah_session=test-opaque" {
			t.Errorf("missing cookie on %s", r.URL.Path)
		}
		for _, f := range fixtures {
			if r.Method == f.method && r.URL.Path == f.path {
				if f.path == "/api/v1/auth/csrf" {
					w.Header().Set("Set-Cookie", "terserah_login_csrf=opaque; Path=/; HttpOnly; SameSite=Lax")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(f.status)
				_, _ = io.WriteString(w, f.body)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer backend.Close()

	h := testHandler(t, backend.URL)
	for name, f := range fixtures {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(f.method, f.path, nil)
			r.Host = "localhost:3000"
			r.Header.Set("Cookie", "terserah_session=test-opaque")
			if f.method != http.MethodGet {
				r.Header.Set("Origin", "http://localhost:3000")
				r.Header.Set("X-CSRF-Token", "client-token")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != f.status || strings.TrimSpace(w.Body.String()) != f.body {
				t.Fatalf("%s %s: status=%d body=%q", f.method, f.path, w.Code, w.Body.String())
			}
			if name == "csrf" && !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") {
				t.Fatal("prelogin cookie missing from proxy response")
			}
		})
	}
}

func TestAdminDatabaseConnectionsPage(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/databases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/databases: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, part := range []string{
		"Database Connections - Terserah", `id="connections-content" hidden`,
		`id="connections-forbidden"`, `id="connections-body"`,
		`id="connection-create-form"`, `id="connection-edit-form"`,
		`id="connection-create-password"`, `id="connection-edit-password"`,
		`/static/js/connections.js`,
	} {
		if !strings.Contains(w.Body.String(), part) {
			t.Errorf("connections page missing %q", part)
		}
	}
	if strings.Contains(w.Body.String(), `id="login-form"`) {
		t.Error("connections page rendered login form")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q, want no-store", got)
	}
	for _, asset := range []string{"/static/js/connections.js", "/static/css/style.css"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, asset, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d", asset, w.Code)
		}
	}
}

func TestAdminDatabaseConnectionsNavigation(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	for _, path := range []string{"/dashboard", "/admin/users", "/admin/groups", "/admin/grants"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `href="/admin/databases"`) {
				t.Fatalf("page %s missing admin connections link; status=%d", path, w.Code)
			}
		})
	}
}

func TestAdminDatabaseConnectionsProxyContract(t *testing.T) {
	const largeID = "9007199254740993"
	fixtures := []struct {
		method, path, payload, response string
		status                          int
	}{
		{"GET", "/api/v1/admin/databases", "", `{"databases":[{"id":"9007199254740993","name":"target","host":"localhost","port":5432,"database_name":"app","username":"demo","sslmode":"disable"}]}`, 200},
		{"POST", "/api/v1/admin/databases", `{"name":"target","host":"localhost","port":5432,"database_name":"app","username":"demo","password":"secret","sslmode":"disable"}`, `{"id":"9007199254740993","name":"target"}`, 201},
		{"PATCH", "/api/v1/admin/databases/" + largeID, `{"name":"updated","username":"editor","sslmode":"require"}`, `{"id":"9007199254740993","name":"updated"}`, 200},
		{"POST", "/api/v1/admin/databases/" + largeID + "/test", "", `{"status":"ok"}`, 200},
		{"POST", "/api/v1/admin/databases/" + largeID + "/test", "", `{"error":{"code":"target_unavailable","message":"Cannot connect"}}`, 503},
		{"PATCH", "/api/v1/admin/databases/" + largeID, "", `{"error":{"code":"connection_conflict","message":"Conflict"}}`, 409},
		{"DELETE", "/api/v1/admin/databases/" + largeID, "", "", 204},
		{"GET", "/api/v1/admin/databases", "", `{"error":{"code":"forbidden","message":"Access denied"}}`, 403},
	}
	var expected struct{ method, path, payload string }
	var response string
	var status int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expected.method || r.URL.Path != expected.path {
			t.Errorf("upstream request: %s %s, want %s %s", r.Method, r.URL.Path, expected.method, expected.path)
		}
		if r.Host != "localhost:3000" || r.Header.Get("Cookie") != "terserah_session=opaque" {
			t.Errorf("proxy dropped browser host or session cookie")
		}
		if r.Method != http.MethodGet {
			if r.Header.Get("Origin") != "http://localhost:3000" || r.Header.Get("X-CSRF-Token") != "token" {
				t.Errorf("proxy dropped CSRF headers")
			}
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != expected.payload {
			t.Errorf("payload=%q, want %q", body, expected.payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	defer backend.Close()
	h := testHandler(t, backend.URL)
	for index, tc := range fixtures {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			expected.method, expected.path, expected.payload = tc.method, tc.path, tc.payload
			status, response = tc.status, tc.response
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.payload))
			r.Host = "localhost:3000"
			r.Header.Set("Cookie", "terserah_session=opaque")
			if tc.method != http.MethodGet {
				r.Header.Set("Origin", "http://localhost:3000")
				r.Header.Set("X-CSRF-Token", "token")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || strings.TrimSpace(w.Body.String()) != tc.response {
				t.Fatalf("proxy %s %s: status=%d body=%q", tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}
}

func TestPhase3GrantSelectionAndViewerRoutes(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	for _, tc := range []struct {
		path     string
		status   int
		contains []string
	}{
		{"/admin/grants", http.StatusOK, []string{`id="grant-database"`, `id="grants-refresh"`, `id="grants-no-databases"`, `/static/js/grants.js`}},
		{"/dashboard", http.StatusOK, []string{`<th scope="col">Workspace</th>`, `/static/js/dashboard.js`}},
		{"/databases/9007199254740993/viewer", http.StatusOK, []string{`data-database-id="9007199254740993"`, `id="viewer-workspace" hidden`, `id="viewer-schemas-list"`, `/static/js/viewer.js`}},
		{"/databases/0/viewer", http.StatusBadRequest, nil},
		{"/databases/abc/viewer", http.StatusBadRequest, nil},
		{"/databases/9223372036854775808/viewer", http.StatusBadRequest, nil},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			for _, part := range tc.contains {
				if !strings.Contains(w.Body.String(), part) {
					t.Errorf("missing %q", part)
				}
			}
			if tc.status == http.StatusOK && w.Header().Get("Cache-Control") != "no-store" {
				t.Error("page must not be cached")
			}
		})
	}
	for _, path := range []string{"/static/js/grants.js", "/static/js/dashboard.js", "/static/js/viewer.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d", path, w.Code)
		}
	}
}

func TestPhase3GrantAndViewerProxyContract(t *testing.T) {
	const largeID = "9007199254740993"
	for _, tc := range []struct {
		method, path, body, response string
		status                       int
	}{
		{"GET", "/api/v1/admin/groups", "", `{"groups":[{"id":"19","name":"team","user_ids":["5"],"grants":[{"group_id":"19","database_id":"9007199254740993","access_level":"view"}]}]}`, 200},
		{"GET", "/api/v1/admin/databases", "", `{"databases":[{"id":"9007199254740993","name":"target","database_name":"app"}]}`, 200},
		{"PUT", "/api/v1/admin/groups/19/databases/" + largeID, `{"access_level":"edit"}`, `{"grant":{"group_id":"19","database_id":"9007199254740993","access_level":"edit"}}`, 200},
		{"DELETE", "/api/v1/admin/groups/19/databases/" + largeID, "", `{"status":"ok"}`, 200},
		{"GET", "/api/v1/databases", "", `{"databases":[{"id":"9007199254740993","name":"target","database_name":"app","access":"view"}]}`, 200},
		{"GET", "/api/v1/databases/" + largeID + "/schemas", "", `{"schemas":["public"]}`, 200},
		{"GET", "/api/v1/databases/" + largeID + "/schemas", "", `{"error":{"code":"forbidden","message":"Access revoked"}}`, 403},
	} {
		t.Run(tc.method+" "+tc.path+" "+strconv.Itoa(tc.status), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("upstream %s %s", r.Method, r.URL.Path)
				}
				if r.Host != "localhost:3000" || r.Header.Get("Cookie") != "terserah_session=opaque" {
					t.Errorf("proxy lost origin host or cookie")
				}
				if tc.method != http.MethodGet && (r.Header.Get("Origin") != "http://localhost:3000" || r.Header.Get("X-CSRF-Token") != "csrf-test") {
					t.Error("proxy lost mutation origin/CSRF")
				}
				payload, _ := io.ReadAll(r.Body)
				if string(payload) != tc.body {
					t.Errorf("payload=%q, want %q", payload, tc.body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer backend.Close()
			h := testHandler(t, backend.URL)
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Host = "localhost:3000"
			r.Header.Set("Cookie", "terserah_session=opaque")
			if tc.method != http.MethodGet {
				r.Header.Set("Origin", "http://localhost:3000")
				r.Header.Set("X-CSRF-Token", "csrf-test")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || strings.TrimSpace(w.Body.String()) != tc.response {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestViewerCompletePageAndEmbeddedModule(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/databases/9007199254740993/viewer", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("viewer page status=%d body=%s", w.Code, w.Body.String())
	}
	for _, expected := range []string{
		`data-database-id="9007199254740993"`, `id="viewer-schemas-list"`, `id="viewer-tables-list"`,
		`id="viewer-columns-body"`, `id="viewer-constraints-body"`, `id="viewer-indexes-body"`,
		`id="viewer-rows-head"`, `id="viewer-rows-body"`, `id="viewer-row-limit"`,
		`id="viewer-rows-prev"`, `id="viewer-rows-next"`, `id="viewer-rows-error"`,
		`id="viewer-workspace" hidden`, `/static/js/viewer.js`, `Migration History`,
	} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Errorf("viewer page missing %s", expected)
		}
	}
	if strings.Contains(w.Body.String(), `id="login-form"`) || strings.Contains(w.Body.String(), `id="draft-sql"`) {
		t.Fatal("viewer page rendered unrelated form")
	}
	for _, asset := range []string{"/static/js/viewer.js", "/static/css/style.css"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, asset, nil))
		if w.Code != http.StatusOK {
			t.Errorf("asset %s returned %d", asset, w.Code)
		}
	}
}

func TestViewerReadOnlyProxyPreservesEncodedPathAndErrorStatus(t *testing.T) {
	paths := []string{
		"/api/v1/databases/9007199254740993/schemas",
		"/api/v1/databases/9007199254740993/schemas/data%20%2F%20%252026/tables",
		"/api/v1/databases/9007199254740993/schemas/data%20%2F%20%252026/tables/orders%20%2F%20%22draft%22",
		"/api/v1/databases/9007199254740993/schemas/data%20%2F%20%252026/tables/orders%20%2F%20%22draft%22/rows?limit=10&offset=20",
	}
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls >= len(paths) {
			t.Error("unexpected request count")
			return
		}
		if r.Method != http.MethodGet || r.URL.RequestURI() != paths[calls] {
			t.Errorf("backend received %s %s, want GET %s", r.Method, r.URL.RequestURI(), paths[calls])
		}
		if r.Header.Get("Cookie") != "terserah_session=test-session" {
			t.Error("session cookie not forwarded")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 4 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":{"code":"result_too_large"}}`)
		} else {
			_, _ = io.WriteString(w, `{"tables":[]}`)
		}
	}))
	defer backend.Close()
	h := testHandler(t, backend.URL)
	for index, path := range paths {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Cookie", "terserah_session=test-session")
		h.ServeHTTP(w, req)
		want := http.StatusOK
		if index == 3 {
			want = http.StatusRequestEntityTooLarge
		}
		if w.Code != want {
			t.Errorf("proxy %s: got %d, want %d", path, w.Code, want)
		}
		if index == 3 && !strings.Contains(w.Body.String(), "result_too_large") {
			t.Error("proxy did not forward BE2 size error")
		}
	}
	if calls != len(paths) {
		t.Fatalf("received %d requests, want %d", calls, len(paths))
	}
}

func TestExecuteMigrationProxyPreservesRevisionAndCSRF(t *testing.T) {
	const payload = `{"revision":"9007199254740993"}`
	const response = `{"execution_id":"abc123","version":"9","status":"unknown","error_code":null}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/databases/42/drafts/9007199254740995/execute" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Host != "localhost:3000" || r.Header.Get("Origin") != "http://localhost:3000" {
			t.Error("Host or Origin changed by reverse proxy")
		}
		if r.Header.Get("X-CSRF-Token") != "csrf-test" || r.Header.Get("Cookie") != "terserah_session=opaque-test" {
			t.Error("CSRF or opaque session not forwarded")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("content type missing")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != payload {
			t.Errorf("body=%s err=%v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, response)
	}))
	defer backend.Close()
	h := testHandler(t, backend.URL)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/databases/42/drafts/9007199254740995/execute", strings.NewReader(payload))
	r.Host = "localhost:3000"
	r.Header.Set("Origin", "http://localhost:3000")
	r.Header.Set("Cookie", "terserah_session=opaque-test")
	r.Header.Set("X-CSRF-Token", "csrf-test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || strings.TrimSpace(w.Body.String()) != response {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestPhase6HistoryAndDraftPaginationPages(t *testing.T) {
	h := testHandler(t, "http://localhost:8080")
	cases := []struct {
		path     string
		expected []string
	}{
		{"/databases/9007199254740993/history", []string{
			`data-database-id="9007199254740993"`, `id="history-pagination"`,
			`id="history-prev"`, `id="history-next"`, `id="history-page-number"`,
			`id="history-reconcile-panel"`, `id="history-reconcile"`,
			`id="history-reconcile-body"`, `id="history-detail-sql"`,
		}},
		{"/databases/9007199254740993/drafts", []string{
			`data-database-id="9007199254740993"`, `id="draft-pagination"`,
			`id="draft-prev"`, `id="draft-next"`, `id="draft-page-number"`,
			`id="draft-execute"`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			for _, expected := range tc.expected {
				if !strings.Contains(w.Body.String(), expected) {
					t.Errorf("missing %q", expected)
				}
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("private page cache header missing")
			}
		})
	}
	for _, asset := range []string{
		"/static/js/pagination.js", "/static/js/history.js", "/static/js/drafts.js",
		"/static/css/style.css",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, asset, nil))
		if w.Code != http.StatusOK {
			t.Errorf("missing embedded asset %s: %d", asset, w.Code)
		}
	}
}

func TestPhase6CursorAndReconcileAPIProxy(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		status int
		body   string
	}{
		{"history first", "GET", "/api/v1/databases/9007199254740993/migrations?limit=50", 200, `{"migrations":[],"has_more":false,"next_before":null}`},
		{"history next", "GET", "/api/v1/databases/9007199254740993/migrations?limit=50&before=9007199254741199", 200, `{"migrations":[],"has_more":false,"next_before":null}`},
		{"draft next", "GET", "/api/v1/databases/9007199254740993/drafts?limit=50&after=9007199254741199", 200, `{"drafts":[],"has_more":false,"next_after":null}`},
		{"reconcile accepted", "POST", "/api/v1/databases/9007199254740993/migrations/reconcile", 200, `{"reconciled":[]}`},
		{"reconcile forbidden", "POST", "/api/v1/databases/9007199254740993/migrations/reconcile", 403, `{"error":{"code":"forbidden"}}`},
		{"reconcile conflict", "POST", "/api/v1/databases/9007199254740993/migrations/reconcile", 409, `{"error":{"code":"target_history_unprotected"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tc.method || r.URL.RequestURI() != tc.path {
					t.Errorf("got %s %s, want %s %s", r.Method, r.URL.RequestURI(), tc.method, tc.path)
				}
				if r.Host != "localhost:3000" || r.Header.Get("Cookie") != "terserah_session=test" {
					t.Errorf("lost original host or session: host=%q cookie=%q", r.Host, r.Header.Get("Cookie"))
				}
				if tc.method == "POST" {
					if r.Header.Get("Origin") != "http://localhost:3000" || r.Header.Get("X-CSRF-Token") != "csrf" {
						t.Error("reconcile POST lost origin or CSRF")
					}
				} else if r.Header.Get("X-CSRF-Token") != "" {
					t.Error("list GET should not send CSRF")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer backend.Close()
			h := testHandler(t, backend.URL)
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Host = "localhost:3000"
			r.Header.Set("Cookie", "terserah_session=test")
			if tc.method == "POST" {
				r.Header.Set("Origin", "http://localhost:3000")
				r.Header.Set("X-CSRF-Token", "csrf")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || strings.TrimSpace(w.Body.String()) != tc.body {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
