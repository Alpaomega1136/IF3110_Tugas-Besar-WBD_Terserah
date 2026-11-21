package runner

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/telemetry"
)

var errBatchCommitUnknown = errors.New("batch COMMIT result is unknown")

func beginBatch(ctx context.Context, pool *pgxpool.Pool, isolation pgx.TxIsoLevel) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: isolation})
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='2s'; SET LOCAL standard_conforming_strings=on; SET LOCAL search_path=pg_catalog,pg_temp; SELECT pg_catalog.pg_advisory_xact_lock(3110004)`)
	if err != nil {
		rollback(tx)
		return nil, err
	}
	return tx, nil
}

func (s *Service) runBatch(ctx context.Context, pool *pgxpool.Pool, guard *pgx.Conn, c *jobs.Claim) (Result, error) {
	config, err := batch.Parse(c.SQL)
	if err != nil || config == nil {
		return s.preflightFailure(ctx, c, errSQL)
	}
	tx, err := beginBatch(ctx, pool, pgx.RepeatableRead)
	if err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	defer rollback(tx)
	if err = protectedBatchHistory(ctx, tx); err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	oid, err := batchTable(ctx, tx, *config)
	if err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	result, err := s.reserveJob(ctx, c)
	if err != nil {
		return result, err
	}
	if err = s.validateJob(ctx, tx, c); err != nil {
		rollback(tx)
		return s.finishBatch(ctx, result, "failed", executionCode(err)), nil
	}
	manifest := batch.Manifest{ExecutionID: c.ExecutionID, DatabaseID: c.Job.DatabaseID, Version: result.Version, ActorID: c.Job.RequestedBy, SQL: c.SQL, TableOID: oid}
	if err = s.freezeBatchScope(ctx, tx, *config, &manifest); err != nil {
		rollback(tx)
		return s.finishBatch(ctx, result, "failed", executionCode(err)), nil
	}
	if manifest.ScopeCount == 0 {
		err = s.appendBatchReceipt(ctx, tx, c, result.Version)
	}
	if err == nil {
		err = s.batchLease(ctx, guard, c)
	}
	if err == nil {
		err = commitBatch(ctx, tx)
	}
	if err != nil {
		rollback(tx)
		status, code := batchFailure(err, 0)
		return s.finishBatch(ctx, result, status, code), nil
	}
	if manifest.ScopeCount == 0 {
		return s.finishBatch(ctx, result, "success", ""), nil
	}
	previous := batch.Checkpoint{PreviousSignature: batch.ManifestSignature(s.key, manifest), Signature: batch.ManifestSignature(s.key, manifest)}
	for {
		cp, last, err := s.executeBatch(ctx, pool, guard, c, *config, manifest, previous)
		if err != nil {
			status, code := batchFailure(err, previous.BatchNo)
			return s.finishBatch(ctx, result, status, code), nil
		}
		if err = jobs.New(s.db).ReportProgress(ctx, c, cp.BatchNo, cp.CommittedRows); err != nil {
			return s.finishBatch(ctx, result, "unknown", "progress_unavailable"), nil
		}
		telemetry.Event(ctx, "batch_committed", "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "execution_id", c.ExecutionID, "batch", cp.BatchNo, "committed_rows", cp.CommittedRows, "status", "running")
		if last {
			return s.finishBatch(ctx, result, "success", ""), nil
		}
		previous = cp
	}
}

// Both scans use one Repeatable Read snapshot; only a streaming hash enters Go memory.
func (s *Service) freezeBatchScope(ctx context.Context, tx pgx.Tx, config batch.Config, m *batch.Manifest) error {
	key := pgx.Identifier{config.Key}.Sanitize()
	rows, err := tx.Query(ctx, "SELECT "+key+" FROM "+config.Relation()+" ORDER BY "+key)
	if err != nil {
		return err
	}
	hash := sha256.New()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(id))
		_, _ = hash.Write(encoded[:])
		m.ScopeCount++
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return err
	}
	m.ScopeHash = hex.EncodeToString(hash.Sum(nil))
	_, err = tx.Exec(ctx, `INSERT INTO _terserah.batch_runs(execution_id,database_id,version,executed_by,sql,table_oid,scope_count,scope_hash,signature) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, m.ExecutionID, m.DatabaseID, m.Version, m.ActorID, m.SQL, m.TableOID, m.ScopeCount, m.ScopeHash, batch.ManifestSignature(s.key, *m))
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, "INSERT INTO _terserah.batch_scope(execution_id,row_key) SELECT $1,"+key+" FROM "+config.Relation(), m.ExecutionID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != m.ScopeCount {
		return errBatchScope
	}
	return nil
}

