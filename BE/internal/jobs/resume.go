package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/batch"
)

// The caller holds the database row lock; handoff and new approval consumption are atomic.
func enqueueResumeTx(ctx context.Context, tx pgx.Tx, actor, dbID, reviewID, draftID, revision int64, state string, target *string, parentTrace string) (Job, error) {
	if target == nil || state != approval.StatusApproved {
		return Job{}, approval.ErrNotApproved
	}
	var sourceID int64
	err := tx.QueryRow(ctx, `SELECT j.id FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id AND a.attempt_no=j.attempt_count
 JOIN migration_history h ON h.execution_id=a.execution_id
 WHERE j.database_id=$1 AND j.draft_id=$2 AND j.draft_revision=$3 AND a.execution_id=$4
 AND j.status='partial' AND j.execution_mode='batch' AND j.committed_batches>0
 AND a.status IN ('partial','failed') AND h.status IN ('partial','failed')`, dbID, draftID, revision, *target).Scan(&sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrConflict
	}
	if err != nil {
		return Job{}, err
	}
	source, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id=$1 FOR UPDATE`, sourceID))
	if err != nil {
		return Job{}, err
	}
	if source.Status != Partial || source.ResumedByJobID != nil {
		return Job{}, ErrConflict
	}
	var sql, hash, root string
	if err = tx.QueryRow(ctx, `SELECT sql_snapshot,snapshot_hash,COALESCE(batch_root_execution_id,'') FROM migration_jobs WHERE id=$1`, sourceID).Scan(&sql, &hash, &root); err != nil {
		return Job{}, err
	}
	config, err := batch.Parse(sql)
	if err != nil || config == nil || root == "" || snapshotHash(source, sql) != hash {
		return Job{}, ErrConflict
	}
	review, err := approval.EnsureApprovedReviewTx(ctx, tx, dbID, draftID, revision, "resume", reviewID, target)
	if err != nil {
		return Job{}, err
	}
	if review.SQLSnapshot != sql {
		return Job{}, ErrConflict
	}
	j := Job{DatabaseID: dbID, ReviewID: reviewID, DraftID: draftID, DraftRevision: revision, ActionType: "resume", ExecutionMode: "batch", RequestedBy: actor, ParentJobID: &sourceID, TargetExecutionID: target}
	j, err = scanJob(tx.QueryRow(ctx, `INSERT INTO migration_jobs(database_id,review_id,draft_id,draft_revision,action_type,execution_mode,sql_snapshot,snapshot_hash,requested_by,trace_parent,parent_job_id,target_execution_id,batch_root_execution_id,committed_batches,committed_rows)
 VALUES($1,$2,$3,$4,'resume','batch',$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING `+jobColumns, dbID, reviewID, draftID, revision, sql, snapshotHash(j, sql), actor, traceParent(parentTrace), sourceID, *target, root, source.CommittedBatches, source.CommittedRows))
	if err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_jobs SET status='resumed',resumed_by_job_id=$2 WHERE id=$1 AND status='partial'`, sourceID, j.ID); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET retry_scheduled=true WHERE execution_id=$1`, *target); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_history SET finished_at=COALESCE(finished_at,clock_timestamp()) WHERE execution_id=$1`, *target); err != nil {
		return Job{}, err
	}
	// Reserve the existing active-target slot while the approved resume waits for claim.
	j, err = scanJob(tx.QueryRow(ctx, `UPDATE migration_jobs SET status='retry_wait' WHERE id=$1 RETURNING `+jobColumns, j.ID))
	if err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_reviews SET status='queued' WHERE id=$1`, reviewID); err != nil {
		return Job{}, err
	}
	return j, nil
}

// ValidateResumeTx rechecks the immutable handoff while the caller holds the database lock.
func ValidateResumeTx(ctx context.Context, tx pgx.Tx, j Job, sql string) error {
	if j.ParentJobID == nil || j.TargetExecutionID == nil {
		return ErrConflict
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs child JOIN migration_jobs parent ON parent.id=child.parent_job_id
 JOIN migration_job_attempts a ON a.job_id=parent.id AND a.attempt_no=parent.attempt_count
 JOIN migration_history h ON h.execution_id=a.execution_id
 WHERE child.id=$1 AND child.parent_job_id=$2 AND child.target_execution_id=$3 AND child.database_id=$4
 AND child.requested_by=$5 AND child.sql_snapshot=$6 AND child.action_type='resume' AND child.execution_mode='batch'
 AND parent.status='resumed' AND parent.resumed_by_job_id=child.id AND parent.database_id=child.database_id
 AND parent.draft_id=child.draft_id AND parent.draft_revision=child.draft_revision AND parent.sql_snapshot=child.sql_snapshot
 AND parent.batch_root_execution_id=child.batch_root_execution_id AND a.execution_id=child.target_execution_id
 AND a.retry_scheduled AND a.status IN ('partial','failed') AND h.status IN ('partial','failed') AND h.sql=child.sql_snapshot)`, j.ID, *j.ParentJobID, *j.TargetExecutionID, j.DatabaseID, j.RequestedBy, sql).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}
