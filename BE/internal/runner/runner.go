package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/telemetry"
)

type Service struct {
	db      *pgxpool.Pool
	targets *connections.Service
	key     []byte
}

func New(db *pgxpool.Pool, targets *connections.Service, key []byte) *Service {
	return &Service{db: db, targets: targets, key: append([]byte(nil), key...)}
}

type Draft struct {
	ID                int64     `json:"id,string"`
	DatabaseID        int64     `json:"database_id,string"`
	Title             string    `json:"title"`
	SQL               string    `json:"sql"`
	Revision          int64     `json:"revision,string"`
	CreatedBy         int64     `json:"created_by,string"`
	CreatedByUsername string    `json:"created_by_username"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type draftInput struct {
	Title    string        `json:"title"`
	SQL      string        `json:"sql"`
	Revision int64         `json:"revision,string"`
	Batch    *batch.Config `json:"batch,omitempty"`
}

type Result struct {
	ExecutionID string  `json:"execution_id"`
	Version     int64   `json:"version,string"`
	Status      string  `json:"status"`
	ErrorCode   *string `json:"error_code"`
}

var errConflict = errors.New("draft revision changed or an unresolved execution exists")
var errActiveJob = errors.New("a worker still owns a live execution lease")

func rollback(tx pgx.Tx) {
	if tx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *Service) execStatementsWithReceipt(ctx context.Context, targetTx pgx.Tx, userPath string, parts []string, dbID int64, executionID string, version int64, sqlText string, actorID int64) error {
	_, err := targetTx.Exec(ctx, `SELECT pg_catalog.set_config('search_path', $1, true)`, userPath)
	for index, part := range parts {
		if err != nil {
			break
		}
		// pgx.Tx.Exec falls back to simple protocol for parameterless SQL.
		// ExecParams forces one server-parsed command and discards returned rows.
		statementCtx, span := sqlSpan(ctx, index+1)
		started := time.Now()
		_, err = targetTx.Conn().PgConn().ExecParams(statementCtx, part, nil, nil, nil, nil).Close()
		code := ""
		if err != nil {
			code = executionCode(err)
			telemetry.Failure(span, code)
		}
		telemetry.Event(statementCtx, "sql_executed", "actor_id", actorID, "database_id", dbID, "execution_id", executionID, "statement_index", index+1, "duration_ms", time.Since(started).Milliseconds(), "error_code", code)
		span.End()
	}
	if err == nil {
		_, err = targetTx.Exec(ctx, `SET LOCAL search_path=pg_catalog,pg_temp`)
	}
	if err == nil {
		sig := s.signature(dbID, executionID, version, sqlText, actorID)
		_, err = targetTx.Exec(ctx, `
			INSERT INTO _terserah.migration_receipts (execution_id, version, sql, executed_by, signature)
			VALUES ($1, $2, $3, $4, $5)
		`, executionID, version, sqlText, actorID, sig)
	}
	return err
}

func (s *Service) commitAndFinalizeLedger(ctx context.Context, targetTx pgx.Tx, execErr error, result *Result) {
	ctx, span := telemetry.Start(ctx, "migration.finalize")
	defer span.End()
	if execErr != nil {
		rollback(targetTx)
		result.Status = "failed"
		code := executionCode(execErr)
		result.ErrorCode = &code
	} else if commitErr := targetTx.Commit(ctx); commitErr != nil {
		var pgerr *pgconn.PgError
		code := "commit_unknown"
		if errors.As(commitErr, &pgerr) && pgerr.Severity == "ERROR" &&
			(strings.HasPrefix(pgerr.Code, "23") || pgerr.Code == "40001" || pgerr.Code == "40P01") {
			// Constraint/serialization failures confirm abort; connection failures do not.
			result.Status = "failed"
			code = pgerr.Code
		} else if errors.Is(commitErr, pgx.ErrTxCommitRollback) {
			result.Status = "failed"
			code = "execution_failed"
		} else {
			// A lost COMMIT response cannot be interpreted as rollback or retried automatically.
			result.Status = "unknown"
		}
		result.ErrorCode = &code
	} else {
		result.Status = "success"
	}

	cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	updated, err := s.db.Exec(cleanup, `
		UPDATE migration_history
		SET status = $1,
		    error_code = $2,
		    finished_at = CASE WHEN $1 IN ('success', 'failed') THEN now() ELSE NULL END
		WHERE execution_id = $3 AND status IN ('running', 'unknown')
 AND EXISTS(SELECT 1 FROM migration_job_attempts a JOIN migration_jobs j ON j.id=a.job_id
 WHERE a.execution_id=$3 AND a.attempt_no=j.attempt_count AND NOT a.retry_scheduled AND j.status='running' AND j.lease_expires_at>clock_timestamp())
	`, result.Status, result.ErrorCode, result.ExecutionID)
	if err != nil || updated.RowsAffected() != 1 {
		result.Status = "unknown"
		code := "ledger_unavailable"
		result.ErrorCode = &code
	}
	telemetry.Event(ctx, "execution_finished", "execution_id", result.ExecutionID, "version", result.Version, "status", result.Status, "error_code", result.ErrorCode)
	if result.ErrorCode != nil {
		telemetry.Failure(span, *result.ErrorCode)
	}
}
