package runner

import (
	"context"
	"crypto/hmac"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
)

func protectedCompensationHistory(ctx context.Context, tx pgx.Tx) error {
	if err := protectedBatchHistory(ctx, tx); err != nil {
		return err
	}
	if err := protectedTables(ctx, tx, []string{"compensation_runs", "compensation_checkpoints"}); err != nil {
		return errBatchSetup
	}
	for _, sql := range []string{
		`SELECT execution_id,source_execution_id,source_signature,database_id,version,executed_by,sql,total_batches,total_rows,signature FROM _terserah.compensation_runs LIMIT 0`,
		`SELECT execution_id,batch_no,original_batch_no,row_count,committed_rows,row_hash,previous_signature,signature,attempt_execution_id,attempt_version FROM _terserah.compensation_checkpoints LIMIT 0`,
	} {
		r, err := tx.Query(ctx, sql)
		if err != nil {
			return errBatchSetup
		}
		r.Close()
		if r.Err() != nil {
			return errBatchSetup
		}
	}
	return nil
}

// Verify the original immutable scope and every original row-image hash before undoing it.
func (s *Service) compensationEvidence(ctx context.Context, tx pgx.Tx, dbID int64, current pendingExecution, upRoot, compRoot string) (batch.CompensationManifest, batch.Checkpoint, bool, error) {
	var m batch.CompensationManifest
	var progress batch.Checkpoint
	if current.execution == nil || upRoot == "" || compRoot == "" {
		return m, progress, false, errBatchEvidence
	}
	if err := protectedCompensationHistory(ctx, tx); err != nil {
		return m, progress, false, err
	}
	var source pendingExecution
	err := s.db.QueryRow(ctx, `SELECT execution_id,version,sql,executed_by,receipt_version FROM migration_history WHERE database_id=$1 AND execution_id=$2 AND batch_root_execution_id=execution_id`, dbID, upRoot).Scan(&source.execution, &source.version, &source.sql, &source.actor, &source.format)
	if err != nil {
		return m, progress, false, errBatchEvidence
	}
	config, err := batch.Parse(source.sql)
	if err != nil || config == nil {
		return m, progress, false, errBatchEvidence
	}
	down, err := batch.DownSQL(*config)
	if err != nil || down != strings.TrimSpace(current.sql) {
		return m, progress, false, errBatchEvidence
	}
	_, _, committed, err := s.verifyBatchPending(ctx, tx, dbID, source)
	if err != nil {
		return m, progress, false, err
	}
	original, exists, err := loadBatchManifest(ctx, tx, upRoot)
	if err != nil || !exists {
		return m, progress, false, errBatchEvidence
	}
	var root pendingExecution
	err = s.db.QueryRow(ctx, `SELECT execution_id,version,sql,executed_by,receipt_version FROM migration_history
 WHERE database_id=$1 AND execution_id=$2 AND batch_root_execution_id=$3 AND action_type='compensate'`, dbID, compRoot, upRoot).Scan(&root.execution, &root.version, &root.sql, &root.actor, &root.format)
	if err != nil || root.sql != current.sql {
		return m, progress, false, errBatchEvidence
	}
	m = batch.CompensationManifest{ExecutionID: compRoot, SourceExecutionID: upRoot, SourceSignature: batch.ManifestSignature(s.key, original), SQL: current.sql, DatabaseID: dbID, Version: root.version, ActorID: root.actor, Batches: committed.BatchNo, Rows: committed.CommittedRows}
	var stored batch.CompensationManifest
	stored.ExecutionID = compRoot
	var signature string
	err = tx.QueryRow(ctx, `SELECT source_execution_id,source_signature,sql,database_id,version,executed_by,total_batches,total_rows,signature FROM _terserah.compensation_runs WHERE execution_id=$1`, compRoot).Scan(&stored.SourceExecutionID, &stored.SourceSignature, &stored.SQL, &stored.DatabaseID, &stored.Version, &stored.ActorID, &stored.Batches, &stored.Rows, &signature)
	if errors.Is(err, pgx.ErrNoRows) {
		var orphan bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _terserah.compensation_checkpoints WHERE execution_id=$1)
 OR EXISTS(SELECT 1 FROM _terserah.migration_receipts WHERE execution_id IN ($1,$2))`, compRoot, *current.execution).Scan(&orphan)
		if err != nil {
			return m, progress, false, err
		}
		if orphan {
			return m, progress, false, errBatchEvidence
		}
		return m, progress, false, nil
	}
	if err != nil {
		return m, progress, false, err
	}
	expected := batch.CompensationSignature(s.key, m)
	if stored != m || !hmac.Equal([]byte(signature), []byte(expected)) {
		return m, progress, false, errBatchEvidence
	}
	previous := signature
	for n := int64(1); n <= m.Batches; n++ {
		var cp batch.Checkpoint
		err = tx.QueryRow(ctx, `SELECT batch_no,original_batch_no,row_count,committed_rows,row_hash,previous_signature,signature,attempt_execution_id,attempt_version FROM _terserah.compensation_checkpoints WHERE execution_id=$1 AND batch_no=$2`, compRoot, n).Scan(&cp.BatchNo, &cp.LastKey, &cp.RowCount, &cp.CommittedRows, &cp.RowHash, &cp.PreviousSignature, &cp.Signature, &cp.AttemptExecutionID, &cp.AttemptVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return m, progress, true, err
		}
		var count int64
		var hash string
		err = tx.QueryRow(ctx, `SELECT row_count,row_hash FROM _terserah.batch_checkpoints WHERE execution_id=$1 AND batch_no=$2`, upRoot, m.Batches-n+1).Scan(&count, &hash)
		if err != nil {
			return m, progress, true, errBatchEvidence
		}
		if cp.BatchNo != n || cp.LastKey != m.Batches-n+1 || cp.RowCount != count || cp.RowHash != hash || cp.CommittedRows != progress.CommittedRows+count || cp.CommittedRows > m.Rows || cp.PreviousSignature != previous || !hmac.Equal([]byte(cp.Signature), []byte(batch.CheckpointSignature(s.key, signature, cp))) {
			return m, progress, true, errBatchEvidence
		}
		var valid bool
		err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history h JOIN migration_jobs j ON j.id=h.job_id
 WHERE h.execution_id=$1 AND h.version=$2 AND h.database_id=$3 AND h.sql=$4 AND h.action_type='compensate'
 AND h.batch_root_execution_id=$5 AND j.compensation_root_execution_id=$6)`, cp.AttemptExecutionID, cp.AttemptVersion, dbID, current.sql, upRoot, compRoot).Scan(&valid)
		if err != nil {
			return m, progress, true, err
		}
		if !valid {
			return m, progress, true, errBatchEvidence
		}
		progress = cp
		previous = cp.Signature
	}
	var count int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM _terserah.compensation_checkpoints WHERE execution_id=$1`, compRoot).Scan(&count); err != nil {
		return m, progress, true, err
	}
	if count != progress.BatchNo {
		return m, progress, true, errBatchEvidence
	}
	return m, progress, true, nil
}

func (s *Service) verifyCompensationPending(ctx context.Context, tx pgx.Tx, dbID int64, current pendingExecution, upRoot, compRoot string) (Result, *time.Time, batch.Checkpoint, error) {
	m, cp, exists, err := s.compensationEvidence(ctx, tx, dbID, current, upRoot, compRoot)
	if err != nil {
		return Result{}, nil, cp, err
	}
	code := "compensation_incomplete"
	out := Result{ExecutionID: *current.execution, Version: current.version, Status: "partially_reverted", ErrorCode: &code}
	if !exists {
		return out, nil, cp, nil
	}
	root := pendingExecution{execution: &m.ExecutionID, version: m.Version, sql: m.SQL, actor: m.ActorID, format: 1}
	rootOut, _, err := s.verifyPendingAttempt(ctx, tx, dbID, root)
	if err != nil {
		return out, nil, cp, err
	}
	attempt, finished, err := s.verifyPendingAttempt(ctx, tx, dbID, current)
	if err != nil {
		return out, nil, cp, err
	}
	complete := cp.BatchNo == m.Batches && cp.CommittedRows == m.Rows
	if rootOut.Status == "unknown" || attempt.Status == "unknown" || (rootOut.Status == "success") != complete || (attempt.Status == "success") != complete {
		return out, nil, cp, errBatchEvidence
	}
	if complete {
		return attempt, finished, cp, nil
	}
	return out, nil, cp, nil
}
