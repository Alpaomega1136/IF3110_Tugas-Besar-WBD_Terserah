//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
)

type queueFixture struct {
	a              *m1App
	service        *jobs.Service
	editor, viewer *browser
	first, second  int64
	actor          auth.Principal
}

func newQueueFixture(t *testing.T) *queueFixture {
	t.Helper()
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	first := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "queue-first"))
	second := createTarget(t, admin, targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "queue-second"))
	grant(t, a, "editor", first.ID, "edit")
	grant(t, a, "editor", second.ID, "edit")
	grant(t, a, "viewer", first.ID, "view")
	editor, viewer := a.browser(), a.browser()
	editor.login(t, "editor")
	viewer.login(t, "viewer")
	return &queueFixture{a: a, service: jobs.New(a.db), editor: editor, viewer: viewer,
		first: first.ID, second: second.ID, actor: auth.Principal{ID: a.users["editor"], Username: "editor"}}
}

func (f *queueFixture) draft(t *testing.T, dbID int64, sql string) runner.Draft {
	t.Helper()
	w := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", dbID), map[string]string{"title": "queued migration", "sql": sql})
	status(t, w, 201)
	var d runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *queueFixture) approved(t *testing.T, dbID int64, sql string) (int64, runner.Draft) {
	t.Helper()
	d := f.draft(t, dbID, sql)
	approveDraft(t, f.a, f.editor, dbID, d.ID, d.Revision)
	var reviewID int64
	if err := f.a.db.QueryRow(context.Background(), `SELECT id FROM migration_reviews WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3 AND status='approved'`, dbID, d.ID, d.Revision).Scan(&reviewID); err != nil {
		t.Fatal(err)
	}
	return reviewID, d
}

