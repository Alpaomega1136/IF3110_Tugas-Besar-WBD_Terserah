//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
)

func compensationFixture(t *testing.T, partial bool, before ...int64) (*queueFixture, *pgxpool.Pool, batch.Config, jobs.Job, jobs.Attempt) {
	t.Helper()
	f := newQueueFixture(t)
	db, schema := batchTarget(t)
	ctx := context.Background()
	c := batchConfig(schema)
	seed := int64(0)
	if len(before) > 0 {
		seed = before[0]
	}
	constraint := ""
	if partial {
		constraint = " CONSTRAINT stop_comp_up CHECK(id<>3 OR value<2)"
	}
	if _, err := db.Exec(ctx, `CREATE TABLE `+c.Relation()+`(id bigint PRIMARY KEY,value int`+constraint+`); INSERT INTO `+c.Relation()+` SELECT n,`+fmt.Sprint(seed)+` FROM generate_series(1,5)n`); err != nil {
		t.Fatal(err)
	}
	down, _ := batch.DownSQL(c)
	w := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", f.first), map[string]any{"title": "Reversible batch", "batch": c, "sql_down": down})
	status(t, w, 201)
	var d runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, f.a, f.editor, f.first, d.ID, d.Revision)
	j, _, err := f.service.EnqueueDraft(ctx, f.actor, f.first, d.ID, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	state, code := jobs.Success, ""
	if partial {
		state, code = jobs.Partial, "23514"
	}
	attempt := assertJob(t, f, j, state, code)
	return f, db, c, j, attempt
}

func TestCompensationCanContinueWithNewEditorAfterAccessRevocation(t *testing.T) {
	f, db, c, source, attempt := compensationFixture(t, false)
	ctx := context.Background()
	first := enqueueCompensation(t, f, source, attempt.ExecutionID, c)
	if _, err := f.a.db.Exec(ctx, `UPDATE group_database_permissions SET access_level='view' WHERE database_id=$1 AND group_id IN (SELECT group_id FROM user_groups WHERE user_id=$2)`, f.first, f.actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, first, jobs.PartiallyReverted)
	if current.ErrorCode == nil || *current.ErrorCode != "access_revoked" || current.CommittedRows != 0 {
		t.Fatal("revoked actor executed cancellation", current)
	}
	if _, err := f.a.db.Exec(ctx, `UPDATE group_database_permissions SET access_level='edit' WHERE database_id=$1 AND group_id IN (SELECT group_id FROM user_groups WHERE user_id=$2)`, f.first, f.a.users["viewer"]); err != nil {
		t.Fatal(err)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := *f
	next.editor = f.viewer
	next.actor = auth.Principal{ID: f.a.users["viewer"], Username: "viewer"}
	child := enqueueCompensation(t, &next, first, attempts[0].ExecutionID, c)
	if child.RequestedBy != next.actor.ID {
		t.Fatal("continuation lost its actual requesting editor")
	}
	if _, err = queueWorker(t, &next).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, &next, child, jobs.Success)
	var restored bool
	if err = db.QueryRow(ctx, `SELECT bool_and(value=0) FROM `+c.Relation()).Scan(&restored); err != nil || !restored {
		t.Fatal("new approved editor could not restore the original scope", err)
	}
}

func TestCompensationCannotCancelAnOlderActiveMigration(t *testing.T) {
	f, _, c, source, attempt := compensationFixture(t, false)
	ctx := context.Background()
	next := queuedJob(t, f, f.first, "SELECT 1")
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, next, jobs.Success)
	current, _, err := f.service.Get(ctx, f.actor, f.first, source.ID)
	if err != nil || current.CanCompensate {
		t.Fatal("older active migration was offered for cancellation", err)
	}
	down, _ := batch.DownSQL(c)
	setujuiAksiMigration(t, f.a, f.editor, f.first, source.DraftID, source.DraftRevision, "compensate", attempt.ExecutionID, down)
	if _, _, err = f.service.EnqueueOperation(ctx, f.actor, f.first, "compensate", attempt.ExecutionID); !errors.Is(err, jobs.ErrConflict) {
		t.Fatal("nonlinear cancellation was enqueued", err)
	}
	jobPhase(t, f, source, jobs.Success)
}

