//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/database"
	"terserah/backend/internal/runner"
)

func TestRunnerAPI(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "runner-target"))
	other := createTarget(t, admin, targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "runner-other"))
	grant(t, a, "editor", target.ID, "edit")
	grant(t, a, "viewer", target.ID, "view")
	editor := a.browser()
	editor.login(t, "editor")
	view := a.browser()
	view.login(t, "viewer")
	db, err := database.Open(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := fmt.Sprintf("runner_%d", time.Now().UnixNano())
	q := pgx.Identifier{schema}.Sanitize()
	if _, err = db.Exec(ctx, "CREATE SCHEMA "+q); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.Exec(ctx, "DROP SCHEMA "+q+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	base := fmt.Sprintf("/api/v1/databases/%d/drafts", target.ID)
	if _, err = db.Exec(ctx, "CREATE TABLE "+q+".plan_items(id integer); INSERT INTO "+q+".plan_items VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	wPlan := editor.request("POST", base, map[string]string{"title": "plan", "sql": "UPDATE " + q + ".plan_items SET id=2; CREATE TABLE " + q + ".unsupported(id int)", "sql_down": "DROP TABLE IF EXISTS " + q + ".unsupported; UPDATE " + q + ".plan_items SET id=1"})
	status(t, wPlan, 201)
	var planDraft runner.Draft
	if err = json.Unmarshal(wPlan.Body.Bytes(), &planDraft); err != nil {
		t.Fatal(err)
	}
	planPath := fmt.Sprintf("%s/%d/explain", base, planDraft.ID)
	status(t, a.browser().request("POST", planPath, map[string]string{"revision": "1"}), 401)
	status(t, view.request("POST", planPath, map[string]string{"revision": "1"}), 403)
	status(t, editor.request("POST", planPath, map[string]string{"revision": "2"}), 409)
	wPlan = editor.request("POST", planPath, map[string]string{"revision": "1"})
	status(t, wPlan, 200)
	var plans struct {
		Statements []struct {
			Plan  json.RawMessage `json:"plan"`
			Error string          `json:"error"`
		} `json:"statements"`
	}
	if err = json.Unmarshal(wPlan.Body.Bytes(), &plans); err != nil || len(plans.Statements) != 2 || len(plans.Statements[0].Plan) == 0 || plans.Statements[1].Error == "" {
		t.Fatalf("unexpected plan response: %s", wPlan.Body.String())
	}
	var unchanged int
	if err = db.QueryRow(ctx, "SELECT id FROM "+q+".plan_items").Scan(&unchanged); err != nil || unchanged != 1 {
		t.Fatalf("EXPLAIN changed data: %d, %v", unchanged, err)
	}
	input := map[string]string{"title": "create table", "sql": "CREATE TABLE " + q + ".items(id int PRIMARY KEY, note text); INSERT INTO " + q + ".items VALUES(1,'semi;colon')", "sql_down": "DROP FUNCTION IF EXISTS " + q + ".echo(); DROP TABLE IF EXISTS " + q + ".items"}
	status(t, a.browser().request("POST", base, input), 401)
	status(t, view.request("POST", base, input), 403)
	status(t, admin.request("POST", base, input), 403)
	input["sql"] += "; CREATE FUNCTION " + q + ".echo() RETURNS text LANGUAGE plpgsql AS $body$ BEGIN RETURN 'semi;colon'; END; $body$; SELECT " + q + ".echo()"
	w := editor.request("POST", base, input)
	status(t, w, 201)
	var d runner.Draft
	if err = json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("%s/%d", base, d.ID)
	status(t, editor.request("GET", path, nil), 200)
	status(t, view.request("GET", path, nil), 403)
	status(t, editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", other.ID, d.ID), map[string]string{"revision": "1"}), 403)
	grant(t, a, "editor", other.ID, "edit")
	status(t, editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", other.ID, d.ID), map[string]string{"revision": "1"}), 404)
	status(t, view.request("POST", path+"/execute", map[string]string{"revision": "1"}), 403)
	status(t, editor.request("POST", path+"/execute", map[string]string{"revision": "2"}), 409)
	status(t, editor.request("POST", path+"/execute", map[string]string{"revision": "1", "sql": "DROP TABLE x"}), 400)
	execute := func(path, rev string) runner.Result {
		t.Helper()
		var draftID, revision int64
		if _, err := fmt.Sscanf(path, "/api/v1/databases/"+fmt.Sprint(target.ID)+"/drafts/%d", &draftID); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Sscan(rev, &revision); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := a.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM migration_reviews WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3 AND status IN ('approved','queued'))", target.ID, draftID, revision).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			approveDraft(t, a, editor, target.ID, draftID, revision)
		}
		w := editor.executeAndWait(t, path+"/execute", map[string]string{"revision": rev})
		status(t, w, 200)
		var out runner.Result
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	originalCSRF := editor.csrf
	editor.csrf = "invalid"
	status(t, editor.request("POST", path+"/execute", map[string]string{"revision": "1"}), 403)
	editor.csrf = originalCSRF
	out := execute(path, "1")
	if out.Status != "success" || out.Version != 1 {
		t.Fatalf("result=%+v", out)
	}
	status(t, editor.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas/_terserah/tables", target.ID), nil), 403)
	replay := execute(path, "1")
	if replay.ExecutionID != out.ExecutionID || replay.Status != "success" {
		t.Fatal("replay did not return original result")
	}
	var count int
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+q+".items").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
	var receiptSQL, actorSQL string
	var actor int64
	if err = db.QueryRow(ctx, "SELECT sql,executed_by FROM _terserah.migration_receipts WHERE execution_id=$1", out.ExecutionID).Scan(&receiptSQL, &actor); err != nil || receiptSQL != input["sql"] || actor != a.users["editor"] {
		t.Fatal("missing atomic receipt", err)
	}
	if err = a.db.QueryRow(ctx, "SELECT sql FROM migration_history WHERE execution_id=$1", out.ExecutionID).Scan(&actorSQL); err != nil || actorSQL != receiptSQL {
		t.Fatal("ledger snapshot mismatch")
	}
	// DDL and DML before a failing middle statement must both disappear.
	badSQL := "CREATE TABLE " + q + ".rolled_back(id int); INSERT INTO " + q + ".items VALUES(2,'rollback'); SELECT 1/0; INSERT INTO " + q + ".items VALUES(3,'never')"
	// Draft yang sudah sukses tidak boleh diedit; kegagalan diuji lewat migration baru.
	w = editor.request("PATCH", path, map[string]string{"revision": "1", "title": "failure", "sql": badSQL, "sql_down": "SELECT 1"})
	status(t, w, 409)
	w = editor.request("POST", base, map[string]string{
		"title": "failure", "sql": badSQL,
		"sql_down": "DROP TABLE IF EXISTS " + q + ".rolled_back; DELETE FROM " + q + ".items WHERE id IN (2,3)",
	})
	status(t, w, 201)
	var failedDraft runner.Draft
	if err = json.Unmarshal(w.Body.Bytes(), &failedDraft); err != nil {
		t.Fatal(err)
	}
	failedPath := fmt.Sprintf("%s/%d", base, failedDraft.ID)
	status(t, editor.request("POST", failedPath+"/execute", map[string]string{"revision": "2"}), 409)
	failed := execute(failedPath, "1")
	if failed.Status != "failed" || failed.Version != 2 || failed.ErrorCode == nil || *failed.ErrorCode != "22012" {
		t.Fatalf("result=%+v", failed)
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+q+".items").Scan(&count); err != nil || count != 1 {
		t.Fatal("DML was not rolled back")
	}
	var absent bool
	if err = db.QueryRow(ctx, "SELECT to_regclass($1) IS NULL", schema+".rolled_back").Scan(&absent); err != nil || !absent {
		t.Fatal("DDL was not rolled back")
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1", failed.ExecutionID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed execution has a success receipt")
	}
	if execute(failedPath, "1").ExecutionID != failed.ExecutionID {
		t.Fatal("failed revision retried")
	}
	for _, sql := range []string{"SELECT 1;COMMIT", "BEGIN;SELECT 1", "ROLLBACK", "SET ROLE test", "CREATE DATABASE blocked", "CREATE INDEX CONCURRENTLY ix ON t(id)", "CREATE SUBSCRIPTION s CONNECTION 'redacted' PUBLICATION p"} {
		status(t, editor.request("POST", base, map[string]string{"title": "reject", "sql": sql, "sql_down": "SELECT 1"}), 400)
	}
	// Draft text is validated again at execution, even for pre-existing database rows.
	var unsafeID int64
	if err = a.db.QueryRow(ctx, "INSERT INTO migration_drafts(database_id,title,sql,created_by) VALUES($1,'unsafe','SELECT 1;COMMIT',$2) RETURNING id", target.ID, a.users["editor"]).Scan(&unsafeID); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, unsafeID, 1)
	status(t, editor.request("POST", fmt.Sprintf("%s/%d/execute", base, unsafeID), map[string]string{"revision": "1"}), 400)
	// Replaying a pending execution must not run SQL or create another attempt.
	_, err = a.db.Exec(ctx, "UPDATE migration_history SET status='unknown' WHERE execution_id=$1", failed.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	status(t, editor.request("POST", failedPath+"/execute", map[string]string{"revision": "1"}), 200)
	status(t, editor.request("PATCH", failedPath, map[string]string{"revision": "1", "title": "blocked", "sql": "SELECT 1", "sql_down": "SELECT 1"}), 409)
	_, err = a.db.Exec(ctx, "UPDATE migration_history SET status='failed' WHERE execution_id=$1", failed.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	// Independent sessions issue the same revision simultaneously; only one executes.
	input["sql"] = "INSERT INTO " + q + ".items VALUES(4,'once'); SELECT pg_sleep(0.1)"
	input["sql_down"] = "DELETE FROM " + q + ".items WHERE id=4"
	w = editor.request("POST", base, input)
	status(t, w, 201)
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	concurrentPath := fmt.Sprintf("%s/%d/execute", base, d.ID)
	approveDraft(t, a, editor, target.ID, d.ID, 1)
	browsers := []*browser{a.browser(), a.browser()}
	for _, b := range browsers {
		b.login(t, "editor")
	}
	var wg sync.WaitGroup
	results := make([]runner.Result, 2)
	codes := make([]int, 2)
	decodeErrors := make([]error, 2)
	for i, b := range browsers {
		wg.Add(1)
		go func(i int, b *browser) {
			defer wg.Done()
			w := b.executeAndWait(t, concurrentPath, map[string]string{"revision": "1"})
			codes[i] = w.Code
			decodeErrors[i] = json.Unmarshal(w.Body.Bytes(), &results[i])
		}(i, b)
	}
	wg.Wait()
	for i := range results {
		if decodeErrors[i] != nil || (codes[i] != 200 && codes[i] != 202) || results[i].ExecutionID == "" {
			t.Fatalf("concurrent result: code=%d result=%+v error=%v", codes[i], results[i], decodeErrors[i])
		}
	}
	if results[0].ExecutionID != results[1].ExecutionID || execute(fmt.Sprintf("%s/%d", base, d.ID), "1").Status != "success" {
		t.Fatalf("concurrent results: %+v", results)
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+q+".items WHERE id=4").Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent duplicate execution")
	}
	// Statement timeout rolls back earlier writes and persists failure after cancellation.
	input["sql"] = "INSERT INTO " + q + ".items VALUES(5,'timeout'); SELECT pg_sleep(10)"
	input["sql_down"] = "DELETE FROM " + q + ".items WHERE id=5"
	w = editor.request("POST", base, input)
	status(t, w, 201)
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	timeout := execute(fmt.Sprintf("%s/%d", base, d.ID), "1")
	if timeout.Status != "failed" || timeout.ErrorCode == nil || *timeout.ErrorCode != "execution_timeout" {
		t.Fatalf("timeout=%+v", timeout)
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+q+".items WHERE id=5").Scan(&count); err != nil || count != 0 {
		t.Fatal("timeout committed data")
	}
	input["sql"] = "INSERT INTO " + q + ".items VALUES(6,'deadline'); SELECT pg_sleep(4); SELECT pg_sleep(4)"
	input["sql_down"] = "DELETE FROM " + q + ".items WHERE id=6"
	w = editor.request("POST", base, input)
	status(t, w, 201)
	if err = json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	deadline := execute(fmt.Sprintf("%s/%d", base, d.ID), "1")
	if deadline.Status != "failed" {
		t.Fatalf("deadline=%+v", deadline)
	}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+q+".items WHERE id=6").Scan(&count); err != nil || count != 0 {
		t.Fatal("request deadline committed data")
	}
	var ledgerStatus string
	if err = a.db.QueryRow(ctx, "SELECT status FROM migration_history WHERE execution_id=$1", deadline.ExecutionID).Scan(&ledgerStatus); err != nil || ledgerStatus != "failed" {
		t.Fatal("deadline failure was not persisted")
	}
	// Deferred constraints can fail only during COMMIT; a server rejection is a
	// confirmed rollback, rather than an ambiguous network outcome.
	input["sql"] = "CREATE TABLE " + q + ".deferred_failure(id int UNIQUE DEFERRABLE INITIALLY DEFERRED); INSERT INTO " + q + ".deferred_failure VALUES(1),(1)"
	input["sql_down"] = "DROP TABLE IF EXISTS " + q + ".deferred_failure"
	w = editor.request("POST", base, input)
	status(t, w, 201)
	if err = json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	commitFailure := execute(fmt.Sprintf("%s/%d", base, d.ID), "1")
	if commitFailure.Status != "failed" || commitFailure.ErrorCode == nil || *commitFailure.ErrorCode != "23505" {
		t.Fatalf("commit failure=%+v", commitFailure)
	}
	if err = db.QueryRow(ctx, "SELECT to_regclass($1) IS NULL", schema+".deferred_failure").Scan(&absent); err != nil || !absent {
		t.Fatal("failed COMMIT retained DDL")
	}

}
