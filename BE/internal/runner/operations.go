package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
)

var errActionRetryInvalid = errors.New("retry rollback/forward hanya untuk attempt gagal dengan target yang sama")

// runAction menjalankan SQL Down atau SQL Up immutable pada target dengan lock dan receipt.
// Jalur synchronous ini akan dialihkan ke queue pada tahap integrasi worker.
func (s *Service) runAction(ctx context.Context, p auth.Principal, dbID int64, action, targetExecutionID, retryOf string) (Result, error) {
	var result Result
	if (action != "rollback" && action != "forward") || targetExecutionID == "" || len(targetExecutionID) > 128 || len(retryOf) > 128 {
		return result, errMigrationOrder
	}
	pool, err := s.targets.OpenTarget(ctx, p, dbID, true)
	if err != nil {
		return result, err
	}
	defer pool.Close()
	targetTx, userPath, err := s.lockAndValidateTarget(ctx, pool, p.ID, dbID)
	if err != nil {
		return result, err
	}
	defer rollback(targetTx)
	sqlText, parts, replayed, err := s.recordRunningAction(ctx, targetTx, p.ID, dbID, action, targetExecutionID, retryOf, &result)
	if err != nil || replayed {
		return result, err
	}
	execErr := s.execStatementsWithReceipt(ctx, targetTx, userPath, parts, dbID, result.ExecutionID, result.Version, sqlText, p.ID)
	s.commitAndFinalizeLedger(ctx, targetTx, execErr, &result)
	return result, nil
}

