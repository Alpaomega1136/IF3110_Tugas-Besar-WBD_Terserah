//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
)

func batchTarget(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	db, err := database.Open(context.Background(), os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	schema := fmt.Sprintf("batch_%d", time.Now().UnixNano())
	if _, err = db.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	return db, schema
}

func enqueueBatch(t *testing.T, f *queueFixture, c batch.Config) jobs.Job {
	t.Helper()
	base := fmt.Sprintf("/api/v1/databases/%d/drafts", f.first)
	w := f.editor.request("POST", base, map[string]any{"title": "batch update", "batch": c})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	canonical, err := batch.Encode(c)
	if err != nil || draft.SQL != canonical {
		t.Fatal("draft did not preserve approved batch configuration", err)
	}
	approveDraft(t, f.a, f.editor, f.first, draft.ID, 1)
	w = f.editor.request("POST", fmt.Sprintf("%s/%d/execute", base, draft.ID), map[string]string{"revision": "1"})
	status(t, w, 202)
	var response struct {
		Job jobs.Job `json:"job"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Job.ExecutionMode != "batch" {
		t.Fatal("batch not queued", err)
	}
	return response.Job
}

func TestBatchKeysetBoundariesAndRecoveryImages(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	for i, keys := range [][]int64{{}, {1}, {-9223372036854775808, -7, 0, 9223372036854775807}, {1, 3, 9, 100, 1000}} {
		table := fmt.Sprintf("items_%d", i)
		relation := pgx.Identifier{schema, table}.Sanitize()
		if _, err := db.Exec(context.Background(), "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,flag boolean NOT NULL DEFAULT false,amount numeric NOT NULL DEFAULT 1.25)"); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if _, err := db.Exec(context.Background(), "INSERT INTO "+relation+"(id) VALUES($1)", key); err != nil {
				t.Fatal(err)
			}
		}
		j := enqueueBatch(t, f, batch.Config{Schema: schema, Table: table, Key: "id", Size: 2, Set: map[string]json.RawMessage{"flag": json.RawMessage("true"), "amount": json.RawMessage("9007199254740993.125")}})
		if found, err := queueWorker(t, f).ProcessNext(context.Background()); err != nil || !found {
			t.Fatal("batch worker failed", err)
		}
		a := assertJob(t, f, j, jobs.Success, "")
		current, _, err := f.service.Get(context.Background(), f.actor, f.first, j.ID)
		if err != nil || current.CommittedRows != int64(len(keys)) || current.CommittedBatches != int64((len(keys)+1)/2) {
			t.Fatal("committed progress is wrong", current, err)
		}
		var updated, evidence, scope int
		if err = db.QueryRow(context.Background(), "SELECT count(*) FROM "+relation+" WHERE flag AND amount=9007199254740993.125").Scan(&updated); err != nil || updated != len(keys) {
			t.Fatal("rows skipped or precision changed", updated, err)
		}
		if err = db.QueryRow(context.Background(), `SELECT count(*) FROM _terserah.batch_rows WHERE execution_id=$1 AND before_data->>'flag'='false' AND after_data->>'flag'='true'`, a.ExecutionID).Scan(&evidence); err != nil || evidence != len(keys) {
			t.Fatal("recovery data missing", evidence, err)
		}
		if err = db.QueryRow(context.Background(), `SELECT count(*) FROM _terserah.batch_scope WHERE execution_id=$1`, a.ExecutionID).Scan(&scope); err != nil || scope != len(keys) {
			t.Fatal("scope missing", scope, err)
		}
	}
}

func TestBatchMiddleFailurePreservesCommittedPrefixAndBlocksSuccessor(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(context.Background(), "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int CHECK(id<>3 OR value<2)); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) AS n"); err != nil {
		t.Fatal(err)
	}
	j := enqueueBatch(t, f, batch.Config{Schema: schema, Table: "items", Key: "id", Size: 2, Set: map[string]json.RawMessage{"value": json.RawMessage("2")}})
	if found, err := queueWorker(t, f).ProcessNext(context.Background()); err != nil || !found {
		t.Fatal(err)
	}
	a := assertJob(t, f, j, jobs.Partial, "23514")
	current, _, err := f.service.Get(context.Background(), f.actor, f.first, j.ID)
	if err != nil || current.CommittedRows != 2 || current.CommittedBatches != 1 {
		t.Fatal("partial progress changed", current, err)
	}
	var updated, evidence, checkpoints, receipts int
	if err = db.QueryRow(context.Background(), "SELECT count(*) FROM "+relation+" WHERE value=2").Scan(&updated); err != nil || updated != 2 {
		t.Fatal("committed prefix was lost or failed batch leaked", err)
	}
	if err = db.QueryRow(context.Background(), `SELECT count(*) FROM _terserah.batch_rows WHERE execution_id=$1`, a.ExecutionID).Scan(&evidence); err != nil || evidence != 2 {
		t.Fatal("failed batch left recovery data", err)
	}
	if err = db.QueryRow(context.Background(), `SELECT count(*) FROM _terserah.batch_checkpoints WHERE execution_id=$1`, a.ExecutionID).Scan(&checkpoints); err != nil || checkpoints != 1 {
		t.Fatal("failed batch left checkpoint", err)
	}
	if err = db.QueryRow(context.Background(), `SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1`, a.ExecutionID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("partial migration has a success receipt", err)
	}
	next := queuedJob(t, f, f.first, "SELECT 1")
	if found, err := queueWorker(t, f).ProcessNext(context.Background()); found || err != nil {
		t.Fatal("partial target did not block successor", err)
	}
	nextState, _, err := f.service.Get(context.Background(), f.actor, f.first, next.ID)
	if err != nil || nextState.Status != jobs.Queued {
		t.Fatal("successor did not remain queued", err)
	}
	status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil), 200)
	assertJob(t, f, j, jobs.Partial, "reconciled_partial")
}

func TestBatchRejectsUnsafeKeysTablesAndSQLOverrides(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	base := fmt.Sprintf("/api/v1/databases/%d/drafts", f.first)
	c := batch.Config{Schema: schema, Table: "no_pk", Key: "id", Size: 1, Set: map[string]json.RawMessage{"value": json.RawMessage("2")}}
	status(t, f.editor.request("POST", base, map[string]any{"title": "ambiguous", "sql": "SELECT 1", "batch": c}), 400)
	for _, table := range []struct{ name, definition string }{{"no_pk", "id bigint,value int"}, {"text_pk", "id text PRIMARY KEY,value int"}, {"composite", "id bigint,value int,PRIMARY KEY(id,value)"}} {
		if _, err := db.Exec(context.Background(), "CREATE TABLE "+pgx.Identifier{schema, table.name}.Sanitize()+"("+table.definition+")"); err != nil {
			t.Fatal(err)
		}
		c.Table = table.name
		j := enqueueBatch(t, f, c)
		if _, err := queueWorker(t, f).ProcessNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertJob(t, f, j, jobs.Failed, "unsupported_batch_table")
	}
}

func TestBatchCheckpointFailureRollsBackDataAndImages(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) AS n"); err != nil {
		t.Fatal(err)
	}
	owner, err := database.Open(ctx, os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	constraint := pgx.Identifier{fmt.Sprintf("fault_%d", time.Now().UnixNano())}.Sanitize()
	if _, err = owner.Exec(ctx, "ALTER TABLE _terserah.batch_checkpoints ADD CONSTRAINT "+constraint+" CHECK(batch_no<>2) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := owner.Exec(ctx, "ALTER TABLE _terserah.batch_checkpoints DROP CONSTRAINT "+constraint); err != nil {
			t.Error(err)
		}
	}()
	j := enqueueBatch(t, f, batch.Config{Schema: schema, Table: "items", Key: "id", Size: 2, Set: map[string]json.RawMessage{"value": json.RawMessage("2")}})
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	a := assertJob(t, f, j, jobs.Partial, "23514")
	var changed, images int
	if err = db.QueryRow(ctx, "SELECT count(*) FROM "+relation+" WHERE value=2").Scan(&changed); err != nil || changed != 2 {
		t.Fatal("checkpoint failure committed data", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_rows WHERE execution_id=$1`, a.ExecutionID).Scan(&images); err != nil || images != 2 {
		t.Fatal("checkpoint failure committed recovery images", err)
	}
}
