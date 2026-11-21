package runner

import (
	"context"
	"crypto/hmac"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/policy"
)

type History struct {
	DatabaseID  int64      `json:"database_id,string"`
	DraftID     *int64     `json:"draft_id,string"`
	Revision    *int64     `json:"draft_revision,string"`
	ExecutionID *string    `json:"execution_id"`
	Version     int64      `json:"version,string"`
	SQL         string     `json:"sql,omitempty"`
	ExecutedBy  int64      `json:"executed_by,string"`
	Username    string     `json:"username"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	Status      string     `json:"status"`
	ErrorCode   *string    `json:"error_code"`
}

func (s *Service) listHistoryPage(ctx context.Context, dbID, before int64, limit int) ([]History, bool, *string, error) {
	items, err := s.queryHistory(ctx, dbID, 0, before, limit+1)
	if err != nil {
		return nil, false, nil, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	var next *string
	if more {
		value := strconv.FormatInt(items[len(items)-1].Version, 10)
		next = &value
	}
	return items, more, next, nil
}

func (s *Service) getHistoryDetail(ctx context.Context, dbID, version int64) (History, error) {
	items, err := s.queryHistory(ctx, dbID, version, 0, 1)
	if err != nil {
		return History{}, err
	}
	if len(items) == 0 {
		return History{}, pgx.ErrNoRows
	}
	return items[0], nil
}

func (s *Service) queryHistory(ctx context.Context, dbID, version, before int64, limit int) ([]History, error) {
	rows, err := s.db.Query(ctx, `
		SELECT
			h.database_id,
			h.draft_id,
			h.draft_revision,
			h.execution_id,
			h.version,
			CASE WHEN $2::bigint > 0 THEN h.sql ELSE '' END,
			h.executed_by,
			u.username,
			h.started_at,
			h.finished_at,
			h.status,
			h.error_code
		FROM migration_history h
		JOIN users u ON u.id = h.executed_by
		WHERE h.database_id = $1
		  AND ($2::bigint = 0 OR h.version = $2)
		  AND ($3::bigint = 0 OR h.version < $3)
		ORDER BY h.version DESC
		LIMIT $4
	`, dbID, version, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []History{}
	for rows.Next() {
		var item History
		err := rows.Scan(
			&item.DatabaseID,
			&item.DraftID,
			&item.Revision,
			&item.ExecutionID,
			&item.Version,
			&item.SQL,
			&item.ExecutedBy,
			&item.Username,
			&item.StartedAt,
			&item.FinishedAt,
			&item.Status,
			&item.ErrorCode,
		)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type pendingExecution struct {
	execution *string
	version   int64
	sql       string
	actor     int64
	format    int
	isBatch   bool
}

// reconcilePending checks `_terserah.migration_receipts` on the target DB for any
// migration_history rows left in 'running' or 'unknown' status.
func (s *Service) reconcilePending(ctx context.Context, p auth.Principal, dbID int64) ([]Result, error) {
	pool, err := s.targets.OpenTarget(ctx, p, dbID, true)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	// Receipt reads must see commits completed while waiting for the target lock.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer rollback(tx)

	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='2s'`); err != nil {
		return nil, err
	}
	if _, err = internalSearchPath(ctx, tx); err != nil {
		return nil, err
	}
	// Acquiring the same advisory lock used by `run` proves no in-flight runner
	// transaction can still commit a pending attempt while we inspect receipts.
	if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(3110003); SELECT pg_catalog.pg_advisory_xact_lock(3110004)`); err != nil {
		return nil, err
	}
	if err = protectedHistory(ctx, tx); err != nil {
		return nil, err
	}
	access, err := policy.DatabaseAccess(ctx, s.db, p.ID, dbID)
	if err != nil {
		return nil, err
	}
	if !policy.Allows(access, true) {
		return nil, connections.ErrForbidden
	}

	meta, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(meta)

	var locked int64
	if err = meta.QueryRow(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, dbID).Scan(&locked); err != nil {
		return nil, err
	}
	if err = bindTarget(ctx, meta, tx, dbID); err != nil {
		return nil, err
	}
	rows, err := meta.Query(ctx, `
		SELECT h.execution_id,h.version,h.sql,h.executed_by,h.receipt_version,
 EXISTS(SELECT 1 FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id WHERE a.execution_id=h.execution_id AND j.execution_mode='batch')
		FROM migration_history h
		WHERE h.database_id = $1 AND h.status IN ('running', 'unknown','partial')
		ORDER BY h.version
		LIMIT 100
		FOR UPDATE
	`, dbID)
	if err != nil {
		return nil, err
	}
	items := []pendingExecution{}
	for rows.Next() {
		var item pendingExecution
		if err = rows.Scan(&item.execution, &item.version, &item.sql, &item.actor, &item.format, &item.isBatch); err != nil {
			break
		}
		items = append(items, item)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return nil, err
	}

	results := []Result{}
	for _, item := range items {
		var out Result
		var finished *time.Time
		var progress batch.Checkpoint
		if item.isBatch {
			out, finished, progress, err = s.verifyBatchPending(ctx, tx, dbID, item)
		} else {
			out, finished, err = s.verifyPendingAttempt(ctx, tx, dbID, item)
		}
		if err != nil {
			return nil, err
		}
		_, err = meta.Exec(ctx, `
			UPDATE migration_history
			SET status = $1, error_code = $2, finished_at = $3
			WHERE database_id = $4 AND version = $5 AND status IN ('running', 'unknown','partial')
		`, out.Status, out.ErrorCode, finished, dbID, item.version)
		if err != nil {
			return nil, err
		}
		if item.isBatch || out.Status == "success" || out.Status == "failed" {
			outcome := jobs.Outcome{Status: jobs.Status(out.Status)}
			if out.ErrorCode != nil {
				outcome.ErrorCode = *out.ErrorCode
			}
			if item.isBatch {
				err = jobs.ResolveBatchTx(ctx, meta, out.ExecutionID, outcome, progress.BatchNo, progress.CommittedRows)
			} else {
				err = jobs.ResolveUnknownTx(ctx, meta, out.ExecutionID, outcome)
			}
			if err != nil {
				return nil, err
			}
		}
		results = append(results, out)
	}
	if err = meta.Commit(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Service) verifyPendingAttempt(ctx context.Context, targetTx pgx.Tx, dbID int64, item pendingExecution) (Result, *time.Time, error) {
	out := Result{Version: item.version, Status: "unknown"}
	code := "legacy_receipt_unverified"
	var finished *time.Time

	if item.execution != nil {
		out.ExecutionID = *item.execution
	}
	if item.format == 1 && item.execution != nil {
		var receiptVersion, actor int64
		var sql string
		var signature *string
		var committedAt time.Time
		err := targetTx.QueryRow(ctx, `
			SELECT version, sql, executed_by, committed_at, signature
			FROM _terserah.migration_receipts
			WHERE execution_id = $1
		`, out.ExecutionID).Scan(&receiptVersion, &sql, &actor, &committedAt, &signature)

		expectedSig := s.signature(dbID, out.ExecutionID, item.version, item.sql, item.actor)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			out.Status = "failed"
			code = "reconciled_rollback"
			now := time.Now().UTC()
			finished = &now
		case err != nil:
			return Result{}, nil, err
		case signature != nil &&
			receiptVersion == item.version &&
			sql == item.sql &&
			actor == item.actor &&
			hmac.Equal([]byte(*signature), []byte(expectedSig)):
			out.Status = "success"
			code = ""
			finished = &committedAt
		default:
			code = "receipt_mismatch"
		}
	}
	if code != "" {
		out.ErrorCode = &code
	}
	return out, finished, nil
}
