//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/jobs"
)

func partialResumeFixture(t *testing.T) (*queueFixture, *pgxpool.Pool, string, jobs.Job, jobs.Attempt) {
	t.Helper()
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(context.Background(), "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int CONSTRAINT stop_resume CHECK(id<>3 OR value<2)); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) n"); err != nil {
		t.Fatal(err)
	}
	j := enqueueBatch(t, f, batchConfig(schema))
	if _, err := queueWorker(t, f).ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := assertJob(t, f, j, jobs.Partial, "23514")
	return f, db, schema, j, a
}

func submitResumeReview(t *testing.T, f *queueFixture, b *browser, j jobs.Job, execution string) approval.ReviewView {
	t.Helper()
	w := b.request("POST", fmt.Sprintf("/api/v1/databases/%d/reviews", j.DatabaseID), map[string]string{"draft_id": fmt.Sprint(j.DraftID), "revision": fmt.Sprint(j.DraftRevision), "action_type": "resume", "target_execution_id": execution})
	status(t, w, 201)
	var out struct {
		Review approval.ReviewView `json:"review"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Review
}

func approveResumeReview(t *testing.T, f *queueFixture, r approval.ReviewView) {
	t.Helper()
	for _, leader := range []*browser{f.editor, f.viewer} {
		status(t, leader.request("POST", fmt.Sprintf("/api/v1/databases/%d/reviews/%d/approve", r.DatabaseID, r.ID), map[string]string{"note": "resume original approved scope"}), 200)
	}
}

func TestResumeReviewBindsHistoricalSQLAndExecution(t *testing.T) {
	f, _, _, j, a := partialResumeFixture(t)
	path := fmt.Sprintf("/api/v1/databases/%d/reviews", f.first)
	body := map[string]string{"draft_id": fmt.Sprint(j.DraftID), "revision": "1", "action_type": "resume", "target_execution_id": a.ExecutionID}
	status(t, f.viewer.request("POST", path, body), 403)
	missing := map[string]string{"draft_id": fmt.Sprint(j.DraftID), "revision": "1", "action_type": "resume"}
	status(t, f.editor.request("POST", path, missing), 400)
	missing["target_execution_id"] = strings.Repeat("0", 32)
	status(t, f.editor.request("POST", path, missing), 404)
	status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/reviews", f.second), body), 404)
	// Historical resume must use its frozen payload even if the draft editor has moved on.
	status(t, f.editor.request("PATCH", fmt.Sprintf("/api/v1/databases/%d/drafts/%d", f.first, j.DraftID), map[string]string{"revision": "1", "title": "new revision", "sql": "SELECT 99"}), 200)
	r := submitResumeReview(t, f, f.editor, j, a.ExecutionID)
	if r.TargetExecutionID == nil || *r.TargetExecutionID != a.ExecutionID || r.DraftRevision != 1 || r.SQLSnapshot == "SELECT 99" || !strings.HasPrefix(r.SQLSnapshot, "-- terserah-batch-v1 ") {
		t.Fatal("resume review changed the executed snapshot", r)
	}
	status(t, f.editor.request("POST", path, body), 409)
	if _, err := f.a.db.Exec(context.Background(), `UPDATE migration_reviews SET target_execution_id=$2 WHERE id=$1`, r.ID, strings.Repeat("1", 32)); err == nil {
		t.Fatal("review target could be replaced")
	}
	approveResumeReview(t, f, r)
	tx, err := f.a.db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = approval.EnsureApprovedReviewTx(context.Background(), tx, f.first, j.DraftID, 1, "resume", r.ID, &a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("1", 32)
	if _, err = approval.EnsureApprovedReviewTx(context.Background(), tx, f.first, j.DraftID, 1, "resume", r.ID, &other); err == nil {
		t.Fatal("approval allowed a different target execution")
	}
}
