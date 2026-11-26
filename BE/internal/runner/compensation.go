package runner

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/metrics"
	"terserah/backend/internal/telemetry"
)

func (s *Service) runCompensation(ctx context.Context, pool *pgxpool.Pool, guard *pgx.Conn, c *jobs.Claim) (Result, error) {
	result, err := s.reserveJob(ctx, c)
	if err != nil {
		return result, err
	}
	fail := func(err error) Result {
		status, code := batchFailure(err, 1)
		if status != "unknown" {
			status = "partially_reverted"
		}
		return s.finishBatch(ctx, result, status, code)
	}
	tx, err := beginBatch(ctx, pool, pgx.RepeatableRead)
	if err != nil {
		return fail(err), nil
	}
	defer rollback(ctx, tx)
	if err = s.validateJob(ctx, tx, c); err != nil {
		return fail(err), nil
	}
	item := pendingExecution{execution: &c.ExecutionID, version: c.Version, sql: c.SQL, actor: c.Job.RequestedBy, format: 1}
	m, previous, exists, err := s.compensationEvidence(ctx, tx, c.Job.DatabaseID, item, c.RootExecutionID, c.CompensationRoot)
	if err != nil {
		return fail(err), nil
	}
	config, err := batch.ParseDown(c.SQL)
	if err != nil || config == nil {
		return fail(errBatchEvidence), nil
	}
	if previous.BatchNo < c.Job.CommittedBatches || previous.CommittedRows < c.Job.CommittedRows {
		return fail(errBatchEvidence), nil
	}
	if exists && previous.BatchNo == m.Batches {
		out, _, _, verifyErr := s.verifyCompensationPending(ctx, tx, c.Job.DatabaseID, item, c.RootExecutionID, c.CompensationRoot)
		if verifyErr != nil || out.Status != "success" {
			return s.finishBatch(ctx, result, "unknown", "batch_evidence_invalid"), nil
		}
		return s.finishBatch(ctx, result, "success", ""), nil
	}
	if !exists {
		_, err = tx.Exec(ctx, `INSERT INTO _terserah.compensation_runs(execution_id,source_execution_id,source_signature,database_id,version,executed_by,sql,total_batches,total_rows,signature) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, m.ExecutionID, m.SourceExecutionID, m.SourceSignature, m.DatabaseID, m.Version, m.ActorID, m.SQL, m.Batches, m.Rows, batch.CompensationSignature(s.key, m))
		if err != nil {
			return fail(err), nil
		}
	}
	if m.Batches == 0 {
		if err = s.completeCompensation(ctx, tx, c, m); err != nil {
			return fail(err), nil
		}
	}
	if err = s.batchLease(ctx, guard, c); err != nil {
		return fail(err), nil
	}
	if err = commitBatch(ctx, tx); err != nil {
		return fail(err), nil
	}
	if m.Batches == 0 {
		return s.finishBatch(ctx, result, "success", ""), nil
	}
	if previous.BatchNo == 0 {
		previous.Signature = batch.CompensationSignature(s.key, m)
	}
	for previous.BatchNo < m.Batches {
		cp, err := s.compensateBatch(ctx, pool, guard, c, *config, m, previous)
		if err != nil {
			return fail(err), nil
		}
		if err = jobs.New(s.db).ReportProgress(ctx, c, cp.BatchNo, cp.CommittedRows); err != nil {
			return s.finishBatch(ctx, result, "unknown", "progress_unavailable"), nil
		}
		telemetry.Event(ctx, "compensation_batch_committed", "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "execution_id", c.ExecutionID, "batch", cp.BatchNo, "original_batch", cp.LastKey, "committed_rows", cp.CommittedRows, "status", "running")
		previous = cp
	}
	return s.finishBatch(ctx, result, "success", ""), nil
}

func (s *Service) compensateBatch(ctx context.Context, pool *pgxpool.Pool, guard *pgx.Conn, c *jobs.Claim, config batch.Config, m batch.CompensationManifest, previous batch.Checkpoint) (batch.Checkpoint, error) {
	cp := batch.Checkpoint{BatchNo: previous.BatchNo + 1, LastKey: m.Batches - previous.BatchNo, PreviousSignature: previous.Signature, AttemptExecutionID: c.ExecutionID, AttemptVersion: c.Version}
	ctx = telemetry.WithFields(ctx, "batch", cp.BatchNo, "original_batch", cp.LastKey)
	ctx, span := telemetry.Start(ctx, "migration.compensation.batch")
	defer span.End()
	fail := func(err error) (batch.Checkpoint, error) { telemetry.Failure(span, executionCode(err)); return cp, err }
	if err := s.batchLease(ctx, guard, c); err != nil {
		return fail(err)
	}
	tx, err := beginBatch(ctx, pool, pgx.ReadCommitted)
	if err != nil {
		return fail(err)
	}
	defer rollback(ctx, tx)
	oid, err := batchTable(ctx, tx, config)
	if err != nil {
		return fail(err)
	}
	original, exists, err := loadBatchManifest(ctx, tx, m.SourceExecutionID)
	if err != nil || !exists || !hmac.Equal([]byte(batch.ManifestSignature(s.key, original)), []byte(m.SourceSignature)) {
		return fail(errBatchEvidence)
	}
	if oid != original.TableOID {
		return fail(errBatchTable)
	}
	rows, err := tx.Query(ctx, `SELECT row_key FROM _terserah.batch_rows WHERE execution_id=$1 AND batch_no=$2 ORDER BY row_key LIMIT $3`, m.SourceExecutionID, cp.LastKey, config.Size+1)
	if err != nil {
		return fail(err)
	}
	keys := []int64{}
	for rows.Next() {
		var key int64
		if err = rows.Scan(&key); err != nil {
			break
		}
		keys = append(keys, key)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return fail(err)
	}
	if len(keys) == 0 || len(keys) > config.Size {
		return fail(errBatchEvidence)
	}
	images, err := readRecoveryRows(ctx, tx, m.SourceExecutionID, cp.LastKey, keys)
	if err != nil {
		return fail(err)
	}
	cp.RowHash, err = batch.RowsHash(images)
	if err != nil {
		return fail(err)
	}
	var signed batch.Checkpoint
	err = tx.QueryRow(ctx, `SELECT batch_no,last_key,row_count,committed_rows,row_hash,previous_signature,signature,COALESCE(attempt_execution_id,''),COALESCE(attempt_version,0) FROM _terserah.batch_checkpoints WHERE execution_id=$1 AND batch_no=$2`, m.SourceExecutionID, cp.LastKey).Scan(&signed.BatchNo, &signed.LastKey, &signed.RowCount, &signed.CommittedRows, &signed.RowHash, &signed.PreviousSignature, &signed.Signature, &signed.AttemptExecutionID, &signed.AttemptVersion)
	if err != nil || signed.BatchNo != cp.LastKey || !hmac.Equal([]byte(signed.Signature), []byte(batch.CheckpointSignature(s.key, m.SourceSignature, signed))) || cp.RowHash != signed.RowHash || signed.RowCount != int64(len(images)) {
		return fail(errBatchEvidence)
	}
	type image struct {
		Before json.RawMessage `json:"before"`
		After  json.RawMessage `json:"after"`
	}
	payload := make([]image, len(images))
	for i, r := range images {
		payload[i] = image{r.Before, r.After}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fail(err)
	}
	started := time.Now()
	rows, err = tx.Query(ctx, c.SQL, data)
	if err != nil {
		return fail(err)
	}
	seen := map[int64]bool{}
	allowed := map[int64]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	bytes := 0
	for rows.Next() {
		var key int64
		var restored json.RawMessage
		var exact bool
		if err = rows.Scan(&key, &restored, &exact); err != nil {
			break
		}
		bytes += len(restored)
		if !allowed[key] || seen[key] || !exact || len(restored) == 0 || bytes > batch.MaxBytes {
			err = errBatchScope
			break
		}
		seen[key] = true
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	telemetry.Event(ctx, "compensation_sql_executed", "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "execution_id", c.ExecutionID, "row_count", len(seen), "duration_ms", time.Since(started).Milliseconds(), "parameters", "[redacted]", "error_code", safeBatchCode(err))
	if err != nil {
		return fail(err)
	}
	if len(seen) != len(keys) {
		return fail(errBatchScope)
	}
	cp.RowCount = int64(len(keys))
	cp.CommittedRows = previous.CommittedRows + cp.RowCount
	cp.Signature = batch.CheckpointSignature(s.key, batch.CompensationSignature(s.key, m), cp)
	if _, err = tx.Exec(ctx, `INSERT INTO _terserah.compensation_checkpoints(execution_id,batch_no,original_batch_no,row_count,committed_rows,row_hash,previous_signature,signature,attempt_execution_id,attempt_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, m.ExecutionID, cp.BatchNo, cp.LastKey, cp.RowCount, cp.CommittedRows, cp.RowHash, cp.PreviousSignature, cp.Signature, c.ExecutionID, c.Version); err != nil {
		return fail(err)
	}
	if cp.BatchNo == m.Batches {
		if cp.CommittedRows != m.Rows {
			return fail(errBatchEvidence)
		}
		if err = s.completeCompensation(ctx, tx, c, m); err != nil {
			return fail(err)
		}
	}
	if err = s.batchLease(ctx, guard, c); err != nil {
		return fail(err)
	}
	if err = commitBatch(ctx, tx); err != nil {
		return fail(err)
	}
	metrics.BatchCommitted(true, cp.RowCount)
	return cp, nil
}

func (s *Service) completeCompensation(ctx context.Context, tx pgx.Tx, c *jobs.Claim, m batch.CompensationManifest) error {
	item := pendingExecution{execution: &c.ExecutionID, version: c.Version, sql: c.SQL, actor: c.Job.RequestedBy, format: 1}
	verified, progress, exists, err := s.compensationEvidence(ctx, tx, c.Job.DatabaseID, item, c.RootExecutionID, c.CompensationRoot)
	if err != nil {
		return err
	}
	if !exists || verified != m || progress.BatchNo != m.Batches || progress.CommittedRows != m.Rows {
		return errBatchEvidence
	}
	root := *c
	root.ExecutionID = m.ExecutionID
	root.SQL = m.SQL
	root.Job.RequestedBy = m.ActorID
	if err := s.appendBatchReceipt(ctx, tx, &root, m.Version); err != nil {
		return err
	}
	if c.ExecutionID != m.ExecutionID {
		return s.appendBatchReceipt(ctx, tx, c, c.Version)
	}
	return nil
}
