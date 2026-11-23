//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"terserah/backend/internal/approval"
	"terserah/backend/internal/jobs"
)

func TestFailedJobAcceptsFreshApprovalWithoutResettingBudget(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	reviewID, d := f.approved(t, f.first, "SELECT 1/0")
	j, _, err := f.service.Enqueue(ctx, f.actor, f.first, reviewID, "")
	if err != nil {
		t.Fatal(err)
	}
	w := queueWorker(t, f)
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	first := assertJob(t, f, j, jobs.Failed, "22012")
	var groupID int64
	if err = f.a.db.QueryRow(ctx, `SELECT group_id FROM user_groups WHERE user_id=$1`, f.actor.ID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `INSERT INTO user_groups(user_id,group_id) VALUES($1,$2);`, f.a.users["viewer"], groupID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE groups SET leader_user_id=$1 WHERE id=$2`, f.a.users["viewer"], groupID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.service.RetryExecution(ctx, f.actor, f.first, first.ExecutionID, "up", nil); !errors.Is(err, approval.ErrNotApproved) {
		t.Fatal("stale approval was accepted", err)
	}
	path := fmt.Sprintf("/api/v1/databases/%d/reviews", f.first)
	freshReview := func() int64 {
		t.Helper()
		response := f.editor.request("POST", path, map[string]string{"draft_id": fmt.Sprint(d.ID), "revision": "1", "action_type": "up"})
		status(t, response, 201)
		var out struct {
			Review approval.ReviewView `json:"review"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, a := range out.Review.Approvals {
			if seen[a.LeaderUsername] {
				continue
			}
			seen[a.LeaderUsername] = true
			b := f.a.browser()
			b.login(t, a.LeaderUsername)
			status(t, b.request("POST", fmt.Sprintf("%s/%d/approve", path, out.Review.ID), map[string]string{"note": "new leader reviewed original SQL"}), 200)
		}
		return out.Review.ID
	}
	fresh := freshReview()
	current, replayed, err := f.service.Enqueue(ctx, f.actor, f.first, fresh, "")
	if err != nil || replayed || current.ID != j.ID || current.ReviewID != reviewID || current.AuthorizationReviewID == nil || *current.AuthorizationReviewID != fresh || current.AttemptCount != 1 || current.MaxAttempts != j.MaxAttempts {
		t.Fatal("retry changed job identity or budget", current, err)
	}
	replay, again, err := f.service.Enqueue(ctx, f.actor, f.first, fresh, "")
	if err != nil || !again || replay.ID != j.ID {
		t.Fatal("approval replay duplicated work", err)
	}
	status(t, f.editor.request("POST", path, map[string]string{"draft_id": fmt.Sprint(d.ID), "revision": "1"}), 409)
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil || current.Status != jobs.Failed || len(attempts) != 2 || attempts[0].ExecutionID != first.ExecutionID {
		t.Fatal("freshly approved retry did not execute original SQL", current, attempts, err)
	}
	var prior string
	if err = f.a.db.QueryRow(ctx, `SELECT retry_of_execution_id FROM migration_history WHERE execution_id=$1`, attempts[1].ExecutionID).Scan(&prior); err != nil || prior != first.ExecutionID {
		t.Fatal("retry lineage lost", err)
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_retry_authorizations SET review_id=$1 WHERE job_id=$2`, reviewID, j.ID); err == nil {
		t.Fatal("retry approval could be replaced")
	}
	if _, err = f.a.db.Exec(ctx, `UPDATE migration_jobs SET status='unknown',finished_at=NULL,available_at=clock_timestamp() WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	recovery, err := f.service.RecoveryCandidate(ctx)
	if err != nil || recovery == nil || recovery.Job.AuthorizationReviewID == nil || *recovery.Job.AuthorizationReviewID != fresh {
		t.Fatal("recovery lost the execution approval", err)
	}
	if _, err = w.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	last := freshReview()
	if _, _, err = f.service.Enqueue(ctx, f.actor, f.first, last, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	exhausted := freshReview()
	if _, _, err = f.service.Enqueue(ctx, f.actor, f.first, exhausted, ""); !errors.Is(err, jobs.ErrConflict) {
		t.Fatal("reapproval reset the bounded retry budget", err)
	}
	var oldDecisions int
	if err = f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_review_approvals WHERE review_id=$1 AND decision='approved'`, reviewID).Scan(&oldDecisions); err != nil || oldDecisions != 2 {
		t.Fatal("old approvals were overwritten", err)
	}
}

func TestQueuedAndRunningJobsCannotReplaceTheirApproval(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	review, d := f.approved(t, f.first, "SELECT 1")
	if _, _, err := f.service.Enqueue(ctx, f.actor, f.first, review, ""); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/databases/%d/reviews", f.first)
	body := map[string]string{"draft_id": fmt.Sprint(d.ID), "revision": "1"}
	status(t, f.editor.request("POST", path, body), 409)
	if _, err := f.service.Claim(ctx, "active-review-test", time.Minute); err != nil {
		t.Fatal(err)
	}
	status(t, f.editor.request("POST", path, body), 409)
	var state string
	if err := f.a.db.QueryRow(ctx, `SELECT status FROM migration_reviews WHERE id=$1`, review).Scan(&state); err != nil || state != "queued" {
		t.Fatal("active job approval changed", state, err)
	}
}
