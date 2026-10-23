//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/database"
	"terserah/backend/internal/httpapi"
	"terserah/backend/internal/secretbox"
	"terserah/backend/internal/viewer"
	"terserah/backend/migrations"
)

const testOrigin = "http://localhost:8080"
const testPassword = "test-password-123"

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type m1App struct {
	db      *pgxpool.Pool
	http    http.Handler
	targets *connections.Service
	clock   *testClock
	key     []byte
	users   map[string]int64
}

func newM1App(t *testing.T) *m1App {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL must point to a dedicated test PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal("cannot open test database")
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("m1_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open schema pool")
	}
	t.Cleanup(db.Close)
	if err = migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	app := &m1App{db: db, clock: &testClock{now: time.Now().UTC().Truncate(time.Microsecond)}, key: bytes.Repeat([]byte{7}, 32), users: map[string]int64{}}
	sessions, err := auth.New(db, auth.Options{Origin: testOrigin, Key: app.key, Now: app.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	app.targets, err = connections.New(db, app.key)
	if err != nil {
		t.Fatal(err)
	}
	app.http = httpapi.New(db, sessions.Register, func(mux *http.ServeMux) {
		app.targets.Register(mux, sessions)
		viewer.New(app.targets).Register(mux, sessions)
	})
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"admin", "viewer", "editor", "outsider"} {
		var id int64
		if err = db.QueryRow(ctx, "INSERT INTO users(username,password_hash,is_platform_admin) VALUES($1,$2,$3) RETURNING id", name, hash, name == "admin").Scan(&id); err != nil {
			t.Fatal(err)
		}
		app.users[name] = id
	}
	return app
}

type browser struct {
	app     *m1App
	cookies map[string]*http.Cookie
	csrf    string
}

func (a *m1App) browser() *browser { return &browser{app: a, cookies: map[string]*http.Cookie{}} }

func (b *browser) request(method, path string, body any) *httptest.ResponseRecorder {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(payload))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("X-CSRF-Token", b.csrf)
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.app.http.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}

func status(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status=%d want=%d response=%s", w.Code, want, w.Body.String())
	}
}

func (b *browser) refreshCSRF(t *testing.T) {
	t.Helper()
	w := b.request("GET", "/api/v1/auth/csrf", nil)
	status(t, w, 200)
	var response struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Token == "" {
		t.Fatal("missing CSRF token")
	}
	b.csrf = response.Token
}

func (b *browser) login(t *testing.T, name string) {
	t.Helper()
	b.refreshCSRF(t)
	w := b.request("POST", "/api/v1/auth/login", map[string]string{"username": name, "password": testPassword})
	status(t, w, 200)
	c := b.cookies[auth.CookieName]
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatal("session cookie protections missing")
	}
	if strings.Contains(w.Body.String(), c.Value) || strings.Contains(w.Body.String(), testPassword) {
		t.Fatal("login response leaked a secret")
	}
	b.refreshCSRF(t)
}

func targetInput(t *testing.T, variable, name string) connections.Input {
	t.Helper()
	value := os.Getenv(variable)
	if value == "" {
		t.Fatalf("%s must name a dedicated target test database", variable)
	}
	cfg, err := pgx.ParseConfig(value)
	if err != nil {
		t.Fatal("invalid target test URL")
	}
	return connections.Input{Name: name, Host: cfg.Host, Port: int(cfg.Port), DatabaseName: cfg.Database, Username: cfg.User, Password: &cfg.Password, SSLMode: "disable"}
}

