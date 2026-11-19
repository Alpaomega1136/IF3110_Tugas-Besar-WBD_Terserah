//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/database"
	"terserah/backend/internal/httpapi"
	"terserah/backend/internal/runner"
	"terserah/backend/migrations"
)

func TestDraftListingRevisionContract(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "draft-list"))
	other := createTarget(t, admin, targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "other-draft-list"))
	grant(t, a, "editor", target.ID, "edit")
	grant(t, a, "editor", other.ID, "edit")
	grant(t, a, "viewer", target.ID, "view")
	editor := a.browser()
	editor.login(t, "editor")
	view := a.browser()
	view.login(t, "viewer")
	base := fmt.Sprintf("/api/v1/databases/%d/drafts", target.ID)
	status(t, a.browser().request("GET", base, nil), 401)
	status(t, view.request("GET", base, nil), 403)
	status(t, admin.request("GET", base, nil), 403)
	var page struct {
		Drafts []runner.Draft `json:"drafts"`
		More   bool           `json:"has_more"`
		Next   *string        `json:"next_after"`
	}
	w := editor.request("GET", base, nil)
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Drafts) != 0 || page.More || page.Next != nil {
		t.Fatal("empty draft listing contract changed")
	}
	var created []runner.Draft
	for i := 0; i < 2; i++ {
		w = editor.request("POST", base, map[string]string{"title": fmt.Sprintf("draft-%d", i), "sql": "SELECT 1"})
		status(t, w, 201)
		var d runner.Draft
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		created = append(created, d)
	}
	w = editor.request("GET", base+"?limit=1", nil)
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Drafts) != 1 || !page.More || page.Next == nil {
		t.Fatal("draft pagination missing")
	}
	first := page.Drafts[0]
	if first.ID != created[0].ID || first.Revision != 1 || first.CreatedBy != a.users["editor"] || first.CreatedByUsername != "editor" || first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() || first.SQL != "SELECT 1" {
		t.Fatalf("draft metadata=%+v", first)
	}
	w = editor.request("GET", base+"?after="+*page.Next, nil)
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Drafts) != 1 || page.Drafts[0].ID != created[1].ID || page.More {
		t.Fatal("draft next page changed")
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?after=0", "?after=bad"} {
		status(t, editor.request("GET", base+query, nil), 400)
	}
	path := fmt.Sprintf("%s/%d", base, first.ID)
	w = editor.request("PATCH", path, map[string]string{"title": "changed", "sql": "SELECT 2", "revision": "1"})
	status(t, w, 200)
	var updated runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil || updated.Revision != 2 || updated.CreatedBy != first.CreatedBy || !updated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("revision or creator changed incorrectly")
	}
	status(t, editor.request("PATCH", path, map[string]string{"title": "stale", "sql": "SELECT 3", "revision": "1"}), 409)
	status(t, editor.request("POST", path+"/execute", map[string]string{"revision": "1"}), 409)
	approveDraft(t, a, editor, target.ID, first.ID, 2)
	w = editor.executeAndWait(t, path+"/execute", map[string]string{"revision": "2"})
	status(t, w, 200)
	var result runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Status != "success" {
		t.Fatal("listed draft cannot execute")
	}
	w = editor.request("PATCH", path, map[string]string{"title": "new revision", "sql": "SELECT 4", "revision": "2"})
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil || updated.Revision != 3 {
		t.Fatal("executed draft cannot advance to a new revision")
	}
	w = editor.request("GET", fmt.Sprintf("/api/v1/databases/%d/drafts", other.ID), nil)
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Drafts) != 0 {
		t.Fatal("drafts leaked across databases")
	}
}

