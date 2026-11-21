//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
)

func TestBatchTransientRetryKeepsScopeAndPriorAttemptHistory(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) n"); err != nil {
		t.Fatal(err)
	}
	barrier, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	if _, err = barrier.Exec(ctx, "SELECT * FROM "+relation+" WHERE id=3 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	j := enqueueBatch(t, f, batchConfig(schema))
	w := queueWorker(t, f)
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.Recovering)
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Detect accidental reapplication and accidental rescoping separately.
	if _, err = db.Exec(ctx, "UPDATE "+relation+" SET value=99 WHERE id=1; INSERT INTO "+relation+" VALUES(6,0)"); err != nil {
		t.Fatal(err)
	}
	recoveryDue(t, f, j.ID)
	if _, err = w.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, j, jobs.RetryWait)
	if current.CommittedBatches != 1 || current.CommittedRows != 2 {
		t.Fatal("verified prefix lost", current)
	}
	recoveryDue(t, f, j.ID)
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "retry-final-proxy")
	port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, true, 3)
	if _, err = f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
		t.Fatal(err)
	}
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.Unknown)
	recoveryDue(t, f, j.ID)
	if _, err = w.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	current = jobPhase(t, f, j, jobs.Success)
	if current.AttemptCount != 2 || current.CommittedBatches != 3 || current.CommittedRows != 5 {
		t.Fatal("retry progress incorrect", current)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil || len(attempts) != 2 || attempts[0].Status != jobs.Partial || attempts[1].Status != jobs.Success {
		t.Fatal("retry rewrote prior attempt", attempts, err)
	}
	var valid bool
	err = db.QueryRow(ctx, "SELECT (SELECT value=99 FROM "+relation+" WHERE id=1) AND (SELECT value=0 FROM "+relation+" WHERE id=6) AND (SELECT count(*)=3 FROM "+relation+" WHERE id BETWEEN 3 AND 5 AND value=2)").Scan(&valid)
	if err != nil || !valid {
		t.Fatal("retry repeated a committed row or changed scope", err)
	}
	var root string
	var state string
	if err = f.a.db.QueryRow(ctx, `SELECT batch_root_execution_id,status FROM migration_history WHERE execution_id=$1`, attempts[0].ExecutionID).Scan(&root, &state); err != nil || root != attempts[0].ExecutionID || state != "partial" {
		t.Fatal("root attempt history was replaced", err)
	}
	err = db.QueryRow(ctx, `SELECT count(*)=3 AND count(*) FILTER(WHERE batch_no=1 AND attempt_execution_id=$1)=1 AND count(*) FILTER(WHERE batch_no>1 AND attempt_execution_id=$2)=2 FROM _terserah.batch_checkpoints WHERE execution_id=$1`, root, attempts[1].ExecutionID).Scan(&valid)
	if err != nil || !valid {
		t.Fatal("retry checkpoints lost their attempt identity", err)
	}
	// Reconcile must not turn the sealed original partial attempt into success.
	status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil), 200)
	if err = f.a.db.QueryRow(ctx, `SELECT status FROM migration_history WHERE execution_id=$1`, root).Scan(&state); err != nil || state != "partial" {
		t.Fatal("reconcile rewrote sealed history", err)
	}
}