func TestCompensationCanBeReconciledManuallyBeforeApprovedContinuation(t *testing.T) {
	f, _, c, source, attempt := compensationFixture(t, false)
	ctx := context.Background()
	j := enqueueCompensation(t, f, source, attempt.ExecutionID, c)
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "manual-compensation-proxy")
	port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, true, 3)
	if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
		t.Fatal(err)
	}
	worker := queueWorker(t, f)
	if _, err := worker.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.Unknown)
	w := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil)
	status(t, w, 200)
	current := jobPhase(t, f, j, jobs.PartiallyReverted)
	if current.CommittedBatches != 2 || current.CommittedRows != 3 {
		t.Fatal("manual reconciliation lost restored progress", current)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := enqueueCompensation(t, f, j, attempts[0].ExecutionID, c)
	if _, err = worker.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, child, jobs.Success)
}

func TestBatchDownPreviewRequiresEditAndNeverExecutesSQL(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	c := batchConfig("nonexistent_schema")
	up, _ := batch.Encode(c)
	expected, _ := batch.DownSQL(c)
	path := fmt.Sprintf("/api/v1/databases/%d/drafts/batch-down-preview", f.first)
	body := map[string]string{"sql": up}
	status(t, f.viewer.request("POST", path, body), 403)
	status(t, f.editor.request("POST", path, map[string]string{"sql": "SELECT 1"}), 400)
	w := f.editor.request("POST", path, body)
	status(t, w, 200)
	var out struct {
		SQL string `json:"sql_down"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.SQL != expected {
		t.Fatal("preview did not use the canonical immutable Down template", err)
	}
	var count int
	if err := f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_history`).Scan(&count); err != nil || count != 0 {
		t.Fatal("preview created an execution attempt", err)
	}
}

func TestCompensationRejectsLossyColumnTypeChanges(t *testing.T) {
	f, db, c, original, attempt := compensationFixture(t, false, 2147483647)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE `+c.Relation()+` ALTER COLUMN value TYPE real`); err != nil {
		t.Fatal(err)
	}
	j := enqueueCompensation(t, f, original, attempt.ExecutionID, c)
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, j, jobs.PartiallyReverted)
	if current.CommittedRows != 0 || current.CommittedBatches != 0 {
		t.Fatal("lossy restoration was committed", current)
	}
	var intact bool
	if err := db.QueryRow(ctx, `SELECT bool_and(value=2) FROM `+c.Relation()).Scan(&intact); err != nil || !intact {
		t.Fatal("failed reverse transaction retained rounded values", err)
	}
}

func TestCompensationRecoversLostCommitResponses(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		after         bool
		cut, attempts int
	}{
		{"before_middle", false, 3, 2}, {"after_middle", true, 3, 2}, {"after_final", true, 4, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, db, c, original, attempt := compensationFixture(t, false)
			ctx := context.Background()
			j := enqueueCompensation(t, f, original, attempt.ExecutionID, c)
			input := targetInput(t, "TEST_TARGET_DATABASE_URL", "compensation-proxy")
			port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, scenario.after, scenario.cut)
			if _, err := f.a.db.Exec(ctx, `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
				t.Fatal(err)
			}
			worker := queueWorker(t, f)
			if _, err := worker.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			jobPhase(t, f, j, jobs.Unknown)
			recoveryDue(t, f, j.ID)
			if _, err := worker.RecoverNext(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario.attempts == 2 {
				jobPhase(t, f, j, jobs.RetryWait)
				recoveryDue(t, f, j.ID)
				if _, err := worker.ProcessNext(ctx); err != nil {
					t.Fatal(err)
				}
			}
			current := jobPhase(t, f, j, jobs.Success)
			if current.AttemptCount != scenario.attempts || current.CommittedBatches != 3 || current.CommittedRows != 5 {
				t.Fatal("recovery repeated or lost compensation", current)
			}
			var intact bool
			if err := db.QueryRow(ctx, `SELECT bool_and(value=0) FROM `+c.Relation()).Scan(&intact); err != nil || !intact {
				t.Fatal("recovery did not restore data", err)
			}
			var count int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM _terserah.compensation_checkpoints WHERE execution_id IN (SELECT execution_id FROM _terserah.compensation_runs WHERE source_execution_id=$1)`, attempt.ExecutionID).Scan(&count); err != nil || count != 3 {
				t.Fatal("duplicate reverse checkpoints", count, err)
			}
		})
	}
}

func TestCompensationTamperedCheckpointCannotBeResumed(t *testing.T) {
	f, db, c, original, attempt := compensationFixture(t, false)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `UPDATE `+c.Relation()+` SET value=99 WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	j := enqueueCompensation(t, f, original, attempt.ExecutionID, c)
	worker := queueWorker(t, f)
	if _, err := worker.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, j, jobs.PartiallyReverted)
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pgx.Connect(ctx, os.Getenv("TEST_TARGET_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	var root, signature string
	if err = owner.QueryRow(ctx, `SELECT p.execution_id,p.signature FROM _terserah.compensation_checkpoints p JOIN _terserah.compensation_runs r ON r.execution_id=p.execution_id WHERE r.source_execution_id=$1 AND p.batch_no=1`, attempt.ExecutionID).Scan(&root, &signature); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE _terserah.compensation_checkpoints SET signature='tampered' WHERE execution_id=$1`, root); err != nil {
		t.Fatal(err)
	}
	defer owner.Exec(ctx, `UPDATE _terserah.compensation_checkpoints SET signature=$2 WHERE execution_id=$1`, root, signature)
	child := enqueueCompensation(t, f, j, attempts[0].ExecutionID, c)
	if _, err = worker.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, child, jobs.PartiallyReverted)
	if current.ErrorCode == nil || *current.ErrorCode != "batch_evidence_invalid" {
		t.Fatal("tampered compensation proof accepted", current)
	}
	var intact bool
	if err = db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE id IN (1,2,4) AND value=2)=3 FROM `+c.Relation()).Scan(&intact); err != nil || !intact {
		t.Fatal("invalid evidence caused SQL execution", err)
	}
}

