//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/database"
	"terserah/backend/internal/httpapi"
	"terserah/backend/migrations"
)

type controllableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *controllableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *controllableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openIsolatedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL must point to a dedicated test PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminPool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal("cannot connect to test PostgreSQL")
	}
	t.Cleanup(adminPool.Close)

	schema := fmt.Sprintf("test_be1_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open test pool")
	}
	t.Cleanup(pool.Close)

	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestBE1EndToEndIntegration(t *testing.T) {
	pool := openIsolatedTestPool(t)
	ctx := context.Background()
	clock := &controllableClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}

	authCfg := auth.Config{
		IdleTimeout:      30 * time.Minute,
		AbsoluteLifetime: 2 * time.Hour,
		RotationInterval: 15 * time.Minute,
		GracePeriod:      30 * time.Second,
		Now:              clock.Now,
	}
	authSvc := auth.NewService(auth.NewPostgresStore(pool), authCfg)

	// Bootstrap Platform Admin & verifikasi hash password di PostgreSQL
	adminUser, err := authSvc.BootstrapAdmin(ctx, "platform_admin", "AdminSecretPass123")
	if err != nil {
		t.Fatalf("bootstrap admin failed: %v", err)
	}
	var storedHash string
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, adminUser.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(storedHash, "$pbkdf2-sha256$") || strings.Contains(storedHash, "AdminSecretPass123") {
		t.Fatalf("password was not properly hashed in database: %s", storedHash)
	}

	router := httpapi.NewWithOptions(pool, httpapi.Options{AuthConfig: authCfg})

	doReq := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf != "" {
			r.Header.Set(auth.CSRFHeader, csrf)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, r)
		return rr
	}

	login := func(username, password string) (*http.Cookie, string) {
		rr := doReq("POST", "/api/v1/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, username, password), nil, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("login(%s) status=%d body=%s", username, rr.Code, rr.Body.String())
		}
		ck := rr.Result().Cookies()[0]
		csrfRR := doReq("GET", "/api/v1/auth/csrf", "", ck, "")
		if csrfRR.Code != http.StatusOK {
			t.Fatalf("csrf(%s) status=%d", username, csrfRR.Code)
		}
		var out struct {
			CSRFToken string `json:"csrf_token"`
		}
		if err := json.NewDecoder(csrfRR.Body).Decode(&out); err != nil || out.CSRFToken == "" {
			t.Fatalf("decode csrf: %v", err)
		}
		return ck, out.CSRFToken
	}

	// Rotasi session, grace period, dan konkurensi di PostgreSQL
	adminCookie, adminCSRF := login("platform_admin", "AdminSecretPass123")

	clock.Advance(15 * time.Minute)
	const concurrentReqs = 8
	var wg sync.WaitGroup
	wg.Add(concurrentReqs)
	newCookies := make(chan *http.Cookie, concurrentReqs)
	errs := make(chan error, concurrentReqs)
	for range concurrentReqs {
		go func() {
			defer wg.Done()
			rr := doReq("GET", "/api/v1/auth/me", "", adminCookie, "")
			if rr.Code != http.StatusOK {
				errs <- fmt.Errorf("concurrent /me status=%d", rr.Code)
				return
			}
			for _, c := range rr.Result().Cookies() {
				if c.Name == auth.CookieName && c.Value != "" {
					newCookies <- c
				}
			}
		}()
	}
	wg.Wait()
	close(newCookies)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	var rotatedAdminCookie *http.Cookie
	rotations := 0
	for c := range newCookies {
		rotations++
		rotatedAdminCookie = c
	}
	if rotations != 1 {
		t.Fatalf("expected exactly 1 rotation in PostgreSQL under concurrent load, got %d", rotations)
	}

	// Token lama masih berlaku selama grace period 30 detik
	clock.Advance(10 * time.Second)
	if rr := doReq("GET", "/api/v1/auth/me", "", adminCookie, ""); rr.Code != http.StatusOK {
		t.Fatalf("grace period token failed: %d", rr.Code)
	}
	// Token lama kedaluwarsa setelah grace period berakhir
	clock.Advance(25 * time.Second)
	if rr := doReq("GET", "/api/v1/auth/me", "", adminCookie, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired grace token accepted: %d", rr.Code)
	}
	adminCookie = rotatedAdminCookie

	// Buat user, group, data database target, dan grant izin
	for _, u := range []struct {
		username, password string
	}{
		{"editor_bob", "BobPassword123"},
		{"viewer_carol", "CarolPassword123"},
	} {
		rr := doReq("POST", "/api/v1/admin/users", fmt.Sprintf(`{"username":%q,"password":%q,"is_platform_admin":false}`, u.username, u.password), adminCookie, adminCSRF)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create user %s failed: %d %s", u.username, rr.Code, rr.Body.String())
		}
	}

	for _, g := range []string{"analysts", "developers"} {
		rr := doReq("POST", "/api/v1/admin/groups", fmt.Sprintf(`{"name":%q}`, g), adminCookie, adminCSRF)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create group %s failed: %d %s", g, rr.Code, rr.Body.String())
		}
	}

	// Baris metadata database target (dikelola oleh BE 2 pada sistem utama)
	var db1ID, db2ID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO database_connections (name, host, port, database_name, username, password_encrypted, sslmode)
		VALUES ('orders-db', 'localhost', 5432, 'orders', 'dbuser', $1, 'disable')
		RETURNING id
	`, []byte("encrypted-dummy-1")).Scan(&db1ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO database_connections (name, host, port, database_name, username, password_encrypted, sslmode)
		VALUES ('billing-db', 'localhost', 5432, 'billing', 'dbuser', $1, 'disable')
		RETURNING id
	`, []byte("encrypted-dummy-2")).Scan(&db2ID); err != nil {
		t.Fatal(err)
	}

	// analysts: view pada db1 & db2; developers: edit pada db1
	for _, gr := range []struct {
		gid, dbid int64
		level     string
	}{
		{1, db1ID, "view"},
		{1, db2ID, "view"},
		{2, db1ID, "edit"},
	} {
		rr := doReq("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/databases/%d", gr.gid, gr.dbid), fmt.Sprintf(`{"access_level":%q}`, gr.level), adminCookie, adminCSRF)
		if rr.Code != http.StatusOK {
			t.Fatalf("grant failed: %d %s", rr.Code, rr.Body.String())
		}
	}

	bobCookie, bobCSRF := login("editor_bob", "BobPassword123")
	carolCookie, carolCSRF := login("viewer_carol", "CarolPassword123")

	// Daftarkan bob ke analysts & developers, carol ke analysts
	for _, m := range [][2]int64{{1, 2}, {2, 2}, {1, 3}} {
		rr := doReq("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/users/%d", m[0], m[1]), "", adminCookie, adminCSRF)
		if rr.Code != http.StatusOK {
			t.Fatalf("add membership %v failed: %d %s", m, rr.Code, rr.Body.String())
		}
	}

	// Peningkatan privilege memicu rotasi cookie session pada request berikutnya
	rr := doReq("GET", "/api/v1/databases", "", bobCookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("bob GET /databases failed: %d", rr.Code)
	}
	if len(rr.Result().Cookies()) != 1 || rr.Result().Cookies()[0].Value == bobCookie.Value {
		t.Fatal("expected bob's session token to rotate after privilege elevation")
	}
	bobCookie = rr.Result().Cookies()[0]

	// Carol juga mengalami rotasi cookie session
	rr = doReq("GET", "/api/v1/databases", "", carolCookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("carol GET /databases failed: %d", rr.Code)
	}
	if cks := rr.Result().Cookies(); len(cks) == 1 && cks[0].Value != "" {
		carolCookie = cks[0]
	}

	// Platform Admin tanpa membership group ditolak pada endpoint database target
	if rr := doReq("GET", fmt.Sprintf("/api/v1/databases/%d/migrations", db1ID), "", adminCookie, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for ungranted platform admin on target db, got %d", rr.Code)
	}

	// Operasi draft oleh bob (Edit), ditolak untuk carol (View Only)
	if rr := doReq("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", db1ID), `{"title":"unauthorized","sql":"SELECT 1;"}`, carolCookie, carolCSRF); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for view-only carol creating draft, got %d", rr.Code)
	}
	if rr := doReq("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", db2ID), `{"title":"unauthorized","sql":"SELECT 1;"}`, bobCookie, bobCSRF); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for bob creating draft on view-only db2, got %d", rr.Code)
	}

	rr = doReq("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", db1ID), `{"title":"create orders index","sql":"CREATE INDEX orders_customer_idx ON orders(customer_id);","user_id":"1"}`, bobCookie, bobCSRF)
	if rr.Code != http.StatusCreated {
		t.Fatalf("bob create draft failed: %d %s", rr.Code, rr.Body.String())
	}

	rr = doReq("PATCH", fmt.Sprintf("/api/v1/databases/%d/drafts/1", db1ID), `{"title":"create orders customer index"}`, bobCookie, bobCSRF)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob update draft failed: %d %s", rr.Code, rr.Body.String())
	}

	// Catat riwayat migration yang telah dieksekusi
	if _, err := pool.Exec(ctx, `
		INSERT INTO migration_history (database_id, draft_id, version, sql, executed_by, started_at, finished_at, status)
		VALUES ($1, 1, 1, 'CREATE INDEX orders_customer_idx ON orders(customer_id);', 2, $2, $2, 'success')
	`, db1ID, clock.Now()); err != nil {
		t.Fatal(err)
	}

	// Draft yang sudah dieksekusi tidak dapat diubah lagi
	if rr := doReq("PATCH", fmt.Sprintf("/api/v1/databases/%d/drafts/1", db1ID), `{"sql":"DROP TABLE orders;"}`, bobCookie, bobCSRF); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 updating executed draft, got %d", rr.Code)
	}

	// Carol (View Only) dapat melihat riwayat dan detail migration
	rr = doReq("GET", fmt.Sprintf("/api/v1/databases/%d/migrations", db1ID), "", carolCookie, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"executed_by_username":"editor_bob"`) {
		t.Fatalf("carol list history failed: %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq("GET", fmt.Sprintf("/api/v1/databases/%d/migrations/1", db1ID), "", carolCookie, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"success"`) {
		t.Fatalf("carol get history detail failed: %d %s", rr.Code, rr.Body.String())
	}

	// Pencabutan keanggotaan group langsung membatalkan session pengguna
	rr = doReq("DELETE", "/api/v1/admin/groups/1/users/3", "", adminCookie, adminCSRF)
	if rr.Code != http.StatusOK {
		t.Fatalf("remove carol from group failed: %d", rr.Code)
	}
	if rr := doReq("GET", "/api/v1/auth/me", "", carolCookie, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected carol's session to be revoked after group removal, got %d", rr.Code)
	}
}
