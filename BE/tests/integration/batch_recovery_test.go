//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
)

func batchConfig(schema string) batch.Config {
	return batch.Config{Schema: schema, Table: "items", Key: "id", Size: 2, Set: map[string]json.RawMessage{"value": json.RawMessage("2")}}
}

func TestBatchReconcilesLostCommitResponsesWithoutRepeatingUpdates(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		after         bool
		cut           int
		rows, batches int64
		state         jobs.Status
	}{{"before_middle_commit", false, 3, 2, 1, jobs.Partial}, {"after_middle_commit", true, 3, 4, 2, jobs.Partial}, {"after_final_commit", true, 4, 5, 3, jobs.Success}} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newQueueFixture(t)
			db, schema := batchTarget(t)
			ctx := context.Background()
			relation := pgx.Identifier{schema, "items"}.Sanitize()
			if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) AS n"); err != nil {
				t.Fatal(err)
			}
			input := targetInput(t, "TEST_TARGET_DATABASE_URL", "batch-proxy")
			port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, scenario.after, scenario.cut)
			if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
				t.Fatal(err)
			}
			j := enqueueBatch(t, f, batchConfig(schema))
			if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			a := assertJob(t, f, j, jobs.Unknown, "commit_unknown")
			status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil), 200)
			assertJob(t, f, j, scenario.state, "")
			current, _, err := f.service.Get(ctx, f.actor, f.first, j.ID)
			if err != nil || current.CommittedRows != scenario.rows || current.CommittedBatches != scenario.batches {
				t.Fatal("target proof did not restore committed progress", current, err)
			}
			var changed, evidence int64
			if err = db.QueryRow(ctx, "SELECT count(*) FROM "+relation+" WHERE value=2").Scan(&changed); err != nil || changed != scenario.rows {
				t.Fatal("commit outcome changed target unexpectedly", changed, err)
			}
			if err = db.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_rows WHERE execution_id=$1`, a.ExecutionID).Scan(&evidence); err != nil || evidence != scenario.rows {
				t.Fatal("committed recovery images missing", err)
			}
			status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil), 200)
			if found, err := queueWorker(t, f).ProcessNext(ctx); found || err != nil {
				t.Fatal("reconcile requeued original SQL", err)
			}
		})
	}
}

func TestBatchRestoresLaggingProgressAndRejectsTamperedEvidence(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) AS n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.Exec(ctx, `CREATE FUNCTION reject_batch_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.committed_batches>OLD.committed_batches THEN RAISE EXCEPTION 'injected progress failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_batch_progress BEFORE UPDATE ON migration_jobs FOR EACH ROW EXECUTE FUNCTION reject_batch_progress()`); err != nil {
		t.Fatal(err)
	}
	j := enqueueBatch(t, f, batchConfig(schema))
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	a := assertJob(t, f, j, jobs.Unknown, "progress_unavailable")
	if _, err := f.a.db.Exec(ctx, `DROP TRIGGER reject_batch_progress ON migration_jobs`); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first)
	status(t, f.editor.request("POST", path, nil), 200)
	assertJob(t, f, j, jobs.Partial, "reconciled_partial")
	current, _, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil || current.CommittedRows != 2 || current.CommittedBatches != 1 {
		t.Fatal("lagging metadata was not synchronized", err)
	}
	owner, err := database.Open(ctx, os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var original string
	if err = owner.QueryRow(ctx, `SELECT signature FROM _terserah.batch_checkpoints WHERE execution_id=$1 AND batch_no=1`, a.ExecutionID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE _terserah.batch_checkpoints SET signature='forged' WHERE execution_id=$1`, a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	status(t, f.editor.request("POST", path, nil), 409)
	if _, err = owner.Exec(ctx, `UPDATE _terserah.batch_checkpoints SET signature=$2 WHERE execution_id=$1 AND batch_no=1`, a.ExecutionID, original); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO _terserah.batch_scope(execution_id,row_key) VALUES($1,999)`, a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	status(t, f.editor.request("POST", path, nil), 409)
	assertJob(t, f, j, jobs.Partial, "reconciled_partial")
}

func TestBatchFreezesScopeAndHoldsLockBetweenCommits(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" VALUES(1,0),(3,0),(5,0)"); err != nil {
		t.Fatal(err)
	}
	barrier, err := f.a.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(context.Background())
	if _, err = barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(3110198)`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `CREATE FUNCTION pause_batch_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.committed_batches=1 AND OLD.committed_batches=0 THEN PERFORM pg_advisory_xact_lock(3110198); END IF; RETURN NEW; END $$; CREATE TRIGGER pause_batch_progress BEFORE UPDATE ON migration_jobs FOR EACH ROW EXECUTE FUNCTION pause_batch_progress()`); err != nil {
		t.Fatal(err)
	}
	c := batchConfig(schema)
	c.Size = 1
	j := enqueueBatch(t, f, c)
	done := make(chan error, 1)
	background := queueWorker(t, f)
	go func() { _, err := background.ProcessNext(ctx); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=3110198 AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not reach the gap after target COMMIT")
		}
		time.Sleep(10 * time.Millisecond)
	}
	probe, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Rollback(context.Background())
	var locked bool
	if err = probe.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(3110003)`).Scan(&locked); err != nil || locked {
		t.Fatal("physical target lock was lost between commits", err)
	}
	if _, err = db.Exec(ctx, "INSERT INTO "+relation+" VALUES(2,0)"); err != nil {
		t.Fatal(err)
	}
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	assertJob(t, f, j, jobs.Success, "")
	var value int
	if err = db.QueryRow(ctx, "SELECT value FROM "+relation+" WHERE id=2").Scan(&value); err != nil || value != 0 {
		t.Fatal("a row inserted after scope freeze entered the migration", err)
	}
}