// recordRunningAction menyusun attempt di bawah lock metadata dan memilih SQL immutable.
func (s *Service) recordRunningAction(ctx context.Context, targetTx pgx.Tx, actorID, dbID int64, action, targetID, retryOf string, result *Result) (string, []string, bool, error) {
	meta, err := s.db.Begin(ctx)
	if err != nil {
		return "", nil, false, err
	}
	defer rollback(meta)
	var locked int64
	if err = meta.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, dbID).Scan(&locked); err != nil {
		return "", nil, false, err
	}
	if err = bindTarget(ctx, meta, targetTx, dbID); err != nil {
		return "", nil, false, err
	}

	var nullableDraftID, nullableRevision *int64
	var targetStatus, targetAction string
	err = meta.QueryRow(ctx, `SELECT draft_id,draft_revision,status,action_type FROM migration_history
		WHERE database_id=$1 AND execution_id=$2`, dbID, targetID).
		Scan(&nullableDraftID, &nullableRevision, &targetStatus, &targetAction)
	if err != nil {
		return "", nil, false, err
	}
	if nullableDraftID == nil || nullableRevision == nil {
		return "", nil, false, errMigrationOrder
	}
	draftID, revision := *nullableDraftID, *nullableRevision
	if targetStatus != "success" || (action == "rollback" && targetAction != "up" && targetAction != "forward") ||
		(action == "forward" && targetAction != "rollback") {
		return "", nil, false, errMigrationOrder
	}
	var sqlUp string
	var sqlDown *string
	var irreversible bool
	err = meta.QueryRow(ctx, `SELECT sql_up,sql_down,irreversible FROM migration_revision_snapshots
		WHERE draft_id=$1 AND revision=$2`, draftID, revision).Scan(&sqlUp, &sqlDown, &irreversible)
	if err != nil {
		return "", nil, false, err
	}
	if action == "rollback" && (irreversible || sqlDown == nil) {
		return "", nil, false, errMigrationIrreversible
	}
	sqlText := sqlUp
	if action == "rollback" {
		sqlText = *sqlDown
	}

	// Request biasa bersifat idempotent. Retry eksplisit mengacu pada attempt gagal.
	if retryOf == "" {
		err = meta.QueryRow(ctx, `SELECT execution_id,version,status,error_code FROM migration_history
			WHERE database_id=$1 AND action_type=$2 AND target_execution_id=$3
			ORDER BY version DESC LIMIT 1`, dbID, action, targetID).
			Scan(&result.ExecutionID, &result.Version, &result.Status, &result.ErrorCode)
		if err == nil {
			return "", nil, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, err
		}
	} else {
		err = meta.QueryRow(ctx, `SELECT execution_id,version,status,error_code FROM migration_history
			WHERE database_id=$1 AND retry_of_execution_id=$2`, dbID, retryOf).
			Scan(&result.ExecutionID, &result.Version, &result.Status, &result.ErrorCode)
		if err == nil {
			return "", nil, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, err
		}
		var parentStatus, parentAction, parentSQL, parentTarget string
		err = meta.QueryRow(ctx, `SELECT status,action_type,sql,target_execution_id FROM migration_history
			WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3 AND execution_id=$4
			FOR UPDATE`, dbID, draftID, revision, retryOf).
			Scan(&parentStatus, &parentAction, &parentSQL, &parentTarget)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, errActionRetryInvalid
		}
		if err != nil {
			return "", nil, false, err
		}
		if parentStatus != "failed" || parentAction != action || parentSQL != sqlText || parentTarget != targetID {
			return "", nil, false, errActionRetryInvalid
		}
		var committed bool
		if err = targetTx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _terserah.migration_receipts WHERE execution_id=$1)`, retryOf).Scan(&committed); err != nil {
			return "", nil, false, err
		}
		if committed {
			return "", nil, false, errMigrationUnresolved
		}
	}

	state, err := bacaPosisiMigration(ctx, meta, dbID)
	if err != nil {
		return "", nil, false, err
	}
	if action == "rollback" {
		err = state.cekRollback(targetID)
	} else {
		err = state.cekForward(targetID)
	}
	if err != nil {
		return "", nil, false, err
	}
	if _, err = approval.EnsureApprovedExecutionTx(ctx, meta, dbID, draftID, revision, action, targetID, sqlText); err != nil {
		return "", nil, false, err
	}
	parts, err := statements(sqlText)
	if err != nil {
		return "", nil, false, err
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return "", nil, false, err
	}
	result.ExecutionID = hex.EncodeToString(token[:])
	result.Status = "running"
	var parent *string
	if retryOf != "" {
		parent = &retryOf
	}
	result.Version, err = catatExecutionAksiTx(ctx, meta, dbID, draftID, revision, actorID,
		result.ExecutionID, action, sqlText, &targetID, parent)
	if err != nil {
		return "", nil, false, err
	}
	if err = meta.Commit(ctx); err != nil {
		return "", nil, false, err
	}
	return sqlText, parts, false, nil
}

// simpanHasilAttemptTx memperbarui hasil dan pasangan rollback dalam satu transaksi.
// Dengan demikian status sukses rollback tidak akan terlihat tanpa pasangan execution.
func simpanHasilAttemptTx(ctx context.Context, tx pgx.Tx, result Result, finished *time.Time) error {
	var dbID int64
	if err := tx.QueryRow(ctx, `SELECT database_id FROM migration_history WHERE execution_id=$1`, result.ExecutionID).Scan(&dbID); err != nil {
		return err
	}
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, dbID).Scan(&locked); err != nil {
		return err
	}
	var action, prior string
	var target *string
	if err := tx.QueryRow(ctx, `SELECT action_type,target_execution_id,status FROM migration_history
		WHERE execution_id=$1 FOR UPDATE`, result.ExecutionID).Scan(&action, &target, &prior); err != nil {
		return err
	}
	if prior == result.Status && (prior == "success" || prior == "failed") {
		// Reconciliation lain boleh saja telah menyimpan hasil yang sama.
		return nil
	}
	if prior != "running" && prior != "unknown" {
		return errConflict
	}
	_, err := tx.Exec(ctx, `UPDATE migration_history SET status=$1,error_code=$2,finished_at=$3
		WHERE execution_id=$4`, result.Status, result.ErrorCode, finished, result.ExecutionID)
	if err != nil {
		return err
	}
	if result.Status == "success" && action == "rollback" {
		if target == nil {
			return errMigrationStateInvalid
		}
		_, err = tx.Exec(ctx, `INSERT INTO migration_rollback_pairs
			(database_id,applied_execution_id,rollback_execution_id) VALUES ($1,$2,$3)`,
			dbID, *target, result.ExecutionID)
		if err != nil {
			return err
		}
	}
	return nil
}

// finalisasiAttempt menjaga atomisitas status dan pasangan rollback setelah commit target.
func (s *Service) finalisasiAttempt(ctx context.Context, result Result, finished *time.Time) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := simpanHasilAttemptTx(ctx, tx, result, finished); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
