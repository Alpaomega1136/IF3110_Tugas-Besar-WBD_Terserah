//go:build integration && batchdemo

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
	"terserah/backend/internal/worker"
)

func TestBatchPerformanceComparison(t *testing.T) {
	const rowCount = 120_000
	const batchSize = 5_000
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	var pgVersion string
	if err := db.QueryRow(ctx, "SHOW server_version").Scan(&pgVersion); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"single_update", "batch_update"} {
		relation := pgx.Identifier{schema, table}.Sanitize()
		// The expression index makes each row update do the same hashing work in both runs.
		_, err := db.Exec(ctx, fmt.Sprintf(`CREATE TABLE %[1]s (id bigint PRIMARY KEY, value int, payload text);
			INSERT INTO %[1]s SELECT n, 0, repeat(md5(n::text), 16) FROM generate_series(1, %d) n;
			CREATE INDEX ON %[1]s (md5(repeat(payload, 8) || value::text));`, relation, rowCount))
		if err != nil {
			t.Fatal(err)
		}
	}

	single := pgx.Identifier{schema, "single_update"}.Sanitize()
	started := time.Now()
	updated, err := db.Exec(ctx, "UPDATE "+single+" SET value=1")
	singleDuration := time.Since(started)
	if err != nil || updated.RowsAffected() != rowCount {
		t.Fatalf("single UPDATE affected %d rows: %v", updated.RowsAffected(), err)
	}

	job := enqueueBatch(t, f, batch.Config{Schema: schema, Table: "batch_update", Key: "id", Size: batchSize,
		Set: map[string]json.RawMessage{"value": json.RawMessage("1")}})
	w, err := worker.New(f.service, runner.New(f.a.db, f.a.targets, f.a.key), worker.Options{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	found, err := w.ProcessNext(ctx)
	batchDuration := time.Since(started)
	if err != nil || !found {
		t.Fatalf("batch worker did not process the job: %v", err)
	}
	result := jobPhase(t, f, job, jobs.Success)
	if result.CommittedRows != rowCount || result.CommittedBatches != rowCount/batchSize {
		t.Fatalf("batch committed %d rows in %d batches", result.CommittedRows, result.CommittedBatches)
	}
	relation := pgx.Identifier{schema, "batch_update"}.Sanitize()
	var matching int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM "+relation+" WHERE value=1").Scan(&matching); err != nil || matching != rowCount {
		t.Fatalf("batch changed %d rows: %v", matching, err)
	}
	t.Logf("PostgreSQL %s | %d rows | single UPDATE %v | batch worker %v | %d batches of %d",
		pgVersion, rowCount, singleDuration.Round(time.Millisecond), batchDuration.Round(time.Millisecond), result.CommittedBatches, batchSize)
}
