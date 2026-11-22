package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/telemetry"
)

// Recovery contains server-internal evidence coordinates, never an HTTP payload.
type Recovery struct {
	Job                                            Job    `json:"-"`
	ExecutionID, SQL, TraceParent, RootExecutionID string `json:"-"`
	Version                                        int64  `json:"-"`
	HistoryStatus                                  Status `json:"-"`
	HistoryCode                                    string `json:"-"`
}

func (s *Service) RecoveryCandidate(ctx context.Context) (*Recovery, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE available_at<=clock_timestamp()
 AND (status IN ('unknown','recovering') OR (status='running' AND lease_expires_at<=clock_timestamp())) ORDER BY available_at,id LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &Recovery{Job: j}
	var version *int64
	var state, code *string
	var hash string
	err = tx.QueryRow(ctx, `SELECT a.execution_id,j.sql_snapshot,j.trace_parent,COALESCE(j.batch_root_execution_id,''),j.snapshot_hash,h.version,h.status,h.error_code
 FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id AND a.attempt_no=j.attempt_count
 LEFT JOIN migration_history h ON h.execution_id=a.execution_id WHERE j.id=$1`, j.ID).Scan(&r.ExecutionID, &r.SQL, &r.TraceParent, &r.RootExecutionID, &hash, &version, &state, &code)
	if err != nil {
		return nil, err
	}
	if snapshotHash(j, r.SQL) != hash {
		return nil, ErrConflict
	}
	if version != nil {
		r.Version = *version
	}
	if state != nil {
		r.HistoryStatus = Status(*state)
	}
	if code != nil {
		r.HistoryCode = *code
	}
	if j.ExecutionMode == "batch" && r.RootExecutionID == "" {
		r.RootExecutionID = r.ExecutionID
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// DeferRecovery changes probe timing only; it never releases the target or retries SQL.
func (s *Service) DeferRecovery(ctx context.Context, r *Recovery, code string, delay time.Duration) error {
	if r == nil || delay < time.Second || delay > time.Minute || (code != "" && !codePattern.MatchString(code)) {
		return ErrInvalid
	}
	_, err := s.db.Exec(ctx, `UPDATE migration_jobs SET available_at=clock_timestamp()+$3::bigint*interval '1 millisecond',error_code=CASE WHEN $4='' THEN error_code ELSE $4 END
 WHERE id=$1 AND attempt_count=$2 AND (status IN ('unknown','recovering') OR (status='running' AND lease_expires_at<=clock_timestamp()))`, r.Job.ID, r.Job.AttemptCount, delay.Milliseconds(), code)
	return err
}

func RetryCause(r *Recovery) string {
	if r.HistoryStatus == Success {
		return ""
	}
	if r.HistoryStatus == Failed || r.HistoryStatus == Partial {
		if Transient(r.HistoryCode) {
			return r.HistoryCode
		}
		return ""
	}
	if r.Job.ErrorCode != nil && Transient(*r.Job.ErrorCode) {
		return *r.Job.ErrorCode
	}
	if r.Job.Status == Running || r.Job.ErrorCode == nil {
		return "worker_interrupted"
	}
	switch *r.Job.ErrorCode {
	case "commit_unknown", "ledger_unavailable", "progress_unavailable", "lease_lost":
		return "worker_interrupted"
	}
	return ""
}

// ResolveRecovery is called with both target locks held, after receipt/checkpoint verification.
func (s *Service) ResolveRecovery(ctx context.Context, r *Recovery, out Outcome, batches, rows int64, retryCause string) error {
	if r == nil || !validOutcome(out) || out.Status == Unknown || batches < 0 || rows < batches || (out.Status == Partial && batches == 0) || (retryCause != "" && !Transient(retryCause)) {
		return ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	var id int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, r.Job.DatabaseID).Scan(&id); err != nil {
		return err
	}
	current, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id=$1 AND attempt_count=$2
 AND (status IN ('unknown','recovering') OR (status='running' AND lease_expires_at<=clock_timestamp())) FOR UPDATE`, r.Job.ID, r.Job.AttemptCount))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	var execution, sql string
	if err = tx.QueryRow(ctx, `SELECT execution_id,sql_snapshot FROM migration_job_attempts WHERE job_id=$1 AND attempt_no=$2 FOR UPDATE`, current.ID, current.AttemptCount).Scan(&execution, &sql); err != nil {
		return err
	}
	if execution != r.ExecutionID || sql != r.SQL || current.CommittedBatches > batches || current.CommittedRows > rows {
		return ErrConflict
	}
	var state Status
	var existingCode *string
	err = tx.QueryRow(ctx, `SELECT status,error_code FROM migration_history WHERE execution_id=$1 FOR UPDATE`, execution).Scan(&state, &existingCode)
	if errors.Is(err, pgx.ErrNoRows) {
		var root any
		if r.RootExecutionID != "" {
			if r.RootExecutionID != execution {
				return ErrConflict
			}
			root = r.RootExecutionID
		}
		err = tx.QueryRow(ctx, `INSERT INTO migration_history(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,receipt_version,batch_root_execution_id,started_at)
 SELECT $1,$2,$3,COALESCE(MAX(version),0)+1,$4,$5,'running',$6,1,$7,(SELECT started_at FROM migration_job_attempts WHERE execution_id=$6)
 FROM migration_history WHERE database_id=$1 RETURNING status`, current.DatabaseID, current.DraftID, current.DraftRevision, r.SQL, current.RequestedBy, execution, root).Scan(&state)
	}
	if err != nil {
		return err
	}
	attempt := out
	if state == Success && out.Status != Success {
		return ErrConflict
	}
	if state == Failed || state == Partial {
		if out.Status == Success {
			return ErrConflict
		}
		attempt.Status = state
		if existingCode != nil {
			attempt.ErrorCode = *existingCode
		}
	}
	retry := out.Status != Success && retryCause != "" && current.AttemptCount < current.MaxAttempts
	if _, err = tx.Exec(ctx, `UPDATE migration_history SET status=$2,error_code=NULLIF($3,''),finished_at=CASE WHEN $4 OR $2 IN ('success','failed') THEN COALESCE(finished_at,clock_timestamp()) ELSE NULL END WHERE execution_id=$1`, execution, attempt.Status, attempt.ErrorCode, retry); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET status=$2,error_code=NULLIF($3,''),finished_at=COALESCE(finished_at,clock_timestamp()),retry_scheduled=$4 WHERE execution_id=$1`, execution, attempt.Status, attempt.ErrorCode, retry); err != nil {
		return err
	}
	jobState := out.Status
	code := out.ErrorCode
	delay := time.Duration(0)
	if retry {
		jobState = RetryWait
		code = retryCause
		delay = RetryDelay(current.AttemptCount)
	}
	var root any
	if r.RootExecutionID != "" {
		root = r.RootExecutionID
	}
	_, err = tx.Exec(ctx, `UPDATE migration_jobs SET status=$2,error_code=NULLIF($3,''),committed_batches=$4,committed_rows=$5,
 finished_at=CASE WHEN $2 IN ('success','failed') THEN clock_timestamp() ELSE NULL END,worker_id=NULL,lease_token_hash=NULL,lease_expires_at=NULL,
 batch_root_execution_id=COALESCE(batch_root_execution_id,$6),available_at=clock_timestamp()+$7::bigint*interval '1 millisecond' WHERE id=$1`, current.ID, jobState, code, batches, rows, root, delay.Milliseconds())
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	telemetry.Event(ctx, "job_recovered", "actor_id", current.RequestedBy, "database_id", current.DatabaseID, "job_id", current.ID, "execution_id", execution, "attempt", current.AttemptCount, "status", jobState, "retry_delay_ms", delay.Milliseconds(), "error_code", code)
	return nil
}
