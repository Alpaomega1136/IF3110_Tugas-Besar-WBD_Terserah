package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
)

// The job identity stays immutable; a new approval authorizes a bounded later attempt.
func attachAuthorization(ctx context.Context, tx pgx.Tx, j *Job) error {
	next := j.AttemptCount
	if j.Status == RetryWait {
		next++
	}
	var id int64
	err := tx.QueryRow(ctx, `SELECT review_id FROM migration_retry_authorizations
 WHERE job_id=$1 AND first_attempt<=$2 ORDER BY first_attempt DESC LIMIT 1`, j.ID, next).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err == nil {
		j.AuthorizationReviewID = &id
	}
	return err
}

func enqueueReauthorizedRetryTx(ctx context.Context, tx pgx.Tx, actor, dbID, reviewID, draftID, revision int64, action string, target *string, sql string) (Job, bool, error) {
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs
 WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3 AND action_type=$4
 AND target_execution_id IS NOT DISTINCT FROM $5::text AND execution_mode='normal'
 AND sql_snapshot=$6 ORDER BY id DESC LIMIT 1 FOR UPDATE`, dbID, draftID, revision, action, target, sql))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if j.Status != Failed {
		return Job{}, false, nil
	}
	if j.AttemptCount >= j.MaxAttempts {
		return Job{}, false, ErrConflict
	}
	review, err := approval.EnsureApprovedReviewTx(ctx, tx, dbID, draftID, revision, action, reviewID, target)
	if err != nil {
		return Job{}, false, err
	}
	if review.SQLSnapshot != sql {
		return Job{}, false, ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO migration_retry_authorizations(job_id,first_attempt,review_id,requested_by) VALUES($1,$2,$3,$4)`, j.ID, j.AttemptCount+1, reviewID, actor); err != nil {
		return Job{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET retry_scheduled=true WHERE job_id=$1 AND attempt_no=$2 AND status='failed'`, j.ID, j.AttemptCount); err != nil {
		return Job{}, false, err
	}
	j, err = scanJob(tx.QueryRow(ctx, `UPDATE migration_jobs SET status='retry_wait',finished_at=NULL,error_code=NULL,available_at=clock_timestamp() WHERE id=$1 RETURNING `+jobColumns, j.ID))
	if err != nil {
		return Job{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_reviews SET status='queued' WHERE id=$1`, reviewID); err != nil {
		return Job{}, false, err
	}
	j.AuthorizationReviewID = &reviewID
	return j, true, nil
}
