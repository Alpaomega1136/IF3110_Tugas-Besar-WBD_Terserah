package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/telemetry"
)

// ExecuteJob runs only a fenced queue claim; HTTP handlers never execute SQL.
func (s *Service) ExecuteJob(ctx context.Context, c *jobs.Claim) (Result, error) {
	if c == nil {
		return Result{}, jobs.ErrInvalid
	}
	p := auth.Principal{ID: c.Job.RequestedBy}
	pool, err := s.targets.OpenTarget(ctx, p, c.Job.DatabaseID, true)
	if err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	defer pool.Close()
	acquired, err := pool.Acquire(ctx)
	if err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	// Closing the hijacked connection releases every session lock; it never returns to a pool.
	conn := acquired.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	lockCtx, lockSpan := telemetry.Start(ctx, "migration.lock")
	started := time.Now()
	_, err = conn.Exec(lockCtx, `SET statement_timeout='5min'; SET lock_timeout='2s'; SET standard_conforming_strings=on; SELECT pg_catalog.pg_advisory_lock(3110003)`)
	if err != nil {
		telemetry.Failure(lockSpan, executionCode(err))
		lockSpan.End()
		return s.preflightFailure(ctx, c, err)
	}
	lockSpan.End()
	telemetry.Event(ctx, "target_lock_acquired", "job_id", c.Job.ID, "database_id", c.Job.DatabaseID, "wait_ms", time.Since(started).Milliseconds())
	if c.Job.ExecutionMode == "batch" {
		return s.runBatch(ctx, pool, conn, c)
	}
	targetTx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	defer rollback(ctx, targetTx)
	path, err := internalSearchPath(ctx, targetTx)
	// Transaction locks cannot be released by migration SQL calling pg_advisory_unlock_all.
	if err == nil {
		_, err = targetTx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(3110003); SELECT pg_catalog.pg_advisory_xact_lock(3110004)`)
	}
	if err == nil {
		err = protectedHistory(ctx, targetTx)
	}
	if err != nil {
		return s.preflightFailure(ctx, c, err)
	}
	result, err := s.reserveJob(ctx, c)
	if err != nil {
		return result, err
	}
	err = s.validateJob(ctx, targetTx, c)
	var parts []string
	if err == nil {
		parts, err = statements(c.SQL)
	}
	if err == nil {
		err = s.execStatementsWithReceipt(ctx, targetTx, path, parts, c.Job.DatabaseID, c.ExecutionID, result.Version, c.SQL, p.ID)
	}
	// Extend and check the lease immediately before COMMIT, including after long SQL.
	if err == nil {
		err = jobs.New(s.db).Heartbeat(ctx, c, 30*time.Second)
	}
	s.commitAndFinalizeLedger(ctx, targetTx, err, &result)
	return result, nil
}

func (s *Service) preflightFailure(ctx context.Context, c *jobs.Claim, cause error) (Result, error) {
	if ctx.Err() != nil {
		cause = ctx.Err()
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	result, err := s.reserveJob(cleanup, c)
	if err != nil {
		return result, err
	}
	s.commitAndFinalizeLedger(cleanup, nil, cause, &result)
	return result, nil
}

func (s *Service) reserveJob(ctx context.Context, c *jobs.Claim) (Result, error) {
	result := Result{ExecutionID: c.ExecutionID, Status: "running"}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	var locked int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, c.Job.DatabaseID).Scan(&locked); err != nil {
		return result, err
	}
	if err = jobs.CheckOwnedTx(ctx, tx, c); err != nil {
		return result, err
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history h WHERE database_id=$1 AND status IN ('running','unknown','partial') AND execution_id IS DISTINCT FROM $2
 AND NOT EXISTS(SELECT 1 FROM migration_job_attempts a WHERE a.execution_id=h.execution_id AND a.retry_scheduled))`, c.Job.DatabaseID, c.ExecutionID).Scan(&pending)
	if err != nil {
		return result, err
	}
	if pending {
		return result, errConflict
	}
	err = tx.QueryRow(ctx, `SELECT version FROM migration_history WHERE execution_id=$1 AND database_id=$2 AND draft_id=$3 AND draft_revision=$4 AND sql=$5 AND executed_by=$6 AND status='running'`, c.ExecutionID, c.Job.DatabaseID, c.Job.DraftID, c.Job.DraftRevision, c.SQL, c.Job.RequestedBy).Scan(&result.Version)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	hash := sha256.Sum256([]byte(c.SQL))
	telemetry.Event(ctx, "execution_started", "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "migration_id", c.Job.DraftID, "revision", c.Job.DraftRevision, "attempt", c.Job.AttemptCount, "execution_id", c.ExecutionID, "version", result.Version, "status", "running", "query", telemetry.RedactSQL(c.SQL), "query_sha256", hex.EncodeToString(hash[:]))
	return result, nil
}

func (s *Service) validateJob(ctx context.Context, target pgx.Tx, c *jobs.Claim) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	var locked int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, c.Job.DatabaseID).Scan(&locked); err != nil {
		return err
	}
	if err = jobs.CheckOwnedTx(ctx, tx, c); err != nil {
		return err
	}
	if err = bindTarget(ctx, tx, target, c.Job.DatabaseID); err != nil {
		return err
	}
	if err = validateApprovedSnapshot(ctx, tx, c.Job, c.SQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func executionCode(err error) string {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		return pgerr.Code
	}
	switch {
	case errors.Is(err, errBatchSetup):
		return "target_batch_unprotected"
	case errors.Is(err, errBatchTable):
		return "unsupported_batch_table"
	case errors.Is(err, errBatchScope):
		return "batch_scope_changed"
	case errors.Is(err, errBatchPayload):
		return "batch_payload_too_large"
	case errors.Is(err, errBatchEvidence):
		return "batch_evidence_invalid"
	case errors.Is(err, connections.ErrForbidden), errors.Is(err, auth.ErrUnauthenticated):
		return "access_revoked"
	case errors.Is(err, approval.ErrNotApproved), errors.Is(err, approval.ErrLeaderMissing):
		return "approval_incomplete"
	case errors.Is(err, errConflict):
		return "snapshot_changed"
	case errors.Is(err, errHistorySetup):
		return "target_history_unprotected"
	case errors.Is(err, errTargetIdentity):
		return "target_identity_conflict"
	case errors.Is(err, connections.ErrMetadataTarget):
		return "metadata_target_forbidden"
	case errors.Is(err, connections.ErrTargetUnavailable):
		return "target_unavailable"
	case errors.Is(err, jobs.ErrLeaseLost):
		return "lease_lost"
	case errors.Is(err, context.Canceled):
		return "worker_stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "execution_timeout"
	default:
		var network net.Error
		if errors.As(err, &network) {
			return "network_error"
		}
		return "execution_failed"
	}
}