func enqueueCompensation(t *testing.T, f *queueFixture, j jobs.Job, execution string, c batch.Config) jobs.Job {
	t.Helper()
	ctx := context.Background()
	down, _ := batch.DownSQL(c)
	setujuiAksiMigration(t, f.a, f.editor, j.DatabaseID, j.DraftID, j.DraftRevision, "compensate", execution, down)
	var review int64
	if err := f.a.db.QueryRow(ctx, `SELECT id FROM migration_reviews WHERE database_id=$1 AND action_type='compensate' AND target_execution_id=$2 AND status='approved'`, j.DatabaseID, execution).Scan(&review); err != nil {
		t.Fatal(err)
	}
	w := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/jobs", j.DatabaseID), map[string]string{"review_id": fmt.Sprint(review)})
	status(t, w, 202)
	var out struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Job.ActionType != "compensate" || out.Job.ExecutionMode != "batch" || out.Job.ParentJobID == nil || *out.Job.ParentJobID != j.ID {
		t.Fatal("invalid compensation handoff", out.Job)
	}
	replay := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/jobs", j.DatabaseID), map[string]string{"review_id": fmt.Sprint(review)})
	status(t, replay, 200)
	return out.Job
}

func TestCompensationReversesCommittedBatchesAndAllowsForward(t *testing.T) {
	f, db, c, original, attempt := compensationFixture(t, false)
	ctx := context.Background()
	path := fmt.Sprintf("/api/v1/databases/%d/migration-executions/%s/compensate", f.first, attempt.ExecutionID)
	status(t, f.viewer.request("POST", path, nil), 403)
	status(t, f.editor.request("POST", path, nil), 409)
	if _, err := db.Exec(ctx, `INSERT INTO `+c.Relation()+` VALUES(6,7)`); err != nil {
		t.Fatal(err)
	}
	j := enqueueCompensation(t, f, original, attempt.ExecutionID, c)
	var changed int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM `+c.Relation()+` WHERE value=2`).Scan(&changed); err != nil || changed != 5 {
		t.Fatal("enqueue ran compensation SQL inline", err)
	}
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, j, jobs.Success)
	if current.CommittedBatches != 3 || current.CommittedRows != 5 {
		t.Fatal("restored counts incorrect", current)
	}
	var restored bool
	if err := db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE id<=5 AND value=0)=5 AND count(*) FILTER(WHERE id=6 AND value=7)=1 FROM `+c.Relation()).Scan(&restored); err != nil || !restored {
		t.Fatal("frozen scope or restoration changed", err)
	}
	var reverse []int64
	rows, err := db.Query(ctx, `SELECT original_batch_no FROM _terserah.compensation_checkpoints WHERE execution_id=(SELECT execution_id FROM _terserah.compensation_runs WHERE source_execution_id=$1) ORDER BY batch_no`, attempt.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		reverse = append(reverse, n)
	}
	rows.Close()
	if fmt.Sprint(reverse) != "[3 2 1]" {
		t.Fatal("compensation order incorrect", reverse)
	}
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	rolled := attempts[0]
	var pairs int
	if err = f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_rollback_pairs WHERE applied_execution_id=$1 AND rollback_execution_id=$2`, attempt.ExecutionID, rolled.ExecutionID).Scan(&pairs); err != nil || pairs != 1 {
		t.Fatal("complete compensation did not change linear position", err)
	}
	w := f.viewer.request("GET", fmt.Sprintf("/api/v1/databases/%d/migrations?hide_rollback=true", f.first), nil)
	status(t, w, 200)
	var page struct {
		Migrations []runner.History `json:"migrations"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Migrations) != 0 {
		t.Fatal("Hide rollback did not hide the complete pair", err, w.Body.String())
	}
	up, _ := batch.Encode(c)
	setujuiAksiMigration(t, f.a, f.editor, f.first, original.DraftID, original.DraftRevision, "forward", rolled.ExecutionID, up)
	forward, _, err := f.service.EnqueueOperation(ctx, f.actor, f.first, "forward", rolled.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if forward.ExecutionMode != "batch" {
		t.Fatal("batch forward lost execution mode")
	}
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, forward, jobs.Success)
}

