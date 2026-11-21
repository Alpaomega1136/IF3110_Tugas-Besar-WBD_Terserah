package runner

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
)

// The caller holds both target locks: no previous batch can still commit while evidence is read.
func (s *Service) verifyBatchPending(ctx context.Context, tx pgx.Tx, dbID int64, item pendingExecution) (Result, *time.Time, batch.Checkpoint, error) {
	var progress batch.Checkpoint
	if item.execution == nil || item.format != 1 {
		return Result{}, nil, progress, errBatchEvidence
	}
	config, err := batch.Parse(item.sql)
	if err != nil || config == nil {
		return Result{}, nil, progress, errBatchEvidence
	}
	if err = protectedBatchHistory(ctx, tx); err != nil {
		return Result{}, nil, progress, err
	}
	m := batch.Manifest{ExecutionID: *item.execution}
	var signature string
	err = tx.QueryRow(ctx, `SELECT database_id,version,executed_by,sql,table_oid,scope_count,scope_hash,signature FROM _terserah.batch_runs WHERE execution_id=$1`, m.ExecutionID).Scan(&m.DatabaseID, &m.Version, &m.ActorID, &m.SQL, &m.TableOID, &m.ScopeCount, &m.ScopeHash, &signature)
	if errors.Is(err, pgx.ErrNoRows) {
		var orphan bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _terserah.batch_scope WHERE execution_id=$1)
 OR EXISTS(SELECT 1 FROM _terserah.batch_rows WHERE execution_id=$1)
 OR EXISTS(SELECT 1 FROM _terserah.batch_checkpoints WHERE execution_id=$1)
 OR EXISTS(SELECT 1 FROM _terserah.migration_receipts WHERE execution_id=$1)`, m.ExecutionID).Scan(&orphan)
		if err != nil {
			return Result{}, nil, progress, err
		}
		if orphan {
			return Result{}, nil, progress, errBatchEvidence
		}
		code := "reconciled_rollback"
		now := time.Now().UTC()
		return Result{ExecutionID: m.ExecutionID, Version: item.version, Status: "failed", ErrorCode: &code}, &now, progress, nil
	}
	if err != nil {
		return Result{}, nil, progress, err
	}
	if m.DatabaseID != dbID || m.Version != item.version || m.ActorID != item.actor || m.SQL != item.sql || m.ScopeCount < 0 || !hmac.Equal([]byte(signature), []byte(batch.ManifestSignature(s.key, m))) {
		return Result{}, nil, progress, errBatchEvidence
	}
	if err = verifyScope(ctx, tx, m); err != nil {
		return Result{}, nil, progress, err
	}
	previous := signature
	verifiedAttempts := map[string]int64{}
	for next := int64(1); ; next++ {
		var cp batch.Checkpoint
		err = tx.QueryRow(ctx, `SELECT batch_no,last_key,row_count,committed_rows,row_hash,previous_signature,signature,COALESCE(attempt_execution_id,''),COALESCE(attempt_version,0) FROM _terserah.batch_checkpoints WHERE execution_id=$1 AND batch_no=$2`, m.ExecutionID, next).Scan(&cp.BatchNo, &cp.LastKey, &cp.RowCount, &cp.CommittedRows, &cp.RowHash, &cp.PreviousSignature, &cp.Signature, &cp.AttemptExecutionID, &cp.AttemptVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return Result{}, nil, progress, err
		}
		if cp.PreviousSignature != previous || cp.RowCount < 1 || cp.RowCount > int64(config.Size) || cp.CommittedRows != progress.CommittedRows+cp.RowCount || cp.CommittedRows > m.ScopeCount || !hmac.Equal([]byte(cp.Signature), []byte(batch.CheckpointSignature(s.key, signature, cp))) {
			return Result{}, nil, progress, errBatchEvidence
		}
		if cp.AttemptExecutionID != "" && cp.AttemptVersion < 1 {
			return Result{}, nil, progress, errBatchEvidence
		}
		if cp.AttemptExecutionID != "" && verifiedAttempts[cp.AttemptExecutionID] != cp.AttemptVersion {
			var valid bool
			err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history WHERE execution_id=$1 AND version=$2 AND database_id=$3 AND sql=$4 AND executed_by=$5 AND batch_root_execution_id=$6)`, cp.AttemptExecutionID, cp.AttemptVersion, dbID, m.SQL, m.ActorID, m.ExecutionID).Scan(&valid)
			if err != nil {
				return Result{}, nil, progress, err
			}
			if !valid {
				return Result{}, nil, progress, errBatchEvidence
			}
			verifiedAttempts[cp.AttemptExecutionID] = cp.AttemptVersion
		} else if cp.AttemptExecutionID == "" && cp.AttemptVersion != 0 {
			return Result{}, nil, progress, errBatchEvidence
		}
		var cursor *int64
		if progress.BatchNo > 0 {
			cursor = &progress.LastKey
		}
		keys, err := batchKeys(ctx, tx, m.ExecutionID, cursor, config.Size)
		if err != nil {
			return Result{}, nil, progress, err
		}
		if int64(len(keys)) != cp.RowCount || keys[len(keys)-1] != cp.LastKey {
			return Result{}, nil, progress, errBatchEvidence
		}
		rows, err := readRecoveryRows(ctx, tx, m.ExecutionID, next, keys)
		if err != nil {
			return Result{}, nil, progress, err
		}
		hash, err := batch.RowsHash(rows)
		if err != nil || hash != cp.RowHash {
			return Result{}, nil, progress, errBatchEvidence
		}
		progress = cp
		previous = cp.Signature
	}
	var checkpoints, images int64
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM _terserah.batch_checkpoints WHERE execution_id=$1),(SELECT count(*) FROM _terserah.batch_rows WHERE execution_id=$1)`, m.ExecutionID).Scan(&checkpoints, &images)
	if err != nil {
		return Result{}, nil, progress, err
	}
	if checkpoints != progress.BatchNo || images != progress.CommittedRows {
		return Result{}, nil, progress, errBatchEvidence
	}
	out, finished, err := s.verifyPendingAttempt(ctx, tx, dbID, item)
	if err != nil {
		return Result{}, nil, progress, err
	}
	if out.Status == "success" {
		if progress.CommittedRows != m.ScopeCount {
			return Result{}, nil, progress, errBatchEvidence
		}
		return out, finished, progress, nil
	}
	if out.Status == "unknown" || progress.CommittedRows == m.ScopeCount {
		return Result{}, nil, progress, errBatchEvidence
	}
	if progress.BatchNo > 0 {
		code := "reconciled_partial"
		out.Status = "partial"
		out.ErrorCode = &code
		finished = nil
	}
	return out, finished, progress, nil
}

func verifyScope(ctx context.Context, tx pgx.Tx, m batch.Manifest) error {
	rows, err := tx.Query(ctx, `SELECT row_key FROM _terserah.batch_scope WHERE execution_id=$1 ORDER BY row_key`, m.ExecutionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	hash := sha256.New()
	count := int64(0)
	for rows.Next() {
		var key int64
		if err = rows.Scan(&key); err != nil {
			return err
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(key))
		_, _ = hash.Write(encoded[:])
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != m.ScopeCount || hex.EncodeToString(hash.Sum(nil)) != m.ScopeHash {
		return errBatchEvidence
	}
	return nil
}

func readRecoveryRows(ctx context.Context, tx pgx.Tx, execution string, batchNo int64, keys []int64) ([]batch.Row, error) {
	rows, err := tx.Query(ctx, `SELECT row_key,
 CASE WHEN octet_length(before_data::text)<=8388608 THEN before_data ELSE NULL END,
 CASE WHEN octet_length(after_data::text)<=8388608 THEN after_data ELSE NULL END
 FROM _terserah.batch_rows WHERE execution_id=$1 AND batch_no=$2 ORDER BY row_key`, execution, batchNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]batch.Row, 0, len(keys))
	beforeBytes, afterBytes := 0, 0
	for rows.Next() {
		var row batch.Row
		if err = rows.Scan(&row.Key, &row.Before, &row.After); err != nil {
			return nil, err
		}
		beforeBytes += len(row.Before)
		afterBytes += len(row.After)
		if len(row.Before) == 0 || len(row.After) == 0 || beforeBytes > batch.MaxBytes || afterBytes > batch.MaxBytes || len(result) >= len(keys) || keys[len(result)] != row.Key {
			return nil, errBatchEvidence
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(keys) {
		return nil, errBatchEvidence
	}
	return result, nil
}
