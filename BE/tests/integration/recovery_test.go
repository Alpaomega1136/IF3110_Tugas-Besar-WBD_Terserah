//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
	"terserah/backend/internal/worker"
)

func recoveryDue(t *testing.T, f *queueFixture, id int64) {
	t.Helper()
	if _, err := f.a.db.Exec(context.Background(), `UPDATE migration_jobs SET available_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func jobPhase(t *testing.T, f *queueFixture, j jobs.Job, want jobs.Status) jobs.Job {
	t.Helper()
	current, _, err := f.service.Get(context.Background(), f.actor, j.DatabaseID, j.ID)
	if err != nil || current.Status != want {
		t.Fatalf("job phase=%s want=%s err=%v", current.Status, want, err)
	}
	return current
}

func TestRecoveryRetriesRolledBackTransientFailureWithBackoff(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "counter"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id int PRIMARY KEY,value int); INSERT INTO "+relation+" VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	barrier, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	if _, err = barrier.Exec(ctx, "SELECT * FROM "+relation+" FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	j := queuedJob(t, f, f.first, "UPDATE "+relation+" SET value=value+1 WHERE id=1")
	w := queueWorker(t, f)
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.Recovering)
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	recoveryDue(t, f, j.ID)
	if _, err = w.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.RetryWait)
	if found, err := w.ProcessNext(ctx); err != nil || found {
		t.Fatal("retry skipped its delay", err)
	}
	recoveryDue(t, f, j.ID)
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, j, jobs.Success)
	if current.AttemptCount != 2 {
		t.Fatal("retry did not allocate a fresh attempt")
	}
	var value int
	if err = db.QueryRow(ctx, "SELECT value FROM "+relation).Scan(&value); err != nil || value != 1 {
		t.Fatal("retry duplicated an update", value, err)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil || len(attempts) != 2 || attempts[0].Status != jobs.Failed || attempts[1].Status != jobs.Success || attempts[0].ExecutionID == attempts[1].ExecutionID {
		t.Fatal("retry replaced attempt history", err)
	}
}

func TestRecoveryVerifiesUnknownCommitBeforeRetry(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			f := newQueueFixture(t)
			db, schema := batchTarget(t)
			ctx := context.Background()
			relation := pgx.Identifier{schema, "counter"}.Sanitize()
			if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id int PRIMARY KEY,value int); INSERT INTO "+relation+" VALUES(1,0)"); err != nil {
				t.Fatal(err)
			}
			input := targetInput(t, "TEST_TARGET_DATABASE_URL", "recover-commit")
			port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, after)
			if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
				t.Fatal(err)
			}
			j := queuedJob(t, f, f.first, "UPDATE "+relation+" SET value=value+1")
			w := queueWorker(t, f)
			if _, err := w.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			jobPhase(t, f, j, jobs.Unknown)
			recoveryDue(t, f, j.ID)
			if _, err := w.RecoverNext(ctx); err != nil {
				t.Fatal(err)
			}
			if !after {
				jobPhase(t, f, j, jobs.RetryWait)
				recoveryDue(t, f, j.ID)
				if _, err := w.ProcessNext(ctx); err != nil {
					t.Fatal(err)
				}
			}
			current := jobPhase(t, f, j, jobs.Success)
			want := 1
			if !after {
				want = 2
			}
			if current.AttemptCount != want {
				t.Fatal("commit proof did not control retry", current.AttemptCount)
			}
			var value int
			if err := db.QueryRow(ctx, "SELECT value FROM "+relation).Scan(&value); err != nil || value != 1 {
				t.Fatal("ambiguous COMMIT was applied twice", value, err)
			}
		})
	}
}

func TestRecoveryCannotStealExpiredWorkerTargetLock(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	relation := pgx.Identifier{schema, "counter"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id int PRIMARY KEY,value int); INSERT INTO "+relation+" VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	barrier, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	if _, err = barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(3110201)`); err != nil {
		t.Fatal(err)
	}
	gate := pgx.Identifier{schema, "wait_for_recovery_probe"}.Sanitize()
	if _, err = db.Exec(ctx, "CREATE FUNCTION "+gate+"() RETURNS void LANGUAGE sql SET lock_timeout='30s' AS 'SELECT pg_catalog.pg_advisory_xact_lock(3110201)'"); err != nil {
		t.Fatal(err)
	}
	j := queuedJob(t, f, f.first, "UPDATE "+relation+" SET value=value+1; SELECT "+gate+"()")
	old := queueWorker(t, f)
	done := make(chan error, 1)
	go func() { _, err := old.ProcessNext(ctx); done <- err }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var locked bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=3110201 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old worker did not hold target lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.a.db.Exec(ctx, `UPDATE migration_jobs SET lease_expires_at=clock_timestamp()-interval '1 second',available_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	replacement := queueWorker(t, f)
	if _, err := replacement.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.Running)
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("expired worker finalized metadata", err)
	}
	recoveryDue(t, f, j.ID)
	if _, err := replacement.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.RetryWait)
	recoveryDue(t, f, j.ID)
	if _, err := replacement.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.Success)
	var value int
	if err := db.QueryRow(ctx, "SELECT value FROM "+relation).Scan(&value); err != nil || value != 1 {
		t.Fatal("stale worker committed data", value, err)
	}
}

func TestRecoveryStopsAtRetryLimitAndRejectsPermanentErrors(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, "CREATE FUNCTION "+pgx.Identifier{schema, "fail"}.Sanitize()+"() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='40001',MESSAGE='injected serialization failure'; END $$"); err != nil {
		t.Fatal(err)
	}
	j := queuedJob(t, f, f.first, "SELECT "+pgx.Identifier{schema, "fail"}.Sanitize()+"()")
	w := queueWorker(t, f)
	for attempt := 1; attempt <= 3; attempt++ {
		recoveryDue(t, f, j.ID)
		if _, err := w.ProcessNext(ctx); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			jobPhase(t, f, j, jobs.Recovering)
			recoveryDue(t, f, j.ID)
			if _, err := w.RecoverNext(ctx); err != nil {
				t.Fatal(err)
			}
			jobPhase(t, f, j, jobs.RetryWait)
		}
	}
	if current := jobPhase(t, f, j, jobs.Failed); current.AttemptCount != 3 {
		t.Fatal("retry limit was not enforced")
	}
	if found, err := w.RecoverNext(ctx); found || err != nil {
		t.Fatal("exhausted job kept retrying", err)
	}
	permanent := queuedJob(t, f, f.first, "SELECT 1/0")
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	if current := jobPhase(t, f, permanent, jobs.Failed); current.AttemptCount != 1 {
		t.Fatal("permanent SQL error was retried")
	}
}

func TestRecoveryVerifiesWithRevokedActorButDoesNotRetrySQL(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	j := queuedJob(t, f, f.first, "SELECT 1")
	c, err := f.service.Claim(ctx, "dead-worker", 30*time.Second)
	if err != nil || c == nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE group_database_permissions SET access_level='view' WHERE database_id=$1;`, f.first); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET lease_expires_at=clock_timestamp()-interval '1 second',available_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = queueWorker(t, f).RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, j, jobs.Failed)
	if current.ErrorCode == nil || *current.ErrorCode != "access_revoked" || current.AttemptCount != 1 {
		t.Fatal("revoked actor was allowed to retry")
	}
	target, err := database.Open(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var count int
	if err = target.QueryRow(ctx, `SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1`, c.ExecutionID).Scan(&count); err != nil || count != 0 {
		t.Fatal("recovery executed actor SQL", err)
	}
}

