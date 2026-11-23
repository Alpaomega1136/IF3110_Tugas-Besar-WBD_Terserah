package runner

import (
	"context"
	"github.com/jackc/pgx/v5"
	"strings"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
)

func validateCompensationSnapshot(ctx context.Context, tx pgx.Tx, j jobs.Job, sql string) error {
	if err := jobs.ValidateCompensationTx(ctx, tx, j, sql); err != nil {
		return errConflict
	}
	config, err := batch.ParseDown(sql)
	if err != nil || config == nil {
		return errBatchEvidence
	}
	var up, root, originalTarget, own string
	var sourceStatus string
	err = tx.QueryRow(ctx, `SELECT snap.sql_up,child.compensation_root_execution_id,root.target_execution_id,source.status,own.execution_id
 FROM migration_jobs child JOIN migration_history root ON root.execution_id=child.compensation_root_execution_id
 JOIN migration_history source ON source.execution_id=root.target_execution_id
 JOIN migration_job_attempts own ON own.job_id=child.id AND own.attempt_no=child.attempt_count
 JOIN migration_revision_snapshots snap ON snap.draft_id=child.draft_id AND snap.revision=child.draft_revision
 WHERE child.id=$1 AND root.database_id=child.database_id AND source.database_id=child.database_id
 AND root.draft_id=child.draft_id AND root.draft_revision=child.draft_revision`, j.ID).Scan(&up, &root, &originalTarget, &sourceStatus, &own)
	if err != nil {
		return err
	}
	expected, err := batch.Encode(*config)
	if err != nil || strings.TrimSpace(up) != expected {
		return errBatchEvidence
	}
	state, err := bacaPosisiMigrationTanpaAttempt(ctx, tx, j.DatabaseID, own)
	if err != nil {
		return err
	}
	if sourceStatus == "success" {
		if err = state.cekRollback(originalTarget); err != nil {
			return err
		}
	} else if state.HasUnresolved {
		return errMigrationUnresolved
	}
	review, err := approval.EnsureApprovedReviewTx(ctx, tx, j.DatabaseID, j.DraftID, j.DraftRevision, "compensate", j.ReviewID, j.TargetExecutionID)
	if err != nil {
		return err
	}
	if review.SQLSnapshot != sql {
		return errConflict
	}
	return nil
}