func createTarget(t *testing.T, b *browser, in connections.Input) connections.Connection {
	t.Helper()
	w := b.request("POST", "/api/v1/admin/databases", in)
	status(t, w, 201)
	if strings.Contains(w.Body.String(), "password") {
		t.Fatal("target response exposes password fields")
	}
	var result connections.Connection
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func grant(t *testing.T, a *m1App, user string, target int64, access string) {
	t.Helper()
	ctx := context.Background()
	var group int64
	if err := a.db.QueryRow(ctx, "INSERT INTO groups(name) VALUES($1) RETURNING id", fmt.Sprintf("%s_%d_%s", user, target, access)).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(ctx, "INSERT INTO user_groups(user_id,group_id) VALUES($1,$2)", a.users[user], group); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(ctx, "INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES($1,$2,$3)", group, target, access); err != nil {
		t.Fatal(err)
	}
}

func TestM1ConnectionsAPI(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	status(t, a.browser().request("GET", "/api/v1/admin/databases", nil), 401)
	admin := a.browser()
	admin.login(t, "admin")
	firstInput := targetInput(t, "TEST_TARGET_DATABASE_URL", "first")
	secondInput := targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "second")
	if firstInput.DatabaseName == secondInput.DatabaseName {
		t.Fatal("two distinct target databases required")
	}
	first := createTarget(t, admin, firstInput)
	second := createTarget(t, admin, secondInput)
	firstPath := "/api/v1/admin/databases/" + strconv.FormatInt(first.ID, 10)
	secondPath := "/api/v1/admin/databases/" + strconv.FormatInt(second.ID, 10)
	status(t, admin.request("POST", firstPath+"/test", nil), 200)
	status(t, admin.request("POST", secondPath+"/test", nil), 200)
	metadataInput := targetInput(t, "TEST_DATABASE_URL", "forbidden-metadata")
	metadata := createTarget(t, admin, metadataInput)
	status(t, admin.request("POST", fmt.Sprintf("/api/v1/admin/databases/%d/test", metadata.ID), nil), 409)
	grant(t, a, "outsider", metadata.ID, "edit")
	if pool, err := a.targets.OpenTarget(ctx, auth.Principal{ID: a.users["outsider"]}, metadata.ID, true); err == nil {
		pool.Close()
		t.Fatal("metadata opened as a migration target")
	}
	status(t, admin.request("DELETE", fmt.Sprintf("/api/v1/admin/databases/%d", metadata.ID), nil), 204)
	duplicate := firstInput
	duplicate.Name = "duplicate"
	status(t, admin.request("POST", "/api/v1/admin/databases", duplicate), 409)
	if firstInput.Host == "127.0.0.1" {
		duplicate.Host = "LOCALHOST."
		status(t, admin.request("POST", "/api/v1/admin/databases", duplicate), 409)
	}
	status(t, admin.request("PATCH", firstPath, map[string]any{"database_name": secondInput.DatabaseName}), 409)
	status(t, admin.request("PATCH", firstPath, map[string]any{"unexpected": true}), 400)
	status(t, admin.request("PATCH", "/api/v1/admin/databases/not-an-id", map[string]string{"name": "x"}), 400)
	status(t, admin.request("POST", "/api/v1/admin/databases/999999/test", nil), 404)
	var ciphertext []byte
	if err := a.db.QueryRow(ctx, "SELECT password_encrypted FROM database_connections WHERE id=$1", first.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(*firstInput.Password)) {
		t.Fatal("target password stored as plaintext")
	}
	box, _ := secretbox.New(a.key)
	plain, err := box.Open(ciphertext, fmt.Sprintf("target:%d", first.ID))
	if err != nil || string(plain) != *firstInput.Password {
		t.Fatal("stored credential cannot be decrypted")
	}
	status(t, admin.request("PATCH", firstPath, map[string]string{"name": "renamed", "password": ""}), 200)
	var retained []byte
	if err = a.db.QueryRow(ctx, "SELECT password_encrypted FROM database_connections WHERE id=$1", first.ID).Scan(&retained); err != nil || !bytes.Equal(ciphertext, retained) {
		t.Fatal("empty update replaced credential")
	}
	status(t, admin.request("PATCH", firstPath, map[string]string{"password": "deliberately-wrong-test-password"}), 200)
	w := admin.request("POST", firstPath+"/test", nil)
	status(t, w, 503)
	if strings.Contains(w.Body.String(), "deliberately-wrong") {
		t.Fatal("connection error leaked password")
	}
	status(t, admin.request("PATCH", firstPath, map[string]string{"password": *firstInput.Password}), 200)
	status(t, admin.request("POST", firstPath+"/test", nil), 200)
	unavailable := firstInput
	unavailable.Name = "offline"
	unavailable.Port = 1
	offline := createTarget(t, admin, unavailable)
	status(t, admin.request("POST", fmt.Sprintf("/api/v1/admin/databases/%d/test", offline.ID), nil), 503)
	grant(t, a, "viewer", first.ID, "view")
	grant(t, a, "editor", first.ID, "view")
	grant(t, a, "editor", first.ID, "edit")
	grant(t, a, "editor", second.ID, "view")
	viewer := a.browser()
	viewer.login(t, "viewer")
	editor := a.browser()
	editor.login(t, "editor")
	outsider := a.browser()
	outsider.login(t, "outsider")
	for _, tc := range []struct {
		b           *browser
		count       int
		firstAccess string
	}{{viewer, 1, "view"}, {editor, 2, "edit"}, {outsider, 0, ""}, {admin, 0, ""}} {
		w := tc.b.request("GET", "/api/v1/databases", nil)
		status(t, w, 200)
		var result struct {
			Items []connections.Connection `json:"databases"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Items) != tc.count {
			t.Fatalf("unexpected accessible database count: %d", len(result.Items))
		}
		if tc.count > 0 && string(result.Items[0].Access) != tc.firstAccess {
			t.Fatal("wrong effective access")
		}
	}
	status(t, viewer.request("GET", "/api/v1/admin/databases", nil), 403)
	status(t, viewer.request("PATCH", firstPath, map[string]string{"name": "forbidden"}), 403)
	status(t, editor.request("POST", secondPath+"/test", nil), 403)
	for _, tc := range []struct {
		user    string
		target  int64
		write   bool
		allowed bool
	}{{"viewer", first.ID, false, true}, {"viewer", first.ID, true, false}, {"viewer", second.ID, false, false}, {"editor", first.ID, true, true}, {"outsider", first.ID, false, false}, {"admin", first.ID, false, false}} {
		pool, err := a.targets.OpenTarget(ctx, auth.Principal{ID: a.users[tc.user], IsPlatformAdmin: tc.user == "admin"}, tc.target, tc.write)
		if pool != nil {
			var databaseName string
			if queryErr := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); queryErr != nil || databaseName != firstInput.DatabaseName {
				t.Fatal("returned pool does not reach expected target")
			}
			pool.Close()
		}
		if (err == nil) != tc.allowed {
			t.Fatalf("target policy mismatch for %s", tc.user)
		}
	}
	oldCSRF := admin.csrf
	admin.csrf = ""
	status(t, admin.request("PATCH", firstPath, map[string]string{"name": "forbidden"}), 403)
	admin.csrf = oldCSRF
	if _, err := a.db.Exec(ctx, "INSERT INTO migration_drafts(database_id,title,sql,created_by) VALUES($1,'fixture','SELECT 1',$2)", first.ID, a.users["editor"]); err != nil {
		t.Fatal(err)
	}
	status(t, admin.request("DELETE", firstPath, nil), 409)
	status(t, viewer.request("GET", "/api/v1/auth/me", nil), 200)
	status(t, admin.request("DELETE", secondPath, nil), 204)
	status(t, editor.request("GET", "/api/v1/auth/me", nil), 401)
	status(t, admin.request("DELETE", fmt.Sprintf("/api/v1/admin/databases/%d", offline.ID), nil), 204)
}

func TestM1Sessions(t *testing.T) {
	a := newM1App(t)
	t.Run("login csrf credentials and origin", func(t *testing.T) {
		b := a.browser()
		status(t, b.request("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": testPassword}), 403)
		b.refreshCSRF(t)
		status(t, b.request("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong"}), 401)
		status(t, b.request("POST", "/api/v1/auth/login", map[string]string{"username": "missing", "password": "wrong"}), 401)
		r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://attacker.invalid")
		r.Header.Set("X-CSRF-Token", b.csrf)
		for _, c := range b.cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		a.http.ServeHTTP(w, r)
		status(t, w, 403)
	})
	t.Run("rotation grace recovery and logout", func(t *testing.T) {
		b := a.browser()
		b.login(t, "admin")
		old := *b.cookies[auth.CookieName]
		expiry := old.Expires
		a.clock.Advance(auth.RotationInterval)
		status(t, b.request("GET", "/api/v1/auth/me", nil), 200)
		current := *b.cookies[auth.CookieName]
		if current.Value == old.Value || !current.Expires.Equal(expiry) {
			t.Fatal("rotation failed or extended absolute expiry")
		}
		lost := a.browser()
		lost.cookies[auth.CookieName] = &old
		status(t, lost.request("GET", "/api/v1/auth/me", nil), 200)
		if lost.cookies[auth.CookieName].Value != current.Value {
			t.Fatal("grace request did not recover current token")
		}
		a.clock.Advance(auth.GracePeriod + time.Second)
		stale := a.browser()
		stale.cookies[auth.CookieName] = &old
		status(t, stale.request("GET", "/api/v1/auth/me", nil), 401)
		status(t, b.request("POST", "/api/v1/auth/logout", nil), 204)
		replay := a.browser()
		replay.cookies[auth.CookieName] = &current
		status(t, replay.request("GET", "/api/v1/auth/me", nil), 401)
	})
	t.Run("idle expiry", func(t *testing.T) {
		b := a.browser()
		b.login(t, "viewer")
		a.clock.Advance(auth.IdleTimeout)
		status(t, b.request("GET", "/api/v1/auth/me", nil), 401)
	})
	t.Run("absolute expiry while active", func(t *testing.T) {
		b := a.browser()
		b.login(t, "viewer")
		for range 16 {
			a.clock.Advance(29 * time.Minute)
			status(t, b.request("GET", "/api/v1/auth/me", nil), 200)
		}
		a.clock.Advance(16 * time.Minute)
		status(t, b.request("GET", "/api/v1/auth/me", nil), 401)
	})
	t.Run("access increase and revocation", func(t *testing.T) {
		b := a.browser()
		b.login(t, "editor")
		old := *b.cookies[auth.CookieName]
		tx, err := a.db.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err = auth.ChangeUserAccess(context.Background(), tx, a.users["editor"], true); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		status(t, b.request("GET", "/api/v1/auth/me", nil), 200)
		if old.Value == b.cookies[auth.CookieName].Value {
			t.Fatal("privilege increase did not rotate")
		}
		tx, err = a.db.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err = auth.ChangeUserAccess(context.Background(), tx, a.users["editor"], false); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		status(t, b.request("GET", "/api/v1/auth/me", nil), 401)
		replay := a.browser()
		replay.cookies[auth.CookieName] = &old
		status(t, replay.request("GET", "/api/v1/auth/me", nil), 401)
	})
	t.Run("concurrent rotation", func(t *testing.T) {
		b := a.browser()
		b.login(t, "admin")
		old := *b.cookies[auth.CookieName]
		a.clock.Advance(auth.RotationInterval)
		results := make(chan *httptest.ResponseRecorder, 2)
		for range 2 {
			go func() {
				r := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
				r.AddCookie(&old)
				w := httptest.NewRecorder()
				a.http.ServeHTTP(w, r)
				results <- w
			}()
		}
		var current string
		for range 2 {
			w := <-results
			status(t, w, 200)
			for _, c := range w.Result().Cookies() {
				if c.Name == auth.CookieName {
					if current != "" && current != c.Value {
						t.Fatal("concurrent requests rotated twice")
					}
					current = c.Value
				}
			}
		}
		if current == "" || current == old.Value {
			t.Fatal("concurrent rotation did not issue a new token")
		}
	})
}
