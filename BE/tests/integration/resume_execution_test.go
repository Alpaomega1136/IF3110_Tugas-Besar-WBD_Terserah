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

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
)

func enqueueApprovedResume(t *testing.T, f *queueFixture, b *browser, r approval.ReviewView) jobs.Job {
	t.Helper()
	w := b.request("POST", fmt.Sprintf("/api/v1/databases/%d/jobs", r.DatabaseID), map[string]string{"review_id": fmt.Sprint(r.ID)})
	status(t, w, 202)
	var out struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Job
}

func TestApprovedResumeUsesFrozenScopeWithNewActorAndLostCommitResponse(t *testing.T) {
	f, db, schema, parent, original := partialResumeFixture(t)
	ctx := context.Background()
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := f.a.db.Exec(ctx, `UPDATE group_database_permissions SET access_level='edit' WHERE database_id=$1 AND group_id IN (SELECT group_id FROM user_groups WHERE user_id=$2)`, f.first, f.a.users["viewer"]); err != nil {
		t.Fatal(err)
	}
	status(t, f.editor.request("PATCH", fmt.Sprintf("/api/v1/databases/%d/drafts/%d", f.first, parent.DraftID), map[string]string{"revision": "1", "title": "changed editor draft", "sql": "SELECT 99"}), 200)
	next := queuedJob(t, f, f.first, "SELECT 1")
	r := submitResumeReview(t, f, f.viewer, parent, original.ExecutionID)
	approveResumeReview(t, f, r)
	child := enqueueApprovedResume(t, f, f.viewer, r)
	if child.RequestedBy != f.a.users["viewer"] {
		t.Fatal("resume lost new requester")
	}
	if _, err := db.Exec(ctx, "ALTER TABLE "+relation+" DROP CONSTRAINT stop_resume; UPDATE "+relation+" SET value=99 WHERE id=1; INSERT INTO "+relation+" VALUES(6,0)"); err != nil {
		t.Fatal(err)
	}
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "approved-resume-fault")
	port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, true, 3)
	if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
		t.Fatal(err)
	}
	w := queueWorker(t, f)
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, child, jobs.Unknown)
	jobPhase(t, f, next, jobs.Queued)
	recoveryDue(t, f, child.ID)
	if _, err := w.RecoverNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, child, jobs.Success)
	if current.AttemptCount != 1 || current.CommittedBatches != 3 || current.CommittedRows != 5 {
		t.Fatal("resume repeated or lost committed work", current)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, child.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatal(err)
	}
	var valid bool
	if err = db.QueryRow(ctx, "SELECT (SELECT value=99 FROM "+relation+" WHERE id=1) AND (SELECT value=0 FROM "+relation+" WHERE id=6) AND (SELECT count(*)=3 FROM "+relation+" WHERE id BETWEEN 3 AND 5 AND value=2)").Scan(&valid); err != nil || !valid {
		t.Fatal("resume changed frozen scope or repeated committed row", err)
	}
	if err = db.QueryRow(ctx, `SELECT count(*)=3 AND count(*) FILTER(WHERE batch_no=1 AND attempt_execution_id=$1)=1 AND count(*) FILTER(WHERE batch_no>1 AND attempt_execution_id=$2)=2 FROM _terserah.batch_checkpoints WHERE execution_id=$1`, original.ExecutionID, attempts[0].ExecutionID).Scan(&valid); err != nil || !valid {
		t.Fatal("resume lost original checkpoint chain", err)
	}
	wHistory := f.editor.request("GET", fmt.Sprintf("/api/v1/databases/%d/migrations/%d", f.first, *attempts[0].Version), nil)
	status(t, wHistory, 200)
	var history runner.History
	if err = json.Unmarshal(wHistory.Body.Bytes(), &history); err != nil || history.ActionType != "resume" || history.TargetExecutionID == nil || *history.TargetExecutionID != original.ExecutionID || history.JobID == nil || *history.JobID != child.ID || history.ExecutedBy != child.RequestedBy || history.SQL != r.SQLSnapshot {
		t.Fatal("resume history lost immutable action references", history, err)
	}
	status(t, f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil), 200)
	var state string
	if err = f.a.db.QueryRow(ctx, `SELECT status FROM migration_history WHERE execution_id=$1`, original.ExecutionID).Scan(&state); err != nil || state != "partial" {
		t.Fatal("successful resume overwrote original history", err)
	}
	jobPhase(t, f, parent, jobs.Resumed)
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, next, jobs.Success)
}

