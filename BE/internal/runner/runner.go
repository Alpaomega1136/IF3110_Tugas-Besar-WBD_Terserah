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
	ID                 int64     `json:"id,string"`
	DatabaseID         int64     `json:"database_id,string"`
	Title              string    `json:"title"`
	SQL                string    `json:"sql"`
	SQLDown            *string   `json:"sql_down"`
	Irreversible       bool      `json:"irreversible"`
	IrreversibleReason *string   `json:"irreversible_reason"`
	Immutable          bool      `json:"immutable"`
	Revision           int64     `json:"revision,string"`
	CreatedBy          int64     `json:"created_by,string"`
	CreatedByUsername  string    `json:"created_by_username"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type draftInput struct {
	Title              string        `json:"title"`
	SQL                string        `json:"sql"`
	SQLDown            *string       `json:"sql_down"`
	Irreversible       bool          `json:"irreversible"`
	IrreversibleReason *string       `json:"irreversible_reason"`
	Revision           int64         `json:"revision,string"`
	Batch              *batch.Config `json:"batch,omitempty"`
}

type Result struct {
	ExecutionID string  `json:"execution_id"`
	Version     int64   `json:"version,string"`
	Status      string  `json:"status"`
	ErrorCode   *string `json:"error_code"`
}

var errConflict = errors.New("draft revision changed or an unresolved execution exists")
var errActiveJob = errors.New("a worker still owns a live execution lease")
var errRetryInvalid = errors.New("retry hanya diizinkan untuk execution Up yang telah gagal")

func rollback(parent context.Context, tx pgx.Tx) {
	if tx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Second)
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
		queryCtx, finish := (telemetry.SQLTracer{Role: "target", DatabaseID: dbID}).BeginSQL(statementCtx, part, 0)
		tag, queryErr := targetTx.Conn().PgConn().ExecParams(queryCtx, part, nil, nil, nil, nil).Close()
		err = queryErr
		finish(tag, err)
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
		rollback(ctx, targetTx)
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

	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	var finished *time.Time
	if result.Status == "success" || result.Status == "failed" {
		now := time.Now().UTC()
		finished = &now
	}
	err := s.finalisasiAttempt(cleanup, *result, finished)
	if err != nil {
		result.Status = "unknown"
		code := "ledger_unavailable"
		result.ErrorCode = &code
	}
	telemetry.Event(ctx, "execution_finished", "execution_id", result.ExecutionID, "version", result.Version, "status", result.Status, "error_code", result.ErrorCode)
	if result.ErrorCode != nil {
		telemetry.Failure(span, *result.ErrorCode)
	}
}