func TestJobsAPI(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	base := fmt.Sprintf("/api/v1/databases/%d/jobs", f.first)
	admin := f.a.browser()
	admin.login(t, "admin")
	status(t, f.a.browser().request("GET", base, nil), 401)
	status(t, admin.request("GET", base, nil), 403)
	status(t, f.viewer.request("GET", base, nil), 200)
	status(t, f.viewer.request("POST", base, map[string]string{"review_id": "1"}), 403)

	d := f.draft(t, f.first, "SELECT 'private-job-sql'")
	path := fmt.Sprintf("/api/v1/databases/%d/reviews", f.first)
	w := f.editor.request("POST", path, map[string]string{"draft_id": fmt.Sprint(d.ID), "revision": "1"})
	status(t, w, 201)
	var review struct {
		Review approval.ReviewView `json:"review"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	in := map[string]string{"review_id": fmt.Sprint(review.Review.ID)}
	status(t, f.editor.request("POST", base, in), 409)
	decision := fmt.Sprintf("%s/%d/approve", path, review.Review.ID)
	status(t, f.editor.request("POST", decision, map[string]string{"note": "editor approved"}), 200)
	status(t, f.editor.request("POST", base, in), 409)
	status(t, f.viewer.request("POST", decision, map[string]string{"note": "viewer approved"}), 200)
	status(t, f.editor.request("POST", base, map[string]any{"review_id": review.Review.ID}), 400)
	status(t, f.editor.request("POST", base, map[string]string{"review_id": in["review_id"], "sql": "SELECT 99"}), 400)
	csrf := f.editor.csrf
	f.editor.csrf = "invalid"
	status(t, f.editor.request("POST", base, in), 403)
	f.editor.csrf = csrf

	w = f.editor.request("POST", base, in)
	status(t, w, 202)
	var queued struct {
		Job      jobs.Job `json:"job"`
		Replayed bool     `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil || queued.Job.ID < 1 || queued.Job.Status != jobs.Queued || queued.Replayed || queued.Job.AttemptCount != 0 {
		t.Fatal("invalid queue response")
	}
	jobPath := fmt.Sprintf("%s/%d", base, queued.Job.ID)
	if w.Header().Get("Location") != jobPath || strings.Contains(w.Body.String(), "private-job-sql") || strings.Contains(w.Body.String(), "lease_token") {
		t.Fatal("job API location or payload privacy failed")
	}
	w = f.editor.request("POST", base, in)
	status(t, w, 200)
	var replay struct {
		Job      jobs.Job `json:"job"`
		Replayed bool     `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.Job.ID != queued.Job.ID || !replay.Replayed {
		t.Fatal("duplicate submission created different work")
	}
	var reviewStatus string
	if err := f.a.db.QueryRow(ctx, `SELECT status FROM migration_reviews WHERE id=$1`, review.Review.ID).Scan(&reviewStatus); err != nil || reviewStatus != "queued" {
		t.Fatal("approval and enqueue were not linked")
	}
	status(t, f.viewer.request("GET", jobPath, nil), 200)
	status(t, f.editor.request("GET", fmt.Sprintf("/api/v1/databases/%d/jobs/%d", f.second, queued.Job.ID), nil), 404)
	status(t, f.viewer.request("GET", fmt.Sprintf("/api/v1/databases/%d/jobs", f.second), nil), 403)
	status(t, f.editor.request("GET", base+"/999999", nil), 404)
	for _, query := range []string{"?limit=0", "?limit=101", "?before=0", "?before=-1", "?before=invalid", "?before=9223372036854775808"} {
		status(t, f.editor.request("GET", base+query, nil), 400)
	}
	// A separate pool/service has no shared Go state with the submitting instance.
	reopened, err := pgxpool.NewWithConfig(ctx, f.a.db.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	read, attempts, err := jobs.New(reopened).Get(ctx, f.actor, f.first, queued.Job.ID)
	if err != nil || read.ID != queued.Job.ID || len(attempts) != 0 {
		t.Fatal("job did not survive service reconstruction")
	}
	// Execute now reuses the same queue entry; it cannot execute queued SQL inline.
	status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", f.first, d.ID), map[string]string{"revision": "1"}), 200)
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET sql_snapshot='SELECT 99' WHERE id=$1`, queued.Job.ID); err == nil {
		t.Fatal("job SQL snapshot was mutable")
	}
	status(t, f.editor.request("PATCH", fmt.Sprintf("/api/v1/databases/%d/drafts/%d", f.first, d.ID), map[string]string{"title": "new draft revision", "sql": "SELECT 88", "revision": "1"}), 200)
	claim, err := f.service.Claim(ctx, "api-test-worker", time.Minute)
	if err != nil || claim == nil || claim.SQL != "SELECT 'private-job-sql'" || claim.Job.DraftRevision != 1 {
		t.Fatal("claim used mutable draft SQL instead of the approved snapshot")
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_job_attempts SET sql_snapshot='SELECT 99' WHERE execution_id=$1`, claim.ExecutionID); err == nil {
		t.Fatal("attempt SQL snapshot was mutable")
	}
	status(t, f.viewer.request("GET", jobPath, nil), 200)
	if strings.Contains(f.viewer.request("GET", jobPath, nil).Body.String(), "private-job-sql") {
		t.Fatal("worker payload leaked through job detail")
	}
	otherReview, _ := f.approved(t, f.first, "SELECT 2")
	newer, _, err := f.service.Enqueue(ctx, f.actor, f.first, otherReview, "")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Jobs []jobs.Job `json:"jobs"`
		More bool       `json:"has_more"`
		Next *string    `json:"next_before"`
	}
	w = f.viewer.request("GET", base+"?limit=1", nil)
	status(t, w, 200)
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Jobs) != 1 || page.Jobs[0].ID != newer.ID || !page.More || page.Next == nil {
		t.Fatal("job pagination failed")
	}
	w = f.viewer.request("GET", base+"?limit=1&before="+*page.Next, nil)
	status(t, w, 200)
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Jobs) != 1 || page.Jobs[0].ID != queued.Job.ID || page.More || page.Next != nil {
		t.Fatal("job cursor skipped or repeated a job")
	}
}

func TestJobsConcurrentClaimsAndFencing(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	parent := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	for _, target := range []int64{f.first, f.first, f.first, f.second, f.second} {
		reviewID, _ := f.approved(t, target, "SELECT 1")
		if _, _, err := f.service.Enqueue(ctx, f.actor, target, reviewID, parent); err != nil {
			t.Fatal(err)
		}
	}
	const consumers = 8
	claims := make([]*jobs.Claim, consumers)
	errorsOut := make([]error, consumers)
	var wg sync.WaitGroup
	for i := range claims {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claims[i], errorsOut[i] = f.service.Claim(ctx, fmt.Sprintf("worker-%d", i), time.Minute)
		}(i)
	}
	wg.Wait()
	byTarget := map[int64]*jobs.Claim{}
	for i, claim := range claims {
		if errorsOut[i] != nil {
			t.Fatal(errorsOut[i])
		}
		if claim == nil {
			continue
		}
		if byTarget[claim.Job.DatabaseID] != nil || claim.Job.AttemptCount != 1 || claim.TraceParent != parent {
			t.Fatal("duplicate dispatch or lost trace context")
		}
		byTarget[claim.Job.DatabaseID] = claim
	}
	if len(byTarget) != 2 {
		t.Fatal("different databases were not independently dispatchable")
	}
	first := byTarget[f.first]
	wrong := *first
	wrong.LeaseToken = strings.Repeat("0", 64)
	if err := f.service.Heartbeat(ctx, &wrong, time.Minute); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("wrong lease owner renewed work")
	}
	if err := f.service.Finish(ctx, &wrong, jobs.Outcome{Status: jobs.Success}); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("wrong lease owner finalized work")
	}
	if err := f.service.Heartbeat(ctx, first, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Finish(ctx, first, jobs.Outcome{Status: jobs.Success}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Finish(ctx, first, jobs.Outcome{Status: jobs.Success}); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("completed attempt was finalized twice")
	}
	other := byTarget[f.second]
	if err := f.service.Finish(ctx, other, jobs.Outcome{Status: jobs.Unknown, ErrorCode: "commit_unknown"}); err != nil {
		t.Fatal(err)
	}
	next, err := f.service.Claim(ctx, "next-worker", time.Minute)
	if err != nil || next == nil || next.Job.DatabaseID != f.first || next.Job.ID == first.Job.ID {
		t.Fatal("known completion did not release the database queue")
	}
	// An expired heartbeat is not evidence that the old transaction rolled back.
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, next.Job.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Heartbeat(ctx, next, time.Minute); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("expired lease was resurrected")
	}
	if err = f.service.Finish(ctx, next, jobs.Outcome{Status: jobs.Failed, ErrorCode: "execution_failed"}); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("expired worker overwrote an uncertain outcome")
	}
	if claim, err := f.service.Claim(ctx, "replacement-worker", time.Minute); err != nil || claim != nil {
		t.Fatal("expired or unknown work was automatically retried")
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, first.Job.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != jobs.Success || attempts[0].ExecutionID != first.ExecutionID {
		t.Fatal("attempt evidence was lost")
	}
}

func TestJobsAtomicEnqueueAndDuplicateRequests(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	reviewID, _ := f.approved(t, f.first, "SELECT 1")
	_, err := f.a.db.Exec(ctx, `CREATE FUNCTION reject_queue_test() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.status='queued' THEN RAISE EXCEPTION 'injected queue failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_queue_test BEFORE UPDATE ON migration_reviews FOR EACH ROW EXECUTE FUNCTION reject_queue_test()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.service.Enqueue(ctx, f.actor, f.first, reviewID, ""); err == nil {
		t.Fatal("injected approval update failure was ignored")
	}
	var count int
	var state string
	if err = f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("job persisted without atomic approval update")
	}
	if err = f.a.db.QueryRow(ctx, `SELECT status FROM migration_reviews WHERE id=$1`, reviewID).Scan(&state); err != nil || state != "approved" {
		t.Fatal("failed enqueue changed the review")
	}
	if _, err = f.a.db.Exec(ctx, `DROP TRIGGER reject_queue_test ON migration_reviews; DROP FUNCTION reject_queue_test()`); err != nil {
		t.Fatal(err)
	}
	const requests = 6
	ids := make([]int64, requests)
	failures := make([]error, requests)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			job, _, err := f.service.Enqueue(ctx, f.actor, f.first, reviewID, "")
			ids[i], failures[i] = job.ID, err
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if failures[i] != nil || ids[i] < 1 || ids[i] != ids[0] {
			t.Fatalf("concurrent enqueue mismatch: id=%d err=%v", ids[i], failures[i])
		}
	}
	if err = f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate requests created duplicate jobs")
	}
	_, err = f.a.db.Exec(ctx, `CREATE FUNCTION reject_claim_test() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected claim failure'; END $$;
 CREATE TRIGGER reject_claim_test BEFORE INSERT ON migration_job_attempts FOR EACH ROW EXECUTE FUNCTION reject_claim_test()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Claim(ctx, "worker-test", time.Minute); err == nil {
		t.Fatal("injected attempt insert failure was ignored")
	}
	job, attempts, err := f.service.Get(ctx, f.actor, f.first, ids[0])
	if err != nil || job.Status != jobs.Queued || job.AttemptCount != 0 || len(attempts) != 0 {
		t.Fatal("failed claim left half-persisted ownership")
	}
}

func TestJobsRejectStaleOrExecutedReviews(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	reviewID, d := f.approved(t, f.first, "SELECT 1")
	status(t, f.editor.request("PATCH", fmt.Sprintf("/api/v1/databases/%d/drafts/%d", f.first, d.ID),
		map[string]string{"title": "changed", "sql": "SELECT 2", "revision": "1"}), 200)
	if _, _, err := f.service.Enqueue(ctx, f.actor, f.first, reviewID, ""); !errors.Is(err, jobs.ErrConflict) {
		t.Fatal("stale revision was queued")
	}
	reviewID, _ = f.approved(t, f.first, "SELECT 3")
	grant(t, f.a, "outsider", f.first, "view")
	if _, _, err := f.service.Enqueue(ctx, f.actor, f.first, reviewID, ""); !errors.Is(err, approval.ErrNotApproved) {
		t.Fatal("a newly accessing group was omitted from approval")
	}
	reviewID, d = f.approved(t, f.first, "SELECT 4")
	if _, err := f.a.db.Exec(ctx, `UPDATE users SET is_active=false WHERE id=$1`, f.actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Enqueue(ctx, f.actor, f.first, reviewID, ""); !errors.Is(err, jobs.ErrForbidden) {
		t.Fatal("inactive actor queued work with an old principal")
	}
	if _, err := f.a.db.Exec(ctx, `UPDATE users SET is_active=true WHERE id=$1`, f.actor.ID); err != nil {
		t.Fatal(err)
	}
	// Represent an existing Milestone 1 execution, before the queue was introduced.
	if _, err := f.a.db.Exec(ctx, `INSERT INTO migration_history(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,receipt_version)
 VALUES($1,$2,1,1,'SELECT 4',$3,'success','00000000000000000000000000000001',1)`, f.first, d.ID, f.actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Enqueue(ctx, f.actor, f.first, reviewID, ""); !errors.Is(err, jobs.ErrConflict) {
		t.Fatal("already executed revision was queued again")
	}
	var unsupported int64
	if err := f.a.db.QueryRow(ctx, `INSERT INTO migration_reviews(database_id,draft_id,draft_revision,action_type,target_execution_id,sql_snapshot,status,requested_by)
 VALUES($1,$2,1,'rollback','prior-execution','SELECT 4','approved',$3) RETURNING id`, f.first, d.ID, f.actor.ID).Scan(&unsupported); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Enqueue(ctx, f.actor, f.first, unsupported, ""); !errors.Is(err, jobs.ErrUnsupported) {
		t.Fatal("a rollback review was incorrectly queued as SQL up")
	}
}
