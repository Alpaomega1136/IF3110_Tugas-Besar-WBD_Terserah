package jobs

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/batch"
)

func enqueueCompensationTx(ctx context.Context, tx pgx.Tx, actor, dbID, reviewID, draftID, revision int64, state string, target *string, parentTrace string) (Job, error) {
	if target == nil || state != approval.StatusApproved {
		return Job{}, approval.ErrNotApproved
	}
	source, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND id=(SELECT job_id FROM migration_history WHERE database_id=$1 AND execution_id=$2) FOR UPDATE`, dbID, *target))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrConflict
	}
	if err != nil {
		return Job{}, err
	}
	if source.DraftID != draftID || source.DraftRevision != revision || source.ExecutionMode != "batch" || source.ResumedByJobID != nil ||
		(source.Status != Partial && source.Status != Success && source.Status != PartiallyReverted) ||
		(source.ActionType == "compensate" && source.Status != PartiallyReverted) {
		return Job{}, ErrConflict
	}
	var latest, root string
	var compRoot *string
	if err = tx.QueryRow(ctx, `SELECT a.execution_id,j.batch_root_execution_id,j.compensation_root_execution_id FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id AND a.attempt_no=j.attempt_count WHERE j.id=$1`, source.ID).Scan(&latest, &root, &compRoot); err != nil {
		return Job{}, err
	}
	if latest != *target || root == "" {
		return Job{}, ErrConflict
	}
	if source.Status == Success {
		var last bool
		err = tx.QueryRow(ctx, `SELECT COALESCE($2=(SELECT h.execution_id FROM migration_history h WHERE h.database_id=$1 AND h.status='success' AND h.action_type IN ('up','resume','forward') AND NOT EXISTS(SELECT 1 FROM migration_rollback_pairs p WHERE p.applied_execution_id=h.execution_id) ORDER BY h.version DESC LIMIT 1),false)`, dbID, *target).Scan(&last)
		if err != nil {
			return Job{}, err
		}
		if !last {
			return Job{}, ErrConflict
		}
	}
	review, err := approval.EnsureApprovedReviewTx(ctx, tx, dbID, draftID, revision, "compensate", reviewID, target)
	if err != nil {
		return Job{}, err
	}
	config, err := batch.ParseDown(review.SQLSnapshot)
	if err != nil || config == nil {
		return Job{}, ErrUnsupported
	}
	var up string
	if err = tx.QueryRow(ctx, `SELECT sql_up FROM migration_revision_snapshots WHERE draft_id=$1 AND revision=$2`, draftID, revision).Scan(&up); err != nil {
		return Job{}, err
	}
	expected, err := batch.Encode(*config)
	if err != nil || strings.TrimSpace(up) != expected {
		return Job{}, ErrUnsupported
	}
	j := Job{DatabaseID: dbID, ReviewID: reviewID, DraftID: draftID, DraftRevision: revision, ActionType: "compensate", ExecutionMode: "batch", RequestedBy: actor, ParentJobID: &source.ID, TargetExecutionID: target}
	var restoredBatches, restoredRows int64
	if source.ActionType == "compensate" {
		restoredBatches, restoredRows = source.CommittedBatches, source.CommittedRows
	}
	j, err = scanJob(tx.QueryRow(ctx, `INSERT INTO migration_jobs(database_id,review_id,draft_id,draft_revision,action_type,execution_mode,sql_snapshot,snapshot_hash,requested_by,trace_parent,parent_job_id,target_execution_id,batch_root_execution_id,compensation_root_execution_id,committed_batches,committed_rows)
 VALUES($1,$2,$3,$4,'compensate','batch',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING `+jobColumns, dbID, reviewID, draftID, revision, review.SQLSnapshot, snapshotHash(j, review.SQLSnapshot), actor, traceParent(parentTrace), source.ID, *target, root, compRoot, restoredBatches, restoredRows))
	if err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_jobs SET status='resumed',resumed_by_job_id=$2,finished_at=NULL WHERE id=$1`, source.ID, j.ID); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET retry_scheduled=true WHERE execution_id=$1`, *target); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_history SET finished_at=COALESCE(finished_at,clock_timestamp()) WHERE execution_id=$1`, *target); err != nil {
		return Job{}, err
	}
	j, err = scanJob(tx.QueryRow(ctx, `UPDATE migration_jobs SET status='retry_wait' WHERE id=$1 RETURNING `+jobColumns, j.ID))
	if err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_reviews SET status='queued' WHERE id=$1`, reviewID); err != nil {
		return Job{}, err
	}
	return j, nil
}

func ValidateCompensationTx(ctx context.Context, tx pgx.Tx, j Job, sql string) error {
	if j.ParentJobID == nil || j.TargetExecutionID == nil {
		return ErrConflict
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs child JOIN migration_jobs parent ON parent.id=child.parent_job_id
 JOIN migration_job_attempts a ON a.job_id=parent.id AND a.attempt_no=parent.attempt_count
 JOIN migration_revision_snapshots snap ON snap.draft_id=child.draft_id AND snap.revision=child.draft_revision
 WHERE child.id=$1 AND child.parent_job_id=$2 AND child.target_execution_id=$3 AND a.execution_id=child.target_execution_id
 AND child.database_id=$4 AND child.requested_by=$5 AND child.sql_snapshot=$6 AND child.action_type='compensate'
 AND child.execution_mode='batch' AND parent.status='resumed' AND parent.resumed_by_job_id=child.id
 AND parent.database_id=child.database_id AND parent.draft_id=child.draft_id AND parent.draft_revision=child.draft_revision
 AND parent.batch_root_execution_id=child.batch_root_execution_id AND snap.sql_down=child.sql_snapshot AND NOT snap.irreversible
 AND ((parent.action_type='compensate' AND a.status='partially_reverted' AND parent.compensation_root_execution_id=child.compensation_root_execution_id)
 OR (parent.action_type IN ('up','resume','forward') AND a.status IN ('success','partial','failed'))))`, j.ID, *j.ParentJobID, *j.TargetExecutionID, j.DatabaseID, j.RequestedBy, sql).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}