func TestManualResumeCanCreateAnotherApprovedRecoveryAfterPartialFailure(t *testing.T) {
	f, db, schema, parent, first := partialResumeFixture(t)
	ctx := context.Background()
	w := queueWorker(t, f)
	r := submitResumeReview(t, f, f.editor, parent, first.ExecutionID)
	approveResumeReview(t, f, r)
	child := enqueueApprovedResume(t, f, f.editor, r)
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, child, jobs.Partial)
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, child.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != jobs.Partial {
		t.Fatal("failed resume lost global partial state", err)
	}
	r = submitResumeReview(t, f, f.editor, child, attempts[0].ExecutionID)
	approveResumeReview(t, f, r)
	grandchild := enqueueApprovedResume(t, f, f.editor, r)
	if _, err = db.Exec(ctx, "ALTER TABLE "+pgx.Identifier{schema, "items"}.Sanitize()+" DROP CONSTRAINT stop_resume"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, grandchild, jobs.Success)
	if current.CommittedRows != 5 || current.CommittedBatches != 3 {
		t.Fatal("second manual recovery lost original progress")
	}
	jobPhase(t, f, parent, jobs.Resumed)
	jobPhase(t, f, child, jobs.Resumed)
	var count int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_runs WHERE sql=$1`, r.SQLSnapshot).Scan(&count); err != nil || count != 1 {
		t.Fatal("manual recovery created a new frozen scope", count, err)
	}
}

func TestApprovedResumeRechecksAccessAndApprovalBeforeSQL(t *testing.T) {
	for _, scenario := range []string{"access", "approval"} {
		t.Run(scenario, func(t *testing.T) {
			f, db, _, parent, original := partialResumeFixture(t)
			ctx := context.Background()
			r := submitResumeReview(t, f, f.editor, parent, original.ExecutionID)
			approveResumeReview(t, f, r)
			child := enqueueApprovedResume(t, f, f.editor, r)
			query := `UPDATE group_database_permissions SET access_level='view' WHERE database_id=$1`
			code := "access_revoked"
			if scenario == "approval" {
				query = `UPDATE groups SET leader_user_id=NULL WHERE id IN (SELECT group_id FROM group_database_permissions WHERE database_id=$1)`
				code = "approval_incomplete"
			}
			if _, err := f.a.db.Exec(ctx, query, f.first); err != nil {
				t.Fatal(err)
			}
			w := queueWorker(t, f)
			if _, err := w.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			current := jobPhase(t, f, child, jobs.Partial)
			_, attempts, err := f.service.Get(ctx, f.actor, f.first, child.ID)
			if err != nil || len(attempts) != 1 || attempts[0].Status != jobs.Failed || attempts[0].ErrorCode == nil || *attempts[0].ErrorCode != code || current.CommittedRows != 2 {
				t.Fatal("resume bypassed fresh authorization or lost partial gate", attempts, err)
			}
			var checkpoints int
			if err = db.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_checkpoints WHERE execution_id=$1`, original.ExecutionID).Scan(&checkpoints); err != nil || checkpoints != 1 {
				t.Fatal("revoked resume committed more SQL", err)
			}
		})
	}
}

func TestApprovedResumeRejectsCorruptCheckpointBeforeSQL(t *testing.T) {
	f, db, _, parent, original := partialResumeFixture(t)
	ctx := context.Background()
	r := submitResumeReview(t, f, f.editor, parent, original.ExecutionID)
	approveResumeReview(t, f, r)
	child := enqueueApprovedResume(t, f, f.editor, r)
	owner, err := database.Open(ctx, os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var signature string
	if err = owner.QueryRow(ctx, `SELECT signature FROM _terserah.batch_checkpoints WHERE execution_id=$1 AND batch_no=1`, original.ExecutionID).Scan(&signature); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE _terserah.batch_checkpoints SET signature='tampered' WHERE execution_id=$1 AND batch_no=1`, original.ExecutionID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := owner.Exec(ctx, `UPDATE _terserah.batch_checkpoints SET signature=$2 WHERE execution_id=$1 AND batch_no=1`, original.ExecutionID, signature); err != nil {
			t.Error(err)
		}
	}()
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, child, jobs.Partial)
	if current.ErrorCode == nil || *current.ErrorCode != "batch_evidence_invalid" || current.CommittedRows != 2 {
		t.Fatal("resume accepted corrupt checkpoint", current)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, child.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != jobs.Failed {
		t.Fatal("corrupt proof lost failed attempt", err)
	}
	var receipts int
	if err = db.QueryRow(ctx, `SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1`, attempts[0].ExecutionID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("corrupt proof produced success receipt", err)
	}
}
