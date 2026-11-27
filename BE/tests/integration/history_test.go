//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"terserah/backend/internal/database"
	"terserah/backend/internal/runner"
)

func TestReconcileReachesAttemptsBeyondFirstHundred(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	// The first hundred predate signed receipts and must remain unknown.
	_, err := f.a.db.Exec(ctx, `
		INSERT INTO migration_history (database_id, version, sql, executed_by, status, execution_id, receipt_version)
		SELECT $1, n, 'SELECT 1', $2, 'unknown',
		       CASE WHEN n = 101 THEN 'reconcile-no-receipt' END,
		       CASE WHEN n = 101 THEN 1 ELSE 0 END
		FROM generate_series(1, 101) AS n`, f.first, f.actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first)
	for i := 0; i < 2; i++ {
		w := f.editor.request("POST", path, nil)
		status(t, w, 200)
		var response struct {
			Results []runner.Result `json:"reconciled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Results) != 100 {
			t.Fatalf("request %d returned %d results, err=%v", i+1, len(response.Results), err)
		}
	}
	var legacyUnknown, verifiedRollback int
	err = f.a.db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE version <= 100 AND status = 'unknown' AND error_code = 'legacy_receipt_unverified'),
		       count(*) FILTER (WHERE version = 101 AND status = 'failed' AND error_code = 'reconciled_rollback')
		FROM migration_history WHERE database_id = $1`, f.first).Scan(&legacyUnknown, &verifiedRollback)
	if err != nil || legacyUnknown != 100 || verifiedRollback != 1 {
		t.Fatalf("legacy unknown=%d, verified rollback=%d, err=%v", legacyUnknown, verifiedRollback, err)
	}
}

func TestReconcileSavesProgressBeforeDeadline(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	const attempts = 40
	_, err := f.a.db.Exec(ctx, `
		INSERT INTO migration_history (database_id, version, sql, executed_by, status, execution_id, receipt_version)
		SELECT $1, n, 'SELECT 1', $2, 'unknown', 'slow-reconcile-' || n, 1
		FROM generate_series(1, $3::integer) AS n`, f.first, f.actor.ID, attempts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.a.db.Exec(ctx, `
		CREATE FUNCTION test_slow_reconcile() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(0.10);
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER test_slow_reconcile BEFORE UPDATE OF status ON migration_history
		FOR EACH ROW EXECUTE FUNCTION test_slow_reconcile();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.a.db.Exec(ctx, "DROP FUNCTION IF EXISTS test_slow_reconcile() CASCADE"); err != nil {
			t.Error(err)
		}
	})

	path := fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first)
	limited, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	w := f.editor.requestContext(limited, "POST", path, nil)
	status(t, w, 200)

	var completed int
	err = f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_history
		WHERE database_id = $1 AND status = 'failed' AND error_code = 'reconciled_rollback'`, f.first).Scan(&completed)
	if err != nil || completed == 0 || completed >= attempts {
		t.Fatalf("expected a committed prefix, got %d of %d attempts, err=%v", completed, attempts, err)
	}

	if _, err = f.a.db.Exec(ctx, "DROP TRIGGER test_slow_reconcile ON migration_history"); err != nil {
		t.Fatal(err)
	}
	w = f.editor.request("POST", path, nil)
	status(t, w, 200)
	var response struct {
		Results []runner.Result `json:"reconciled"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Results) != attempts-completed {
		t.Fatalf("expected remaining %d attempts, got %d, err=%v", attempts-completed, len(response.Results), err)
	}
}

func TestHistoryAndReconciliation(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "history"))
	other := createTarget(t, admin, targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "other-history"))
	grant(t, a, "editor", target.ID, "edit")
	grant(t, a, "viewer", target.ID, "view")
	grant(t, a, "viewer", other.ID, "view")
	editor := a.browser()
	editor.login(t, "editor")
	view := a.browser()
	view.login(t, "viewer")
	base := fmt.Sprintf("/api/v1/databases/%d/migrations", target.ID)
	status(t, a.browser().request("GET", base, nil), 401)
	status(t, admin.request("GET", base, nil), 403)
	status(t, view.request("POST", base+"/reconcile", nil), 403)
	status(t, view.request("GET", base+"?limit=0", nil), 400)
	status(t, view.request("GET", base+"?before=-1", nil), 400)
	status(t, view.request("GET", base+"/invalid", nil), 400)
	status(t, view.request("GET", base+"/1", nil), 404)
	create := func(sql string) runner.Draft {
		t.Helper()
		w := editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", target.ID), map[string]string{"title": "history", "sql": sql, "sql_down": "SELECT 1"})
		status(t, w, 201)
		var d runner.Draft
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	execute := func(d runner.Draft) runner.Result {
		t.Helper()
		approveDraft(t, a, editor, target.ID, d.ID, 1)
		w := editor.executeAndWait(t, fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", target.ID, d.ID), map[string]string{"revision": "1"})
		status(t, w, 200)
		var out runner.Result
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	d := create("SELECT 'history; snapshot'")
	first := execute(d)
	failed := execute(create("SELECT 1/0"))
	if first.Status != "success" || failed.Status != "failed" || first.Version != 1 || failed.Version != 2 {
		t.Fatal("nonlinear attempts")
	}
	w := view.request("GET", base+"?limit=1", nil)
	status(t, w, 200)
	var page struct {
		Migrations []runner.History `json:"migrations"`
		Next       *string          `json:"next_before"`
		More       bool             `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Migrations) != 1 || page.Migrations[0].Version != 2 || !page.More || page.Next == nil || *page.Next != "2" || strings.Contains(w.Body.String(), `"sql"`) {
		t.Fatalf("bad page: %s", w.Body.String())
	}
	w = view.request("GET", base+"?before=2", nil)
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Migrations) != 1 || page.More || page.Migrations[0].Version != 1 {
		t.Fatal("bad next page")
	}
	w = view.request("GET", base+"/1", nil)
	status(t, w, 200)
	var detail runner.History
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.SQL != d.SQL || detail.ExecutedBy != a.users["editor"] || detail.Username != "editor" || detail.FinishedAt == nil || detail.DraftID == nil || *detail.DraftID != d.ID {
		t.Fatalf("detail=%s", w.Body.String())
	}
	status(t, view.request("GET", fmt.Sprintf("/api/v1/databases/%d/migrations/1", other.ID), nil), 404)
	mark := func(id, state string) {
		t.Helper()
		if _, err := a.db.Exec(ctx, "UPDATE migration_history SET status=$1,finished_at=NULL WHERE execution_id=$2", state, id); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func() []runner.Result {
		t.Helper()
		w := editor.request("POST", base+"/reconcile", nil)
		status(t, w, 200)
		var out struct {
			Results []runner.Result `json:"reconciled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Results
	}
	mark(first.ExecutionID, "unknown")
	mark(failed.ExecutionID, "running")
	results := reconcile()
	if len(results) != 2 || results[0].Status != "success" || results[1].Status != "failed" || results[1].ErrorCode == nil || *results[1].ErrorCode != "reconciled_rollback" {
		t.Fatalf("reconcile=%+v", results)
	}
	if len(reconcile()) != 0 {
		t.Fatal("reconcile was not idempotent")
	}
	if execute(create("SELECT 2")).Version != 3 {
		t.Fatal("version sequence changed after recovery")
	}
	// Recovery must not infer rollback while an actual runner is still executing.
	checker := a.browser()
	checker.login(t, "editor")
	active := create("SELECT pg_sleep(4)")
	approveDraft(t, a, editor, target.ID, active.ID, 1)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- editor.executeAndWait(t, fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", target.ID, active.ID), map[string]string{"revision": "1"})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var running bool
		if err := a.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM migration_history WHERE database_id=$1 AND draft_id=$2 AND status='running')", target.ID, active.ID).Scan(&running); err != nil {
			t.Fatal(err)
		}
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner did not publish running attempt")
		}
		time.Sleep(20 * time.Millisecond)
	}
	w = checker.request("POST", base+"/reconcile", nil)
	status(t, w, 409)
	if !strings.Contains(w.Body.String(), "migration_busy") {
		t.Fatalf("unexpected lock conflict: %s", w.Body.String())
	}
	w = <-done
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("active runner disrupted: %s", w.Body.String())
	}
	// Target administrator fixture is required only for corruption/privilege fault injection.
	url := os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL")
	if url == "" {
		t.Fatal("explicit TEST_TARGET_ADMIN_DATABASE_URL required for receipt fault injection")
	}
	owner, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal("cannot open receipt fixture admin")
	}
	defer owner.Close()
	var signature string
	if err = owner.QueryRow(ctx, "SELECT signature FROM _terserah.migration_receipts WHERE execution_id=$1", first.ExecutionID).Scan(&signature); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, "UPDATE _terserah.migration_receipts SET signature='forged' WHERE execution_id=$1", first.ExecutionID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.Exec(ctx, "UPDATE _terserah.migration_receipts SET signature=$1 WHERE execution_id=$2", signature, first.ExecutionID)
	}()
	mark(first.ExecutionID, "unknown")
	results = reconcile()
	if len(results) != 1 || results[0].Status != "unknown" || results[0].ErrorCode == nil || *results[0].ErrorCode != "receipt_mismatch" {
		t.Fatalf("forged receipt accepted: %+v", results)
	}
	if _, err = owner.Exec(ctx, "UPDATE _terserah.migration_receipts SET signature=$1 WHERE execution_id=$2", signature, first.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if results = reconcile(); len(results) != 1 || results[0].Status != "success" {
		t.Fatal("valid receipt did not recover")
	}
	// Legacy attempts cannot safely infer rollback merely from absence of a signed receipt.
	mark(failed.ExecutionID, "unknown")
	if _, err = a.db.Exec(ctx, "UPDATE migration_history SET receipt_version=0 WHERE execution_id=$1", failed.ExecutionID); err != nil {
		t.Fatal(err)
	}
	results = reconcile()
	if len(results) != 1 || results[0].Status != "unknown" || results[0].ErrorCode == nil || *results[0].ErrorCode != "legacy_receipt_unverified" {
		t.Fatal("legacy attempt was guessed")
	}
	// The executor cannot alter, delete, or replace the trusted store.
	executor, err := database.Open(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	for _, sql := range []string{"DELETE FROM _terserah.migration_receipts", "UPDATE _terserah.migration_receipts SET signature='fake'", "TRUNCATE _terserah.migration_receipts", "DROP TABLE _terserah.migration_receipts", "DROP SCHEMA _terserah CASCADE"} {
		if _, err = executor.Exec(ctx, sql); err == nil {
			t.Fatal("executor can destroy receipts")
		}
	}
	// Even column-level mutation privilege must fail closed before recovery.
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "unused")
	role := pgx.Identifier{input.Username}.Sanitize()
	if _, err = owner.Exec(ctx, "GRANT UPDATE(signature) ON _terserah.migration_receipts TO "+role); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = owner.Exec(ctx, "REVOKE UPDATE(signature) ON _terserah.migration_receipts FROM "+role) }()
	status(t, editor.request("POST", base+"/reconcile", nil), 409)
	if _, err = owner.Exec(ctx, "REVOKE UPDATE(signature) ON _terserah.migration_receipts FROM "+role); err != nil {
		t.Fatal(err)
	}
	// A second registration of the actual target cannot start another version line.
	aliasInput := targetInput(t, "TEST_TARGET_DATABASE_URL", "alias")
	aliasInput.Port = commitProxy(t, net.JoinHostPort(aliasInput.Host, strconv.Itoa(aliasInput.Port)), false, false)
	alias := createTarget(t, admin, aliasInput)
	grant(t, a, "editor", alias.ID, "edit")
	w = editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", alias.ID), nil)
	status(t, w, 409)
	if !strings.Contains(w.Body.String(), "target_identity_conflict") {
		t.Fatalf("target alias accepted: %s", w.Body.String())
	}
	var identity string
	if err = owner.QueryRow(ctx, "SELECT id::text FROM _terserah.target_identity WHERE singleton").Scan(&identity); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.Exec(ctx, "UPDATE _terserah.target_identity SET id=$1::uuid WHERE singleton", identity)
	}()
	if _, err = owner.Exec(ctx, "UPDATE _terserah.target_identity SET id=gen_random_uuid() WHERE singleton"); err != nil {
		t.Fatal(err)
	}
	w = editor.request("POST", base+"/reconcile", nil)
	status(t, w, 409)
	if !strings.Contains(w.Body.String(), "target_identity_conflict") {
		t.Fatal("replacement target accepted")
	}
	if _, err = owner.Exec(ctx, "UPDATE _terserah.target_identity SET id=$1::uuid WHERE singleton", identity); err != nil {
		t.Fatal(err)
	}
	// A working owner connection is still unsafe for executing/reconciling SQL.
	ownerConfig, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	connectionPath := fmt.Sprintf("/api/v1/admin/databases/%d", target.ID)
	status(t, admin.request("PATCH", connectionPath, map[string]string{"username": ownerConfig.User, "password": ownerConfig.Password}), 200)
	w = editor.request("POST", base+"/reconcile", nil)
	status(t, w, 409)
	if !strings.Contains(w.Body.String(), "target_history_unprotected") {
		t.Fatal("owner account was accepted")
	}
	// History reads remain available without opening the target connection.
	status(t, view.request("GET", base, nil), 200)
	status(t, admin.request("PATCH", connectionPath, map[string]string{"username": input.Username, "password": *input.Password}), 200)
}

func TestReconcileCommitsEarlierAttemptsWhenOneWriteTimesOut(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	_, err := f.a.db.Exec(ctx, `INSERT INTO migration_history (database_id, version, sql, executed_by, status, execution_id, receipt_version)
 SELECT $1, n, 'SELECT 1', $2, 'unknown', 'audit-slow-meta-' || n, 1 FROM generate_series(1,3) AS n`, f.first, f.actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.a.db.Exec(ctx, `CREATE FUNCTION audit_slow_meta() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_sleep(4); RETURN NEW; END $$;
 CREATE TRIGGER audit_slow_meta BEFORE UPDATE OF status ON migration_history
 FOR EACH ROW WHEN (OLD.version = 2) EXECUTE FUNCTION audit_slow_meta();`)
	if err != nil {
		t.Fatal(err)
	}
	defer f.a.db.Exec(ctx, "DROP FUNCTION IF EXISTS audit_slow_meta() CASCADE")
	path := fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first)
	for _, expected := range []int64{1, 3} {
		limited, cancel := context.WithTimeout(ctx, 3*time.Second)
		w := f.editor.requestContext(limited, "POST", path, nil)
		cancel()
		status(t, w, 200)
		var response struct {
			Results []runner.Result `json:"reconciled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Results) != 1 || response.Results[0].Version != expected {
			t.Fatalf("expected version %d, got %s, err=%v", expected, w.Body, err)
		}
	}
	rows, err := f.a.db.Query(ctx, `SELECT status, last_reconciled_at IS NOT NULL FROM migration_history
 WHERE database_id=$1 ORDER BY version`, f.first)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var state string
		var checked bool
		err := row.Scan(&state, &checked)
		if checked {
			state += "-checked"
		}
		return state, err
	})
	if err != nil || strings.Join(got, ",") != "failed,unknown-checked,failed" {
		t.Fatalf("unexpected committed statuses: %v, err=%v", got, err)
	}
}