func TestRecoveryMetadataTimeoutPreservesRetryCause(t *testing.T) {
	t.Run("worker_deadline", func(t *testing.T) {
		testRecoveryMetadataTimeout(t, time.Second, "")
	})
	t.Run("postgres_statement_timeout", func(t *testing.T) {
		testRecoveryMetadataTimeout(t, 10*time.Second, "1s")
	})
}

func testRecoveryMetadataTimeout(t *testing.T, jobTimeout time.Duration, statementTimeout string) {
	t.Helper()
	f := newQueueFixture(t)
	ctx := context.Background()
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "audit-timeout")
	port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, false)
	if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
		t.Fatal(err)
	}
	j := queuedJob(t, f, f.first, "SELECT 1")
	w := queueWorker(t, f)
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	before := jobPhase(t, f, j, jobs.Unknown)
	if before.ErrorCode == nil || *before.ErrorCode != "commit_unknown" {
		t.Fatalf("unexpected original job: %+v", before)
	}
	healthy := queuedJob(t, f, f.second, "SELECT 2")
	claim, err := f.service.Claim(ctx, "expired-test-worker", 30*time.Second)
	if err != nil || claim == nil || claim.Job.ID != healthy.ID {
		t.Fatalf("could not claim healthy job %d: %v", healthy.ID, err)
	}
	if _, err := f.a.db.Exec(ctx, `UPDATE migration_jobs SET
 available_at=clock_timestamp()-CASE WHEN id=$1 THEN interval '2 seconds' ELSE interval '1 second' END,
 lease_expires_at=CASE WHEN id=$2 THEN clock_timestamp()-interval '1 second' ELSE lease_expires_at END
 WHERE id IN ($1,$2)`, j.ID, healthy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.Exec(ctx, `CREATE FUNCTION audit_retry_write() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_sleep(2); RETURN NEW; END $$;
 CREATE TRIGGER audit_retry_write BEFORE UPDATE OF status ON migration_jobs
 FOR EACH ROW WHEN (OLD.status = 'unknown' AND NEW.status = 'retry_wait') EXECUTE FUNCTION audit_retry_write();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.a.db.Exec(ctx, "DROP FUNCTION IF EXISTS audit_retry_write() CASCADE"); err != nil {
			t.Error(err)
		}
	})
	metadata := f.a.db
	if statementTimeout != "" {
		cfg := metadata.Config()
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout
		metadata, err = pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer metadata.Close()
	}
	probe, err := worker.New(jobs.New(metadata), runner.New(metadata, f.a.targets, f.a.key), worker.Options{JobTimeout: jobTimeout})
	if err != nil {
		t.Fatal(err)
	}
	if found, err := probe.RecoverNext(ctx); !found || err != nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
	delayed := jobPhase(t, f, j, jobs.Unknown)
	if delayed.AttemptCount != before.AttemptCount || delayed.ErrorCode == nil || *delayed.ErrorCode != *before.ErrorCode {
		t.Fatalf("unexpected delayed job: %+v", delayed)
	}
	candidate, err := f.service.RecoveryCandidate(ctx)
	if err != nil || candidate == nil || candidate.Job.ID != healthy.ID {
		t.Fatalf("expected healthy job %d next, got %v, err=%v", healthy.ID, candidate, err)
	}
	if found, err := probe.RecoverNext(ctx); !found || err != nil {
		t.Fatalf("healthy recovery did not progress: found=%v err=%v", found, err)
	}
	jobPhase(t, f, healthy, jobs.RetryWait)
	if _, err = f.a.db.Exec(ctx, "DROP TRIGGER audit_retry_write ON migration_jobs"); err != nil {
		t.Fatal(err)
	}
	// An earlier server could have overwritten the job code, while the attempt still has the original cause.
	if _, err = f.a.db.Exec(ctx, "UPDATE migration_jobs SET error_code='metadata_unavailable' WHERE id=$1", j.ID); err != nil {
		t.Fatal(err)
	}
	recoveryDue(t, f, j.ID)
	if found, err := w.RecoverNext(ctx); !found || err != nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
	next := jobPhase(t, f, j, jobs.RetryWait)
	if next.AttemptCount != 1 || next.ErrorCode == nil || *next.ErrorCode != "worker_interrupted" {
		t.Fatalf("rollback proof did not schedule retry for the original attempt: %+v", next)
	}
}