func TestBatchAutomaticRecoveryVerifiesLostCommitBeforeContinuing(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		after    bool
		cut      int
		attempts int
	}{
		{"before_middle", false, 3, 2}, {"after_middle", true, 3, 2}, {"after_final", true, 4, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newQueueFixture(t)
			db, schema := batchTarget(t)
			ctx := context.Background()
			relation := pgx.Identifier{schema, "items"}.Sanitize()
			if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) n"); err != nil {
				t.Fatal(err)
			}
			input := targetInput(t, "TEST_TARGET_DATABASE_URL", "retry-proxy")
			port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, scenario.after, scenario.cut)
			if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
				t.Fatal(err)
			}
			j := enqueueBatch(t, f, batchConfig(schema))
			w := queueWorker(t, f)
			if _, err := w.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			jobPhase(t, f, j, jobs.Unknown)
			recoveryDue(t, f, j.ID)
			if _, err := w.RecoverNext(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario.attempts == 2 {
				jobPhase(t, f, j, jobs.RetryWait)
				recoveryDue(t, f, j.ID)
				if _, err := w.ProcessNext(ctx); err != nil {
					t.Fatal(err)
				}
			}
			current := jobPhase(t, f, j, jobs.Success)
			if current.AttemptCount != scenario.attempts || current.CommittedRows != 5 {
				t.Fatal("commit recovery retried confirmed SQL or lost progress", current)
			}
			var images int
			canonical, err := batch.Encode(batchConfig(schema))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_rows WHERE execution_id=(SELECT execution_id FROM _terserah.batch_runs WHERE sql=$1)`, canonical).Scan(&images); err != nil || images != 5 {
				t.Fatal("repeated recovery images", images, err)
			}
		})
	}
}

// A real OS process dies after the first target commit, before metadata progress.
func TestBatchWorkerProcessRestartContinuesUnreportedCheckpoint(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) n"); err != nil {
		t.Fatal(err)
	}
	barrier, err := f.a.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	if _, err = barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(3110200)`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `CREATE FUNCTION pause_restart_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.committed_batches=1 AND OLD.committed_batches=0 THEN PERFORM pg_advisory_xact_lock(3110200); END IF; RETURN NEW; END $$; CREATE TRIGGER pause_restart_progress BEFORE UPDATE ON migration_jobs FOR EACH ROW EXECUTE FUNCTION pause_restart_progress()`); err != nil {
		t.Fatal(err)
	}
	c := batchConfig(schema)
	c.Size = 1
	j := enqueueBatch(t, f, c)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binary := filepath.Join(t.TempDir(), "worker"+suffix)
	buildCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-race", "-o", binary, "./cmd/worker")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v %s", err, output)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", f.a.db.Config().ConnConfig.RuntimeParams["search_path"])
	u.RawQuery = query.Encode()
	env := append(os.Environ(), "DATABASE_URL="+u.String(), "TARGET_CREDENTIAL_KEY="+base64.StdEncoding.EncodeToString(f.a.key), "OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=")
	start := func() func() {
		cmd := exec.Command(binary)
		cmd.Env = env
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		stop := func() { _ = cmd.Process.Kill(); <-done }
		t.Cleanup(stop)
		return stop
	}
	stop := start()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting bool
		if err = f.a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=3110200 AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not reach committed checkpoint")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if err = barrier.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `DROP TRIGGER pause_restart_progress ON migration_jobs; UPDATE migration_jobs SET lease_expires_at=clock_timestamp()-interval '1 second',available_at=clock_timestamp()-interval '1 second' WHERE status='running'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, "UPDATE "+relation+" SET value=99 WHERE id=1; INSERT INTO "+relation+" VALUES(6,0)"); err != nil {
		t.Fatal(err)
	}
	stopReplacement := start()
	defer stopReplacement()
	deadline = time.Now().Add(20 * time.Second)
	var current jobs.Job
	for {
		current, _, err = f.service.Get(ctx, f.actor, f.first, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == jobs.Success {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement worker failed to recover", current.Status, current.ErrorCode)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if current.AttemptCount != 2 || current.CommittedBatches != 5 || current.CommittedRows != 5 {
		t.Fatal("process restart lost committed checkpoint", current)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil || len(attempts) != 2 || attempts[0].Status != jobs.Partial {
		t.Fatal("restart lost original attempt", err)
	}
	var valid bool
	if err = db.QueryRow(ctx, "SELECT (SELECT value=99 FROM "+relation+" WHERE id=1) AND (SELECT value=0 FROM "+relation+" WHERE id=6)").Scan(&valid); err != nil || !valid {
		t.Fatal("restart reapplied committed batch or changed scope", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*)=5 AND count(*) FILTER(WHERE batch_no=1 AND attempt_execution_id=$1)=1 AND count(*) FILTER(WHERE batch_no>1 AND attempt_execution_id=$2)=4 FROM _terserah.batch_checkpoints WHERE execution_id=$1`, attempts[0].ExecutionID, attempts[1].ExecutionID).Scan(&valid); err != nil || !valid {
		t.Fatal("restart did not retain root checkpoint chain", err)
	}
}
