//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/database"
	"terserah/backend/internal/runner"
)

func TestLastActiveAdmin(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	path := fmt.Sprintf("/api/v1/admin/users/%d", a.users["admin"])
	for _, patch := range []map[string]bool{{"is_active": false}, {"is_platform_admin": false}} {
		w := admin.request("PATCH", path, patch)
		status(t, w, 409)
		if !strings.Contains(w.Body.String(), "last_active_admin") {
			t.Fatal("missing last-admin conflict code")
		}
		status(t, admin.request("GET", "/api/v1/auth/me", nil), 200)
	}
	var out struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	w := admin.request("POST", "/api/v1/admin/users", map[string]any{"username": "other-admin", "password": testPassword, "is_platform_admin": true})
	status(t, w, 201)
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.User.ID == "" {
		t.Fatal("missing second admin")
	}
	other := a.browser()
	other.login(t, "other-admin")
	browsers := []*browser{admin, other}
	paths := []string{path, "/api/v1/admin/users/" + out.User.ID}
	codes := make([]int, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, b := range browsers {
		wg.Add(1)
		go func(i int, b *browser) {
			defer wg.Done()
			<-start
			codes[i] = b.request("PATCH", paths[i], map[string]bool{"is_active": false}).Code
		}(i, b)
	}
	close(start)
	wg.Wait()
	if !((codes[0] == 200 && codes[1] == 409) || (codes[0] == 409 && codes[1] == 200)) {
		t.Fatalf("concurrent admin deactivation statuses=%v", codes)
	}
	var active int
	if err := a.db.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE is_platform_admin AND is_active").Scan(&active); err != nil || active != 1 {
		t.Fatal("last active admin lost", err)
	}
	for i, b := range browsers {
		want := 200
		if codes[i] == 200 {
			want = 401
		}
		status(t, b.request("GET", "/api/v1/auth/me", nil), want)
	}
}

func TestRecoveryIgnoresTargetSearchPath(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "path-target")
	target := createTarget(t, admin, input)
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	// The executor owns the application schema and can restore its own role settings.
	executor, err := database.Open(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal("cannot open fixture executor")
	}
	defer executor.Close()
	role := pgx.Identifier{input.Username}.Sanitize()
	var settings []string
	if err = executor.QueryRow(ctx, `SELECT COALESCE((SELECT setconfig FROM pg_catalog.pg_db_role_setting WHERE setrole=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=$1) AND setdatabase=0),ARRAY[]::text[])`, input.Username).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	var original *string
	for _, setting := range settings {
		if strings.HasPrefix(setting, "search_path=") {
			value := strings.TrimPrefix(setting, "search_path=")
			original = &value
		}
	}
	schema := fmt.Sprintf("path_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	defer func() {
		if original == nil {
			_, err = executor.Exec(ctx, "ALTER ROLE "+role+" RESET search_path")
		} else {
			var command string
			err = executor.QueryRow(ctx, `SELECT pg_catalog.format('ALTER ROLE %I SET search_path = %L',$1::text,$2::text)`, input.Username, *original).Scan(&command)
			if err == nil {
				_, err = executor.Exec(ctx, command)
			}
		}
		if err != nil {
			t.Error("cannot restore fixture search_path", err)
		}
		if _, err = executor.Exec(ctx, "DROP SCHEMA IF EXISTS "+q+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	sql := fmt.Sprintf(`CREATE SCHEMA %[1]s;
 CREATE TABLE %[1]s.items(id int PRIMARY KEY); INSERT INTO %[1]s.items VALUES(1);
 CREATE FUNCTION %[1]s.never_equal(text,text) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT false $$;
 CREATE OPERATOR %[1]s.= (FUNCTION=%[1]s.never_equal,LEFTARG=text,RIGHTARG=text);
 CREATE FUNCTION %[1]s.set_path() RETURNS void LANGUAGE plpgsql AS $body$ BEGIN EXECUTE format('ALTER ROLE %%I SET search_path = %[1]s, pg_catalog',current_user); END; $body$;
 SELECT %[1]s.set_path(); SELECT pg_catalog.set_config('search_path','%[1]s,pg_catalog',true)`, schema)
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	createExecute := func(sql, sqlDown string) (runner.Draft, runner.Result) {
		t.Helper()
		w := editor.request("POST", base+"/drafts", map[string]string{"title": "path regression", "sql": sql, "sql_down": sqlDown})
		status(t, w, 201)
		var d runner.Draft
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		approveDraft(t, a, editor, target.ID, d.ID, 1)
		w = editor.request("POST", fmt.Sprintf("%s/drafts/%d/execute", base, d.ID), map[string]string{"revision": "1"})
		status(t, w, 200)
		var out runner.Result
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Status != "success" {
			t.Fatalf("migration status=%s error=%v", out.Status, err)
		}
		return d, out
	}
	d, first := createExecute(sql, "DROP SCHEMA "+schema+" CASCADE")
	// A global pg_catalog-first override would break unqualified application DDL.
	_, second := createExecute("CREATE TABLE unqualified(id int); INSERT INTO unqualified VALUES(2)", "DROP TABLE IF EXISTS unqualified")
	if second.Version != first.Version+1 {
		t.Fatal("version sequence changed")
	}
	var count int
	if err = executor.QueryRow(ctx, "SELECT count(*) FROM "+q+".unqualified").Scan(&count); err != nil || count != 1 {
		t.Fatal("application search_path was not restored", err)
	}
	status(t, editor.request("GET", base+"/schemas/"+schema+"/tables/items/rows", nil), 200)
	if err = executor.QueryRow(ctx, "SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1", first.ExecutionID).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing real receipt", err)
	}
	if _, err = a.db.Exec(ctx, "UPDATE migration_history SET status='unknown',finished_at=NULL WHERE execution_id=$1", first.ExecutionID); err != nil {
		t.Fatal(err)
	}
	w := editor.request("POST", base+"/migrations/reconcile", nil)
	status(t, w, 200)
	var results struct {
		Items []runner.Result `json:"reconciled"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &results); err != nil || len(results.Items) != 1 || results.Items[0].Status != "success" || results.Items[0].ExecutionID != first.ExecutionID {
		t.Fatalf("real committed migration was not recovered: %s", w.Body.String())
	}
	w = editor.request("POST", fmt.Sprintf("%s/drafts/%d/execute", base, d.ID), map[string]string{"revision": "1"})
	status(t, w, 200)
	var replay runner.Result
	if err = json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.ExecutionID != first.ExecutionID || replay.Status != "success" {
		t.Fatal("recovery replay changed attempt")
	}
}