func (s *Service) executeBatch(ctx context.Context, pool *pgxpool.Pool, guard *pgx.Conn, c *jobs.Claim, config batch.Config, m batch.Manifest, previous batch.Checkpoint) (checkpoint batch.Checkpoint, isLast bool, returnErr error) {
	cp := batch.Checkpoint{BatchNo: previous.BatchNo + 1, PreviousSignature: previous.Signature}
	ctx, span := telemetry.Start(ctx, "migration.batch", trace.WithAttributes(attribute.Int64("batch.number", cp.BatchNo)))
	defer func() {
		if returnErr != nil {
			telemetry.Failure(span, executionCode(returnErr))
		}
		span.End()
	}()
	if err := s.batchLease(ctx, guard, c); err != nil {
		return cp, false, err
	}
	tx, err := beginBatch(ctx, pool, pgx.ReadCommitted)
	if err != nil {
		telemetry.Failure(span, executionCode(err))
		return cp, false, err
	}
	defer rollback(tx)
	oid, err := batchTable(ctx, tx, config)
	if err != nil || oid != m.TableOID {
		telemetry.Failure(span, "batch_table_changed")
		return cp, false, errBatchTable
	}
	var cursor *int64
	if previous.BatchNo > 0 {
		cursor = &previous.LastKey
	}
	keys, err := batchKeys(ctx, tx, c.ExecutionID, cursor, config.Size+1)
	if err != nil {
		return cp, false, err
	}
	if len(keys) == 0 {
		return cp, false, errBatchScope
	}
	last := len(keys) <= config.Size
	if !last {
		keys = keys[:config.Size]
	}
	cp.LastKey = keys[len(keys)-1]
	recovery, err := loadBatchRows(ctx, tx, config, keys)
	if err != nil {
		return cp, false, err
	}
	started := time.Now()
	rows, err := tx.Query(ctx, config.Query(), keys, config.Values())
	if err != nil {
		telemetry.Failure(span, executionCode(err))
		return cp, false, err
	}
	index := make(map[int64]int, len(keys))
	for i, k := range keys {
		index[k] = i
	}
	seen := make(map[int64]bool, len(keys))
	bytes := 0
	for rows.Next() {
		var key int64
		var after json.RawMessage
		if err = rows.Scan(&key, &after); err != nil {
			break
		}
		i, ok := index[key]
		bytes += len(after)
		if !ok || seen[key] {
			err = errBatchScope
			break
		}
		if len(after) == 0 || bytes > batch.MaxBytes {
			err = errBatchPayload
			break
		}
		seen[key] = true
		recovery[i].After = after
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	telemetry.Event(ctx, "batch_sql_executed", "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "execution_id", c.ExecutionID, "batch", cp.BatchNo, "row_count", len(seen), "duration_ms", time.Since(started).Milliseconds(), "query", "UPDATE <approved_table> SET <approved_values> WHERE <key>=ANY($1)", "parameters", "[redacted]", "error_code", safeBatchCode(err))
	if err != nil {
		telemetry.Failure(span, executionCode(err))
		return cp, false, err
	}
	if len(seen) != len(keys) {
		return cp, false, errBatchScope
	}
	cp.RowCount = int64(len(keys))
	cp.CommittedRows = previous.CommittedRows + cp.RowCount
	if cp.CommittedRows > m.ScopeCount || (last && cp.CommittedRows != m.ScopeCount) {
		return cp, false, errBatchScope
	}
	cp.RowHash, err = batch.RowsHash(recovery)
	if err != nil {
		return cp, false, err
	}
	cp.Signature = batch.CheckpointSignature(s.key, batch.ManifestSignature(s.key, m), cp)
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"_terserah", "batch_rows"}, []string{"execution_id", "batch_no", "row_key", "before_data", "after_data"}, pgx.CopyFromSlice(len(recovery), func(i int) ([]any, error) {
		r := recovery[i]
		return []any{c.ExecutionID, cp.BatchNo, r.Key, r.Before, r.After}, nil
	}))
	if err != nil {
		return cp, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO _terserah.batch_checkpoints(execution_id,batch_no,last_key,row_count,committed_rows,row_hash,previous_signature,signature) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, c.ExecutionID, cp.BatchNo, cp.LastKey, cp.RowCount, cp.CommittedRows, cp.RowHash, cp.PreviousSignature, cp.Signature)
	if err != nil {
		return cp, false, err
	}
	if last {
		var scopeCount int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_scope WHERE execution_id=$1`, c.ExecutionID).Scan(&scopeCount); err != nil {
			return cp, false, err
		}
		if scopeCount != m.ScopeCount {
			return cp, false, errBatchScope
		}
		if err = s.appendBatchReceipt(ctx, tx, c, m.Version); err != nil {
			return cp, false, err
		}
	}
	if err = s.batchLease(ctx, guard, c); err != nil {
		return cp, false, err
	}
	if err = commitBatch(ctx, tx); err != nil {
		telemetry.Failure(span, "batch_commit_failed")
		return cp, false, err
	}
	span.SetAttributes(attribute.Int64("batch.rows", cp.RowCount))
	return cp, last, nil
}

func batchKeys(ctx context.Context, tx pgx.Tx, execution string, cursor *int64, limit int) ([]int64, error) {
	query := `SELECT row_key FROM _terserah.batch_scope WHERE execution_id=$1 ORDER BY row_key LIMIT $2`
	args := []any{execution, limit}
	if cursor != nil {
		query = `SELECT row_key FROM _terserah.batch_scope WHERE execution_id=$1 AND row_key>$2 ORDER BY row_key LIMIT $3`
		args = []any{execution, *cursor, limit}
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []int64{}
	for rows.Next() {
		var key int64
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func loadBatchRows(ctx context.Context, tx pgx.Tx, c batch.Config, keys []int64) ([]batch.Row, error) {
	column := pgx.Identifier{c.Key}.Sanitize()
	rows, err := tx.Query(ctx, "SELECT target."+column+", CASE WHEN pg_catalog.octet_length(pg_catalog.to_jsonb(target)::text)<="+"8388608"+" THEN pg_catalog.to_jsonb(target) ELSE NULL END FROM "+c.Relation()+" AS target WHERE target."+column+"=ANY($1::bigint[]) ORDER BY target."+column+" FOR UPDATE", keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]batch.Row, 0, len(keys))
	bytes := 0
	for rows.Next() {
		var row batch.Row
		if err = rows.Scan(&row.Key, &row.Before); err != nil {
			return nil, err
		}
		bytes += len(row.Before)
		if len(row.Before) == 0 || bytes > batch.MaxBytes {
			return nil, errBatchPayload
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != len(keys) {
		return nil, errBatchScope
	}
	for i := range result {
		if result[i].Key != keys[i] {
			return nil, errBatchScope
		}
	}
	return result, nil
}

func (s *Service) batchLease(ctx context.Context, guard *pgx.Conn, c *jobs.Claim) error {
	if err := guard.Ping(ctx); err != nil {
		return jobs.ErrLeaseLost
	}
	return jobs.New(s.db).Heartbeat(ctx, c, 30*time.Second)
}

func commitBatch(ctx context.Context, tx pgx.Tx) error {
	err := tx.Commit(ctx)
	if err == nil {
		return nil
	}
	var pgerr *pgconn.PgError
	if errors.Is(err, pgx.ErrTxCommitRollback) || (errors.As(err, &pgerr) && pgerr.Severity == "ERROR" && (pgerr.Code == "40001" || pgerr.Code == "40P01" || (len(pgerr.Code) == 5 && pgerr.Code[:2] == "23"))) {
		return err
	}
	return errors.Join(errBatchCommitUnknown, err)
}

func batchFailure(err error, committed int64) (string, string) {
	if errors.Is(err, errBatchCommitUnknown) {
		return "unknown", "commit_unknown"
	}
	if committed > 0 {
		return "partial", executionCode(err)
	}
	return "failed", executionCode(err)
}

func safeBatchCode(err error) string {
	if err == nil {
		return ""
	}
	return executionCode(err)
}

func (s *Service) appendBatchReceipt(ctx context.Context, tx pgx.Tx, c *jobs.Claim, version int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO _terserah.migration_receipts(execution_id,version,sql,executed_by,signature) VALUES($1,$2,$3,$4,$5)`, c.ExecutionID, version, c.SQL, c.Job.RequestedBy, s.signature(c.Job.DatabaseID, c.ExecutionID, version, c.SQL, c.Job.RequestedBy))
	return err
}

func (s *Service) finishBatch(ctx context.Context, result Result, status, code string) Result {
	result.Status = status
	if code != "" {
		result.ErrorCode = &code
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	updated, err := s.db.Exec(cleanup, `UPDATE migration_history SET status=$1,error_code=NULLIF($2,''),finished_at=CASE WHEN $1 IN ('success','failed') THEN clock_timestamp() ELSE NULL END WHERE execution_id=$3 AND status IN ('running','unknown','partial')`, status, code, result.ExecutionID)
	if err != nil || updated.RowsAffected() != 1 {
		result.Status = "unknown"
		code = "ledger_unavailable"
		result.ErrorCode = &code
	}
	telemetry.Event(ctx, "execution_finished", "execution_id", result.ExecutionID, "version", result.Version, "status", result.Status, "error_code", code)
	return result
}