func TestPartialBatchCanBeCompensatedWithoutBecomingActive(t *testing.T) {
	f, db, c, original, attempt := compensationFixture(t, true)
	ctx := context.Background()
	j := enqueueCompensation(t, f, original, attempt.ExecutionID, c)
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, j, jobs.Success)
	if current.CommittedBatches != 1 || current.CommittedRows != 2 {
		t.Fatal("compensated uncommitted rows", current)
	}
	var restored bool
	if err := db.QueryRow(ctx, `SELECT bool_and(value=0) FROM `+c.Relation()).Scan(&restored); err != nil || !restored {
		t.Fatal("partial data was not restored", err)
	}
	w := f.editor.request("GET", fmt.Sprintf("/api/v1/databases/%d/migration-state", f.first), nil)
	status(t, w, 200)
	var state struct {
		Active     []any `json:"active"`
		Unresolved bool  `json:"has_unresolved"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || len(state.Active) != 0 || state.Unresolved {
		t.Fatal("cancelled partial migration became active", w.Body.String(), err)
	}
	next := queuedJob(t, f, f.first, "SELECT 1")
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, next, jobs.Success)
}

func TestCompensationConflictRetainsCheckpointAndApprovedContinuationSkipsIt(t *testing.T) {
	f, db, c, original, attempt := compensationFixture(t, false)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `UPDATE `+c.Relation()+` SET value=99 WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	first := enqueueCompensation(t, f, original, attempt.ExecutionID, c)
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	current := jobPhase(t, f, first, jobs.PartiallyReverted)
	if current.CommittedBatches != 1 || current.CommittedRows != 1 {
		t.Fatal("failed reverse batch retained partial writes", current)
	}
	var valid bool
	if err := db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE id=5 AND value=0)=1 AND count(*) FILTER(WHERE id=4 AND value=2)=1 AND count(*) FILTER(WHERE id=3 AND value=99)=1 FROM `+c.Relation()).Scan(&valid); err != nil || !valid {
		t.Fatal("external change or failed batch was overwritten", err)
	}
	next := queuedJob(t, f, f.first, "SELECT 1")
	if claimed, err := f.service.Claim(ctx, "blocked-compensation", 20*1000000000); err != nil || claimed != nil {
		t.Fatal("partially reverted target was released", err)
	}
	other := queuedJob(t, f, f.second, "SELECT 2")
	if _, err := queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, other, jobs.Success)
	_, attempts, err := f.service.Get(ctx, f.actor, f.first, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A later external change to an already restored row must survive continuation.
	if _, err = db.Exec(ctx, `UPDATE `+c.Relation()+` SET value=88 WHERE id=5; UPDATE `+c.Relation()+` SET value=2 WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	child := enqueueCompensation(t, f, first, attempts[0].ExecutionID, c)
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	done := jobPhase(t, f, child, jobs.Success)
	if done.CommittedBatches != 3 || done.CommittedRows != 5 {
		t.Fatal("continuation lost prior progress", done)
	}
	if err = db.QueryRow(ctx, `SELECT count(*) FILTER(WHERE id<5 AND value=0)=4 AND count(*) FILTER(WHERE id=5 AND value=88)=1 FROM `+c.Relation()).Scan(&valid); err != nil || !valid {
		t.Fatal("continuation repeated an already committed reverse batch", err)
	}
	if _, err = queueWorker(t, f).ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	jobPhase(t, f, next, jobs.Success)
}