func sqlSpan(ctx context.Context, index int) (context.Context, trace.Span) {
	return telemetry.Start(ctx, "migration.sql", trace.WithAttributes(attribute.Int("statement.index", index)))
}

func validateApprovedSnapshot(ctx context.Context, tx pgx.Tx, job jobs.Job, sqlSnapshot string) error {
	var err error
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN user_groups ug ON ug.user_id=u.id
 JOIN group_database_permissions p ON p.group_id=ug.group_id WHERE u.id=$1 AND u.is_active AND p.database_id=$2 AND p.access_level='edit')`, job.RequestedBy, job.DatabaseID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return connections.ErrForbidden
	}
	if job.ActionType == "resume" {
		if err = jobs.ValidateResumeTx(ctx, tx, job, sqlSnapshot); err != nil {
			if errors.Is(err, jobs.ErrConflict) {
				return errConflict
			}
			return err
		}
	} else {
		var revision int64
		var sql string
		if err = tx.QueryRow(ctx, `SELECT revision,sql FROM migration_drafts WHERE database_id=$1 AND id=$2 FOR UPDATE`, job.DatabaseID, job.DraftID).Scan(&revision, &sql); err != nil {
			return err
		}
		if revision != job.DraftRevision || sql != sqlSnapshot {
			return errConflict
		}
	}
	review, err := approval.EnsureApprovedReviewTx(ctx, tx, job.DatabaseID, job.DraftID, job.DraftRevision, job.ActionType, job.ReviewID, job.TargetExecutionID)
	if err != nil {
		return err
	}
	config, parseErr := batch.Parse(sqlSnapshot)
	if parseErr != nil || (job.ExecutionMode == "batch") != (config != nil) {
		return errConflict
	}
	if review.ID != job.ReviewID || review.SQLSnapshot != sqlSnapshot || (job.ActionType != "up" && job.ActionType != "resume") || (job.ExecutionMode != "normal" && job.ExecutionMode != "batch") {
		return errConflict
	}
	return nil
}
