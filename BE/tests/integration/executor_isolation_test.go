//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/runner"
)

func TestExecutorIsolation(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "isolated-a")
	second := targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "isolated-b")
	if input.Username == second.Username {
		t.Fatal("target fixtures must use different dedicated executor roles")
	}
	target := createTarget(t, admin, input)
	other := createTarget(t, admin, second)
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	status(t, editor.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", other.ID), nil), 403)
	base := fmt.Sprintf("/api/v1/databases/%d/drafts", target.ID)
	execute := func(sql string, want int) runner.Result {
		t.Helper()
		w := editor.request("POST", base, map[string]string{"title": "isolation", "sql": sql})
		status(t, w, 201)
		var d runner.Draft
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		approveDraft(t, a, editor, target.ID, d.ID, 1)
		w = editor.request("POST", fmt.Sprintf("%s/%d/execute", base, d.ID), map[string]string{"revision": "1"})
		status(t, w, want)
		var result runner.Result
		if want == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		} else if !strings.Contains(w.Body.String(), "target_history_unprotected") {
			t.Fatal("unsafe executor was not diagnosed")
		}
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connA, err := pgx.Connect(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close(context.Background())
	connB, err := pgx.Connect(ctx, os.Getenv("TEST_SECOND_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close(context.Background())
	pid := connB.PgConn().PID()
	// The other executor's query text is hidden even while its connection exists.
	if _, err = connB.Exec(ctx, "SELECT 'isolation-private-marker'"); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("isolation_%d", time.Now().UnixNano())
	defer func() {
		_, _ = connA.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}()
	result := execute(fmt.Sprintf(`CREATE SCHEMA %s; CREATE TABLE %s.probe AS SELECT query FROM pg_catalog.pg_stat_activity WHERE pid=%d AND query LIKE '%%isolation-private-marker%%'`, schema, schema, pid), 200)
	if result.Status != "success" {
		t.Fatal("dedicated executor could not run normal SQL")
	}
	var count int
	if err = connA.QueryRow(ctx, "SELECT count(*) FROM "+schema+".probe").Scan(&count); err != nil || count != 0 {
		t.Fatal("query text from the other target was exposed")
	}
	result = execute(fmt.Sprintf("SELECT pg_catalog.pg_cancel_backend(%d)", pid), 200)
	if result.Status != "failed" || result.ErrorCode == nil || *result.ErrorCode != "42501" {
		t.Fatal("executor can signal another target's session")
	}
	if err = connB.Ping(ctx); err != nil {
		t.Fatal("other target session was interrupted")
	}

	// An accidental direct grant in B makes executor A unsafe. Fail before SQL.
	ownerCfg, err := pgx.ParseConfig(os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	ownerCfg.Database = second.DatabaseName
	owner, err := pgx.ConnectConfig(ctx, ownerCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	role := pgx.Identifier{input.Username}.Sanitize()
	if _, err = owner.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+role); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = owner.Exec(context.Background(), "REVOKE USAGE ON SCHEMA public FROM "+role) }()
	execute("SELECT 1", 409)
	status(t, editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", target.ID), nil), 409)
}
