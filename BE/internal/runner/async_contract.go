package runner

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/connections"
)

var errAsyncOperationInvalid = errors.New("kontrak operasi asynchronous tidak valid")

// AsyncOperationRequest adalah kontrak dari API/queue menuju runner.
// Tipe ini tidak bergantung pada package jobs agar tidak terjadi import cycle.
type AsyncOperationRequest struct {
	DatabaseID        int64
	DraftID           int64
	Revision          int64
	ReviewID          int64
	ActorID           int64
	ActionType        string
	TargetExecutionID string
}

// AsyncOperationPlan berisi SQL yang harus dibekukan sebagai snapshot job.
// Penyimpanan job wajib berada pada transaksi metadata yang sama dengan persiapan ini.
type AsyncOperationPlan struct {
	AsyncOperationRequest
	SQL string `json:"-"`
}

func (in AsyncOperationRequest) valid() bool {
	if in.DatabaseID < 1 || in.DraftID < 1 || in.Revision < 1 || in.ReviewID < 1 || in.ActorID < 1 {
		return false
	}
	switch in.ActionType {
	case "up":
		return in.TargetExecutionID == ""
	case "rollback", "forward":
		return in.TargetExecutionID != "" && len(in.TargetExecutionID) <= 128
	default:
		return false
	}
}

// PrepareAsyncOperationTx dijalankan di transaksi enqueue dengan lock database.
// Pemanggil wajib menyimpan job dan snapshot SQL sebelum transaction commit.
func PrepareAsyncOperationTx(ctx context.Context, tx pgx.Tx, in AsyncOperationRequest) (AsyncOperationPlan, error) {
	return prepareAsyncOperation(ctx, tx, in, "")
}

// RevalidateAsyncOperationTx dijalankan worker SETELAH memperoleh lock target.
// Pastikan claim/lease worker masih sah secara terpisah. Tidak boleh dipakai untuk
// melewati validasi target identity, receipt, maupun pengamanan transaksi SQL.
func RevalidateAsyncOperationTx(ctx context.Context, tx pgx.Tx, in AsyncOperationRequest, executionID, queuedSQL string) error {
	if !in.valid() || executionID == "" || queuedSQL == "" {
		return errAsyncOperationInvalid
	}
	// Urutan lock selalu database_connections terlebih dahulu, sama seperti enqueue.
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, in.DatabaseID).Scan(&locked); err != nil {
		return err
	}
	var draftID, revision, actorID int64
	var action, sqlText, status string
	var target *string
	err := tx.QueryRow(ctx, `SELECT draft_id,draft_revision,executed_by,action_type,sql,status,target_execution_id
		FROM migration_history WHERE database_id=$1 AND execution_id=$2 FOR UPDATE`, in.DatabaseID, executionID).
		Scan(&draftID, &revision, &actorID, &action, &sqlText, &status, &target)
	if err != nil {
		return err
	}
	if status != "running" || draftID != in.DraftID || revision != in.Revision || actorID != in.ActorID ||
		action != in.ActionType || sqlText != queuedSQL || (target == nil && in.TargetExecutionID != "") ||
		(target != nil && *target != in.TargetExecutionID) {
		return errConflict
	}
	plan, err := prepareAsyncOperation(ctx, tx, in, executionID)
	if err != nil {
		return err
	}
	if plan.SQL != queuedSQL {
		return errConflict
	}
	return nil
}

