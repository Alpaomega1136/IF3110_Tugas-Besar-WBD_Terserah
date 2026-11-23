package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/telemetry"
)

// RetryExecution schedules a new attempt within the original bounded job budget.
// The explicit failed execution makes repeated requests idempotent.
func (s *Service) RetryExecution(ctx context.Context, p auth.Principal, dbID int64, execution, action string, target *string) (Job, bool, error) {
	if err := s.authorize(ctx, p, dbID, true); err != nil {
		return Job{}, false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Job{}, false, err
	}
	defer rollback(ctx, tx)
	var locked int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, dbID).Scan(&locked); err != nil {
		return Job{}, false, err
	}
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN user_groups ug ON ug.user_id=u.id JOIN group_database_permissions p ON p.group_id=ug.group_id WHERE u.id=$1 AND u.is_active AND p.database_id=$2 AND p.access_level='edit')`, p.ID, dbID).Scan(&allowed); err != nil {
		return Job{}, false, err
	}
	if !allowed {
		return Job{}, false, ErrForbidden
	}
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND id=(SELECT job_id FROM migration_history WHERE database_id=$1 AND execution_id=$2) FOR UPDATE`, dbID, execution))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, ErrNotFound
	}
	if err != nil {
		return Job{}, false, err
	}
	if err = attachAuthorization(ctx, tx, &j); err != nil {
		return Job{}, false, err
	}
	if j.ActionType != action || j.ExecutionMode != "normal" || (target == nil) != (j.TargetExecutionID == nil) || (target != nil && *target != *j.TargetExecutionID) {
		return Job{}, false, ErrConflict
	}
	var state Status
	var scheduled bool
	var number int
	if err = tx.QueryRow(ctx, `SELECT status,retry_scheduled,attempt_no FROM migration_job_attempts WHERE job_id=$1 AND execution_id=$2`, j.ID, execution).Scan(&state, &scheduled, &number); err != nil {
		return Job{}, false, err
	}
	if scheduled {
		return j, true, nil
	}
	if state != Failed || j.Status != Failed || number != j.AttemptCount || j.AttemptCount >= j.MaxAttempts {
		return Job{}, false, ErrConflict
	}
	reviewID := j.ReviewID
	if j.AuthorizationReviewID != nil {
		reviewID = *j.AuthorizationReviewID
	}
	if _, err = approval.EnsureApprovedReviewTx(ctx, tx, dbID, j.DraftID, j.DraftRevision, j.ActionType, reviewID, j.TargetExecutionID); err != nil {
		return Job{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET retry_scheduled=true WHERE execution_id=$1`, execution); err != nil {
		return Job{}, false, err
	}
	j, err = scanJob(tx.QueryRow(ctx, `UPDATE migration_jobs SET status='retry_wait',finished_at=NULL,error_code=NULL,available_at=clock_timestamp() WHERE id=$1 RETURNING `+jobColumns, j.ID))
	if err != nil {
		return Job{}, false, err
	}
	if err = attachAuthorization(ctx, tx, &j); err != nil {
		return Job{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	telemetry.Event(ctx, "job_retry_queued", "actor_id", p.ID, "database_id", dbID, "job_id", j.ID, "execution_id", execution, "attempt", j.AttemptCount, "status", j.Status)
	return j, false, nil
}

// EnqueueOperation resolves only an exact approved action/target; it never executes SQL.
func (s *Service) EnqueueOperation(ctx context.Context, p auth.Principal, dbID int64, action, target string) (Job, bool, error) {
	if err := s.authorize(ctx, p, dbID, true); err != nil {
		return Job{}, false, err
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history WHERE database_id=$1 AND execution_id=$2)`, dbID, target).Scan(&exists); err != nil {
		return Job{}, false, err
	}
	if !exists {
		return Job{}, false, ErrNotFound
	}
	var reviewID int64
	err := s.db.QueryRow(ctx, `SELECT id FROM migration_reviews WHERE database_id=$1 AND action_type=$2 AND target_execution_id=$3 AND status IN ('approved','queued') ORDER BY id DESC LIMIT 1`, dbID, action, target).Scan(&reviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, approval.ErrNotApproved
	}
	if err != nil {
		return Job{}, false, err
	}
	return s.Enqueue(ctx, p, dbID, reviewID, "")
}
