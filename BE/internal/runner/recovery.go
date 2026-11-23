package runner

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/telemetry"
)

// RecoverJob inspects protected evidence only; it never runs migration SQL.
func (s *Service) RecoverJob(ctx context.Context, r *jobs.Recovery) error {
	pool, err := s.targets.OpenForRecovery(ctx, r.Job.DatabaseID)
	if err != nil {
		return s.deferRecovery(ctx, r, executionCode(err), 5*time.Second)
	}
	defer pool.Close()
	acquired, err := pool.Acquire(ctx)
	if err != nil {
		return s.deferRecovery(ctx, r, "target_unavailable", 5*time.Second)
	}
	guard := acquired.Hijack()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = guard.Close(cleanup)
	}()
	if _, err = guard.Exec(ctx, `SET statement_timeout='5min'; SET lock_timeout='2s'; SET standard_conforming_strings=on`); err != nil {
		return s.deferRecovery(ctx, r, "target_unavailable", 5*time.Second)
	}
	var locked bool
	if err = guard.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_lock(3110003)`).Scan(&locked); err != nil {
		return s.deferRecovery(ctx, r, "target_unavailable", 5*time.Second)
	}
	if !locked {
		return s.deferRecovery(ctx, r, "", 2*time.Second)
	}
	tx, err := guard.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return s.deferRecovery(ctx, r, "target_unavailable", 5*time.Second)
	}
	defer rollback(ctx, tx)
	if _, err = internalSearchPath(ctx, tx); err != nil {
		return s.deferRecovery(ctx, r, "target_unavailable", 5*time.Second)
	}
	if err = tx.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_xact_lock(3110004)`).Scan(&locked); err != nil {
		return s.deferRecovery(ctx, r, "target_unavailable", 5*time.Second)
	}
	if !locked {
		return s.deferRecovery(ctx, r, "", 2*time.Second)
	}
	if err = protectedHistory(ctx, tx); err != nil {
		return s.deferRecovery(ctx, r, executionCode(err), 30*time.Second)
	}
	if err = s.bindRecoveryTarget(ctx, tx, r); err != nil {
		if errors.Is(err, jobs.ErrConflict) {
			return nil
		}
		return s.deferRecovery(ctx, r, executionCode(err), 30*time.Second)
	}
	item := pendingExecution{execution: &r.ExecutionID, version: r.Version, sql: r.SQL, actor: r.Job.RequestedBy, format: 1, isBatch: r.Job.ExecutionMode == "batch"}
	var out Result
	var progress batch.Checkpoint
	if r.Job.ActionType == "compensate" {
		out, _, progress, err = s.verifyCompensationPending(ctx, tx, r.Job.DatabaseID, item, r.RootExecutionID, r.CompensationRoot)
	} else if item.isBatch {
		out, _, progress, err = s.verifyBatchAttempt(ctx, tx, r.Job.DatabaseID, item, r.RootExecutionID)
	} else if r.Version == 0 {
		var exists bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _terserah.migration_receipts WHERE execution_id=$1)`, r.ExecutionID).Scan(&exists)
		if err == nil && exists {
			return s.deferRecovery(ctx, r, "receipt_mismatch", 30*time.Second)
		}
		out = Result{ExecutionID: r.ExecutionID, Status: "failed"}
	} else {
		out, _, err = s.verifyPendingAttempt(ctx, tx, r.Job.DatabaseID, item)
	}
	if err != nil {
		return s.deferRecovery(ctx, r, executionCode(err), 30*time.Second)
	}
	if out.Status == "unknown" {
		code := "receipt_mismatch"
		if out.ErrorCode != nil {
			code = *out.ErrorCode
		}
		return s.deferRecovery(ctx, r, code, 30*time.Second)
	}
	if (r.HistoryStatus == jobs.Success && out.Status != "success") || ((r.HistoryStatus == jobs.Failed || r.HistoryStatus == jobs.Partial || r.HistoryStatus == jobs.PartiallyReverted) && out.Status == "success") {
		return s.deferRecovery(ctx, r, "receipt_mismatch", 30*time.Second)
	}
	cause := ""
	if out.Status != "success" {
		cause = jobs.RetryCause(r)
	}
	if cause != "" && r.Job.AttemptCount < r.Job.MaxAttempts {
		if err = s.retryAllowed(ctx, r); err != nil {
			if !errors.Is(err, connections.ErrForbidden) && !errors.Is(err, approval.ErrNotApproved) && !errors.Is(err, approval.ErrLeaderMissing) && !errors.Is(err, errConflict) && !errors.Is(err, errMigrationOrder) && !errors.Is(err, errMigrationMustForward) && !errors.Is(err, errMigrationIrreversible) && !errors.Is(err, errMigrationUnresolved) && !errors.Is(err, errMigrationStateInvalid) {
				return s.deferRecovery(ctx, r, "metadata_unavailable", 5*time.Second)
			}
			cause = ""
			code := executionCode(err)
			out.ErrorCode = &code
		}
	}
	code := ""
	if out.ErrorCode != nil {
		code = *out.ErrorCode
	}
	if out.Status != "success" && code == "" {
		code = "worker_interrupted"
	}
	if cause != "" {
		code = cause
	}
	err = jobs.New(s.db).ResolveRecovery(ctx, r, jobs.Outcome{Status: jobs.Status(out.Status), ErrorCode: code}, progress.BatchNo, progress.CommittedRows, cause)
	if errors.Is(err, jobs.ErrConflict) {
		return s.deferRecovery(ctx, r, "recovery_conflict", 5*time.Second)
	}
	return err
}

func (s *Service) deferRecovery(ctx context.Context, r *jobs.Recovery, code string, delay time.Duration) error {
	telemetry.Event(ctx, "recovery_deferred", "actor_id", r.Job.RequestedBy, "database_id", r.Job.DatabaseID, "job_id", r.Job.ID, "execution_id", r.ExecutionID, "error_code", code, "delay_ms", delay.Milliseconds())
	return jobs.New(s.db).DeferRecovery(ctx, r, code, delay)
}

func (s *Service) bindRecoveryTarget(ctx context.Context, target pgx.Tx, r *jobs.Recovery) error {
	meta, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, meta)
	var id int64
	if err = meta.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, r.Job.DatabaseID).Scan(&id); err != nil {
		return err
	}
	err = meta.QueryRow(ctx, `SELECT id FROM migration_jobs WHERE id=$1 AND attempt_count=$2
 AND (status IN ('unknown','recovering') OR (status='running' AND lease_expires_at<=clock_timestamp())) FOR UPDATE`, r.Job.ID, r.Job.AttemptCount).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrConflict
	}
	if err != nil {
		return err
	}
	if err = bindTarget(ctx, meta, target, r.Job.DatabaseID); err != nil {
		return err
	}
	return meta.Commit(ctx)
}

func (s *Service) retryAllowed(ctx context.Context, r *jobs.Recovery) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	var id int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, r.Job.DatabaseID).Scan(&id); err != nil {
		return err
	}
	if err = validateApprovedSnapshot(ctx, tx, r.Job, r.SQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