// prepareAsyncOperation memvalidasi permission, posisi linear, snapshot, dan review.
// Baris database_connections dikunci supaya keputusan tidak berubah sampai commit.
func prepareAsyncOperation(ctx context.Context, tx pgx.Tx, in AsyncOperationRequest, ownExecutionID string) (AsyncOperationPlan, error) {
	if !in.valid() {
		return AsyncOperationPlan{}, errAsyncOperationInvalid
	}
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, in.DatabaseID).Scan(&locked); err != nil {
		return AsyncOperationPlan{}, err
	}
	var canEdit bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM users u JOIN user_groups ug ON ug.user_id=u.id
		JOIN group_database_permissions p ON p.group_id=ug.group_id
		WHERE u.id=$1 AND u.is_active AND p.database_id=$2 AND p.access_level='edit'
	)`, in.ActorID, in.DatabaseID).Scan(&canEdit)
	if err != nil {
		return AsyncOperationPlan{}, err
	}
	if !canEdit {
		return AsyncOperationPlan{}, connections.ErrForbidden
	}
	state, err := bacaPosisiMigrationTanpaAttempt(ctx, tx, in.DatabaseID, ownExecutionID)
	if err != nil {
		return AsyncOperationPlan{}, err
	}
	plan := AsyncOperationPlan{AsyncOperationRequest: in}
	var review approval.ReviewView
	if in.ActionType == "up" {
		var revision int64
		err = tx.QueryRow(ctx, `SELECT sql,revision FROM migration_drafts WHERE id=$1 AND database_id=$2 FOR UPDATE`,
			in.DraftID, in.DatabaseID).Scan(&plan.SQL, &revision)
		if err != nil {
			return AsyncOperationPlan{}, err
		}
		if revision != in.Revision {
			return AsyncOperationPlan{}, errConflict
		}
		if err = state.cekUp(in.DraftID); err != nil {
			return AsyncOperationPlan{}, err
		}
		review, err = approval.EnsureApprovedTx(ctx, tx, in.DatabaseID, in.DraftID, in.Revision, in.ActionType)
	} else {
		var targetDraft, targetRevision *int64
		var targetStatus, targetAction string
		err = tx.QueryRow(ctx, `SELECT draft_id,draft_revision,status,action_type FROM migration_history
			WHERE database_id=$1 AND execution_id=$2`, in.DatabaseID, in.TargetExecutionID).
			Scan(&targetDraft, &targetRevision, &targetStatus, &targetAction)
		if err != nil {
			return AsyncOperationPlan{}, err
		}
		if targetDraft == nil || targetRevision == nil || *targetDraft != in.DraftID ||
			*targetRevision != in.Revision || targetStatus != "success" {
			return AsyncOperationPlan{}, errMigrationOrder
		}
		if in.ActionType == "rollback" {
			if targetAction != "up" && targetAction != "forward" {
				return AsyncOperationPlan{}, errMigrationOrder
			}
			if err = state.cekRollback(in.TargetExecutionID); err != nil {
				return AsyncOperationPlan{}, err
			}
			var down *string
			var irreversible bool
			err = tx.QueryRow(ctx, `SELECT sql_down,irreversible FROM migration_revision_snapshots
				WHERE draft_id=$1 AND revision=$2`, in.DraftID, in.Revision).Scan(&down, &irreversible)
			if err != nil {
				return AsyncOperationPlan{}, err
			}
			if irreversible || down == nil {
				return AsyncOperationPlan{}, errMigrationIrreversible
			}
			plan.SQL = *down
		} else {
			if targetAction != "rollback" && targetAction != "compensate" {
				return AsyncOperationPlan{}, errMigrationOrder
			}
			if err = state.cekForward(in.TargetExecutionID); err != nil {
				return AsyncOperationPlan{}, err
			}
			err = tx.QueryRow(ctx, `SELECT sql_up FROM migration_revision_snapshots
				WHERE draft_id=$1 AND revision=$2`, in.DraftID, in.Revision).Scan(&plan.SQL)
			if err != nil {
				return AsyncOperationPlan{}, err
			}
		}
		review, err = approval.EnsureApprovedExecutionTx(ctx, tx, in.DatabaseID, in.DraftID,
			in.Revision, in.ActionType, in.TargetExecutionID, plan.SQL)
	}
	if err != nil {
		return AsyncOperationPlan{}, err
	}
	if review.ID != in.ReviewID || review.SQLSnapshot != plan.SQL {
		return AsyncOperationPlan{}, errConflict
	}
	if err = validateDraftSQL(plan.SQL); err != nil {
		return AsyncOperationPlan{}, err
	}
	return plan, nil
}