func TestLoginRejectsPasswordChangedWhileWaiting(t *testing.T) {
	a := newM1App(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b := a.browser()
	b.refreshCSRF(t)
	newPassword := "changed-password-123"
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var id int64
	if err = tx.QueryRow(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", a.users["editor"]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var blocker int
	if err = tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- b.request("POST", "/api/v1/auth/login", map[string]string{"username": "editor", "password": testPassword})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = a.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("login did not reach account recheck")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err = tx.Exec(ctx, "UPDATE users SET password_hash=$1 WHERE id=$2", newHash, id); err != nil {
		t.Fatal(err)
	}
	if err = auth.ChangeUserAccess(ctx, tx, id, false); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	status(t, <-done, 401)
	b.refreshCSRF(t)
	status(t, b.request("POST", "/api/v1/auth/login", map[string]string{"username": "editor", "password": newPassword}), 200)
	status(t, b.request("GET", "/api/v1/auth/me", nil), 200)
}

func TestBE1MetadataUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("explicit fixture database required")
	}
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal("cannot open upgrade fixture")
	}
	defer admin.Close()
	schema := fmt.Sprintf("be1_upgrade_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid fixture URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	initial, err := os.ReadFile("../../migrations/000001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `CREATE TABLE platform_schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now()); INSERT INTO platform_schema_migrations(version) VALUES('000001_initial.sql')`); err != nil {
		t.Fatal(err)
	}
	// Use the verified BE-01 hash fixture, including its original short password.
	const verifiedLegacy = "$pbkdf2-sha256$i=210000$AAECAwQFBgcICQoLDA0ODw$2QNDrl+R+uQV/xJCW4gZazJPobMhzqlqdv5qX/CgJFY"
	var userID, dbID, draftID int64
	if err = db.QueryRow(ctx, "INSERT INTO users(username,password_hash,is_platform_admin) VALUES('existing', $1,true) RETURNING id", verifiedLegacy).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "INSERT INTO database_connections(name,host,port,database_name,username,password_encrypted,sslmode) VALUES('existing','localhost',5432,'existing','existing','x','disable') RETURNING id").Scan(&dbID); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, "INSERT INTO migration_drafts(database_id,title,sql,created_by) VALUES($1,'existing','SELECT 1',$2) RETURNING id", dbID, userID).Scan(&draftID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO migration_history(database_id,draft_id,version,sql,executed_by,status) VALUES($1,$2,1,'SELECT 1',$3,'success')", dbID, draftID, userID); err != nil {
		t.Fatal(err)
	}
	var oldSession int64
	expires := time.Now().Add(time.Hour)
	if err = db.QueryRow(ctx, "INSERT INTO sessions(user_id,csrf_token_hash,absolute_expires_at) VALUES($1,$2,$3) RETURNING id", userID, bytes.Repeat([]byte{1}, 32), expires).Scan(&oldSession); err != nil {
		t.Fatal(err)
	}
	oldToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	oldHash := sha256.Sum256([]byte(oldToken))
	if _, err = db.Exec(ctx, "INSERT INTO session_tokens(token_hash,session_id,valid_until) VALUES($1,$2,$3)", oldHash[:], oldSession, expires); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, db); err != nil {
		t.Fatal("upgrade not idempotent", err)
	}
	var hash, sql, historyStatus string
	var revision int64
	var format int
	if err = db.QueryRow(ctx, "SELECT password_hash FROM users WHERE id=$1", userID).Scan(&hash); err != nil || hash != verifiedLegacy || !auth.CheckPassword(hash, "old-pass") {
		t.Fatal("legacy password was reset or is unreadable")
	}
	if err = db.QueryRow(ctx, "SELECT sql,revision FROM migration_drafts WHERE id=$1", draftID).Scan(&sql, &revision); err != nil || sql != "SELECT 1" || revision != 1 {
		t.Fatal("legacy draft lost during upgrade")
	}
	if err = db.QueryRow(ctx, "SELECT status,receipt_version FROM migration_history WHERE database_id=$1 AND version=1", dbID).Scan(&historyStatus, &format); err != nil || historyStatus != "success" || format != 0 {
		t.Fatal("legacy history changed during upgrade")
	}
	var revoked bool
	if err = db.QueryRow(ctx, "SELECT revoked_at IS NOT NULL FROM sessions WHERE id=$1", oldSession).Scan(&revoked); err != nil || !revoked {
		t.Fatal("legacy sessions were not invalidated")
	}
	sessions, err := auth.New(db, auth.Options{Origin: testOrigin, Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	app := &m1App{http: httpapi.New(db, sessions.Register)}
	b := app.browser()
	b.cookies[auth.CookieName] = &http.Cookie{Name: auth.CookieName, Value: oldToken}
	status(t, b.request("GET", "/api/v1/auth/me", nil), 401)
	b.refreshCSRF(t)
	status(t, b.request("POST", "/api/v1/auth/login", map[string]string{"username": "existing", "password": "old-pass"}), 200)
	status(t, b.request("GET", "/api/v1/auth/me", nil), 200)

}
