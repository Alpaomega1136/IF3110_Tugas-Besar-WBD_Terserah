package runner

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	if prior != "running" && prior != "unknown" && prior != "partial" {
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
			(database_id,applied_execution_id,rollback_execution_id) VALUES ($1,$2,$3) ON CONFLICT(rollback_execution_id) DO NOTHING`,
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
	defer rollback(ctx, tx)
	var dbID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=(SELECT database_id FROM migration_history WHERE execution_id=$1) FOR UPDATE`, result.ExecutionID).Scan(&dbID); err != nil {
		return err
	}
	var live bool
	if err = tx.QueryRow(ctx, `SELECT h.job_id IS NULL OR EXISTS(SELECT 1 FROM migration_job_attempts a JOIN migration_jobs j ON j.id=a.job_id WHERE a.execution_id=h.execution_id AND a.attempt_no=j.attempt_count AND NOT a.retry_scheduled AND j.status='running' AND j.lease_expires_at>clock_timestamp()) FROM migration_history h WHERE h.execution_id=$1`, result.ExecutionID).Scan(&live); err != nil {
		return err
	}
	if !live {
		return errActiveJob
	}
	if err := simpanHasilAttemptTx(ctx, tx, result, finished); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
