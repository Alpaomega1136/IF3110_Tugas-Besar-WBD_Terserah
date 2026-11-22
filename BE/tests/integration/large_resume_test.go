//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
)

func TestLargeBatchApprovedResumePreservesCommittedPrefixAndFrozenScope(t *testing.T) {
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int CONSTRAINT stop_large CHECK(id<=5000 OR value<2),payload text); INSERT INTO "+relation+" SELECT n,0,repeat(md5(n::text),8) FROM generate_series(1,12000) n"); err != nil {
		t.Fatal(err)
	}
	parent := enqueueBatch(t, f, batch.Config{Schema: schema, Table: "items", Key: "id", Size: 5000, Set: map[string]json.RawMessage{"value": json.RawMessage("2")}})
	w := queueWorker(t, f)
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	original := assertJob(t, f, parent, jobs.Partial, "23514")
	current := jobPhase(t, f, parent, jobs.Partial)
	if current.CommittedRows != 5000 || current.CommittedBatches != 1 {
		t.Fatal("failed batch changed committed prefix")
	}
	r := submitResumeReview(t, f, f.editor, parent, original.ExecutionID)
	approveResumeReview(t, f, r)
	child := enqueueApprovedResume(t, f, f.editor, r)
	if _, err := db.Exec(ctx, "ALTER TABLE "+relation+" DROP CONSTRAINT stop_large; UPDATE "+relation+" SET value=99 WHERE id=1; INSERT INTO "+relation+" VALUES(12001,0,'outside-frozen-scope')"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	completed := jobPhase(t, f, child, jobs.Success)
	if completed.CommittedRows != 12000 || completed.CommittedBatches != 3 {
		t.Fatal("resume did not complete the original scope")
	}
	jobPhase(t, f, parent, jobs.Resumed)
	var valid bool
	if err := db.QueryRow(ctx, "SELECT count(*) FILTER(WHERE value=2)=11999 AND bool_and(CASE WHEN id=1 THEN value=99 WHEN id=12001 THEN value=0 ELSE value=2 END) FROM "+relation).Scan(&valid); err != nil || !valid {
		t.Fatal("resume repeated committed rows or included new rows", err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*)=12000 AND max(batch_no)=3 FROM _terserah.batch_rows WHERE execution_id=$1`, original.ExecutionID).Scan(&valid); err != nil || !valid {
		t.Fatal("large recovery images missing", err)
	}
	if err := f.a.db.QueryRow(ctx, `SELECT status='partial' FROM migration_history WHERE execution_id=$1`, original.ExecutionID).Scan(&valid); err != nil || !valid {
		t.Fatal("original partial history overwritten", err)
	}
}
