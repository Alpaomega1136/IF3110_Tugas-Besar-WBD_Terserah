//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestClaimPersistsHistoryAndRootBeforeTargetExecution(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	j := enqueueBatch(t, f, batchConfig("public"))
	c, err := f.service.Claim(ctx, "durable-claim", 30*time.Second)
	if err != nil || c == nil || c.Version != 1 || c.RootExecutionID != c.ExecutionID {
		t.Fatal("claim did not bind durable attempt/root", err)
	}
	var history, root string
	if err = f.a.db.QueryRow(ctx, `SELECT status,batch_root_execution_id FROM migration_history WHERE execution_id=$1`, c.ExecutionID).Scan(&history, &root); err != nil || history != "running" || root != c.ExecutionID {
		t.Fatal("claimed job has no durable history", err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET batch_root_execution_id=NULL WHERE id=$1`, j.ID); err == nil {
		t.Fatal("batch root could be replaced")
	}
	status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil), 409)
	other := queuedJob(t, f, f.second, "SELECT 2")
	if _, err = f.a.db.Exec(ctx, `CREATE FUNCTION reject_history_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected history failure'; END $$;
 CREATE TRIGGER reject_history_claim BEFORE INSERT ON migration_history FOR EACH ROW EXECUTE FUNCTION reject_history_claim()`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Claim(ctx, "failed-claim", 30*time.Second); err == nil {
		t.Fatal("history failure did not abort claim")
	}
	var state string
	var attempts int
	if err = f.a.db.QueryRow(ctx, `SELECT status,attempt_count FROM migration_jobs WHERE id=$1`, other.ID).Scan(&state, &attempts); err != nil || state != "queued" || attempts != 0 {
		t.Fatal("failed history claim consumed job", err)
	}
	if err = f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_job_attempts WHERE job_id=$1`, other.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("failed claim left an attempt", err)
	}
}

func TestRetryClaimKeepsRootAndCreatesANewExecution(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	j := enqueueBatch(t, f, batchConfig("public"))
	first, err := f.service.Claim(ctx, "first", 30*time.Second)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	// Model the metadata transition performed only after the recovery proof succeeds.
	tx, err := f.a.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE migration_history SET status='failed',error_code='40001',finished_at=clock_timestamp() WHERE execution_id=$1`, first.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET status='failed',error_code='40001',finished_at=clock_timestamp(),retry_scheduled=true WHERE execution_id=$1`, first.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_jobs SET status='retry_wait',worker_id=NULL,lease_token_hash=NULL,lease_expires_at=NULL,available_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if next, err := f.service.Claim(ctx, "too-early", 30*time.Second); err != nil || next != nil {
		t.Fatal("retry ignored backoff", err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET available_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	second, err := f.service.Claim(ctx, "second", 30*time.Second)
	if err != nil || second == nil || second.ExecutionID == first.ExecutionID || second.Version != 2 || second.RootExecutionID != first.ExecutionID || second.Job.AttemptCount != 2 {
		t.Fatal("retry lost audit identity or root", err)
	}
	var oldState string
	if err = f.a.db.QueryRow(ctx, `SELECT status FROM migration_history WHERE execution_id=$1`, first.ExecutionID).Scan(&oldState); err != nil || oldState != "failed" {
		t.Fatal("retry rewrote prior history", err)
	}
}
