package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/batch"
	"terserah/backend/internal/policy"
	"terserah/backend/internal/telemetry"
)

const jobColumns = `id,database_id,review_id,draft_id,draft_revision,action_type,execution_mode,
 requested_by,status,attempt_count,max_attempts,created_at,started_at,finished_at,error_code,committed_batches,committed_rows`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.DatabaseID, &j.ReviewID, &j.DraftID, &j.DraftRevision, &j.ActionType, &j.ExecutionMode,
		&j.RequestedBy, &j.Status, &j.AttemptCount, &j.MaxAttempts, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.ErrorCode,
		&j.CommittedBatches, &j.CommittedRows)
	return j, err
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *Service) authorize(ctx context.Context, p auth.Principal, dbID int64, write bool) error {
	if p.ID < 1 {
		return auth.ErrUnauthenticated
	}
	if dbID < 1 {
		return ErrInvalid
	}
	access, err := policy.DatabaseAccess(ctx, s.db, p.ID, dbID)
	if err != nil {
		return err
	}
	if !policy.Allows(access, write) {
		return ErrForbidden
	}
	return nil
}

// Enqueue binds one approved review to one immutable job in the same transaction.
func (s *Service) Enqueue(ctx context.Context, p auth.Principal, dbID, reviewID int64, parent string) (Job, bool, error) {
	ctx, span := telemetry.Start(ctx, "migration.enqueue", trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()
	if current := telemetry.Parent(ctx); current != "" {
		parent = current
	}
	if reviewID < 1 {
		return Job{}, false, ErrInvalid
	}
	if err := s.authorize(ctx, p, dbID, true); err != nil {
		return Job{}, false, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Job{}, false, err
	}
	defer rollback(tx)
	var locked int64
	if err = tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, dbID).Scan(&locked); err != nil {
		return Job{}, false, err
	}
	// Permission may have changed while this request waited for the database row.
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u
 JOIN user_groups ug ON ug.user_id=u.id JOIN group_database_permissions p ON p.group_id=ug.group_id
 WHERE u.id=$1 AND u.is_active AND p.database_id=$2 AND p.access_level='edit')`, p.ID, dbID).Scan(&allowed)
	if err != nil {
		return Job{}, false, err
	}
	if !allowed {
		return Job{}, false, ErrForbidden
	}
	old, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND review_id=$2`, dbID, reviewID))
	if err == nil {
		return old, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, err
	}
	var draftID, revision int64
	var action, status string
	var targetExecution *string
	err = tx.QueryRow(ctx, `SELECT draft_id,draft_revision,action_type,status,target_execution_id
 FROM migration_reviews WHERE database_id=$1 AND id=$2 FOR UPDATE`, dbID, reviewID).
		Scan(&draftID, &revision, &action, &status, &targetExecution)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, ErrNotFound
	}
	if err != nil {
		return Job{}, false, err
	}
	if action != "up" || targetExecution != nil {
		return Job{}, false, ErrUnsupported
	}
	if status != approval.StatusApproved {
		return Job{}, false, approval.ErrNotApproved
	}
	var sql string
	var actual int64
	err = tx.QueryRow(ctx, `SELECT sql,revision FROM migration_drafts WHERE database_id=$1 AND id=$2 FOR UPDATE`, dbID, draftID).Scan(&sql, &actual)
	if err != nil {
		return Job{}, false, err
	}
	if actual != revision {
		return Job{}, false, ErrConflict
	}
	config, err := batch.Parse(sql)
	if err != nil {
		return Job{}, false, ErrInvalid
	}
	var executed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3)`, dbID, draftID, revision).Scan(&executed); err != nil {
		return Job{}, false, err
	}
	if executed {
		return Job{}, false, ErrConflict
	}
	review, err := approval.EnsureApprovedTx(ctx, tx, dbID, draftID, revision, action)
	if err != nil {
		return Job{}, false, err
	}
	if review.ID != reviewID || review.SQLSnapshot != sql {
		return Job{}, false, ErrConflict
	}
	j := Job{DatabaseID: dbID, ReviewID: reviewID, DraftID: draftID, DraftRevision: revision,
		ActionType: action, ExecutionMode: "normal", RequestedBy: p.ID}
	if config != nil {
		j.ExecutionMode = "batch"
	}
	j, err = scanJob(tx.QueryRow(ctx, `INSERT INTO migration_jobs
 (database_id,review_id,draft_id,draft_revision,action_type,execution_mode,sql_snapshot,snapshot_hash,requested_by,trace_parent)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+jobColumns,
		dbID, reviewID, draftID, revision, action, j.ExecutionMode, sql, snapshotHash(j, sql), p.ID, traceParent(parent)))
	if err != nil {
		return Job{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_reviews SET status='queued' WHERE id=$1`, reviewID); err != nil {
		return Job{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Job{}, false, err
	}
	telemetry.Event(ctx, "job_queued", "actor_id", p.ID, "database_id", dbID, "job_id", j.ID, "review_id", reviewID, "status", j.Status)
	return j, false, nil
}

func (s *Service) List(ctx context.Context, p auth.Principal, dbID, before int64, limit int) ([]Job, bool, *string, error) {
	if before < 0 || limit < 1 || limit > 100 {
		return nil, false, nil, ErrInvalid
	}
	if err := s.authorize(ctx, p, dbID, false); err != nil {
		return nil, false, nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+jobColumns+` FROM migration_jobs
 WHERE database_id=$1 AND ($2::bigint=0 OR id<$2) ORDER BY id DESC LIMIT $3`, dbID, before, limit+1)
	if err != nil {
		return nil, false, nil, err
	}
	defer rows.Close()
	items := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, false, nil, err
		}
		items = append(items, j)
	}
	if err = rows.Err(); err != nil {
		return nil, false, nil, err
	}
	more := len(items) > limit
	var next *string
	if more {
		items = items[:limit]
		value := strconv.FormatInt(items[len(items)-1].ID, 10)
		next = &value
	}
	return items, more, next, nil
}

func (s *Service) Get(ctx context.Context, p auth.Principal, dbID, jobID int64) (Job, []Attempt, error) {
	if jobID < 1 {
		return Job{}, nil, ErrInvalid
	}
	if err := s.authorize(ctx, p, dbID, false); err != nil {
		return Job{}, nil, err
	}
	// Job state and attempt details must describe the same database snapshot.
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Job{}, nil, err
	}
	defer rollback(tx)
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND id=$2`, dbID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, nil, ErrNotFound
	}
	if err != nil {
		return Job{}, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT a.execution_id,h.version,a.attempt_no,a.status,a.started_at,a.finished_at,a.error_code
 FROM migration_job_attempts a LEFT JOIN migration_history h ON h.execution_id=a.execution_id WHERE a.job_id=$1 ORDER BY a.attempt_no`, jobID)
	if err != nil {
		return Job{}, nil, err
	}
	defer rows.Close()
	items := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err = rows.Scan(&a.ExecutionID, &a.Version, &a.Number, &a.Status, &a.StartedAt, &a.FinishedAt, &a.ErrorCode); err != nil {
			return Job{}, nil, err
		}
		items = append(items, a)
	}
	if err = rows.Err(); err != nil {
		return Job{}, nil, err
	}
	rows.Close()
	if err = tx.Commit(ctx); err != nil {
		return Job{}, nil, err
	}
	return j, items, nil
}

// Claim serializes dispatch per database, but never treats an expired lease as safe to retry.
func (s *Service) Claim(ctx context.Context, worker string, lease time.Duration) (*Claim, error) {
	if !validLease(worker, lease) {
		return nil, ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	var dbID int64
	err = tx.QueryRow(ctx, `SELECT d.id FROM database_connections d
 WHERE EXISTS(SELECT 1 FROM migration_jobs q WHERE q.database_id=d.id AND q.status IN ('queued','retry_wait') AND q.available_at<=now() AND q.attempt_count<q.max_attempts AND NOT EXISTS(SELECT 1 FROM migration_jobs w WHERE w.database_id=d.id AND w.status='retry_wait' AND w.id<>q.id))
 AND NOT EXISTS(SELECT 1 FROM migration_jobs a WHERE a.database_id=d.id AND a.status IN ('running','unknown','partial','recovering'))
 AND NOT EXISTS(SELECT 1 FROM migration_history h WHERE h.database_id=d.id AND h.status IN ('running','unknown','partial') AND NOT EXISTS(SELECT 1 FROM migration_job_attempts sealed WHERE sealed.execution_id=h.execution_id AND sealed.retry_scheduled))
 ORDER BY (SELECT min(q.id) FROM migration_jobs q WHERE q.database_id=d.id AND q.status='queued' AND q.available_at<=now()),d.id
 LIMIT 1 FOR UPDATE OF d SKIP LOCKED`).Scan(&dbID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The dispatch query's snapshot may predate another claimant's commit.
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_jobs WHERE database_id=$1 AND status IN ('running','unknown','partial','recovering'))
 OR EXISTS(SELECT 1 FROM migration_history h WHERE database_id=$1 AND status IN ('running','unknown','partial') AND NOT EXISTS(SELECT 1 FROM migration_job_attempts sealed WHERE sealed.execution_id=h.execution_id AND sealed.retry_scheduled))`, dbID).Scan(&blocked)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, nil
	}
	j, err := scanJob(tx.QueryRow(ctx, `SELECT `+jobColumns+` FROM migration_jobs
 WHERE database_id=$1 AND status IN ('queued','retry_wait') AND available_at<=now() AND attempt_count<max_attempts
 AND NOT EXISTS(SELECT 1 FROM migration_jobs w WHERE w.database_id=$1 AND w.status='retry_wait' AND w.id<>migration_jobs.id)
 ORDER BY id LIMIT 1 FOR UPDATE`, dbID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sql, hash, parent string
	if err = tx.QueryRow(ctx, `SELECT sql_snapshot,snapshot_hash,trace_parent FROM migration_jobs WHERE id=$1`, j.ID).Scan(&sql, &hash, &parent); err != nil {
		return nil, err
	}
	if snapshotHash(j, sql) != hash {
		return nil, ErrConflict
	}
	var execution [16]byte
	var token [32]byte
	if _, err = rand.Read(execution[:]); err != nil {
		return nil, err
	}
	if _, err = rand.Read(token[:]); err != nil {
		return nil, err
	}
	c := &Claim{ExecutionID: hex.EncodeToString(execution[:]), WorkerID: worker, LeaseToken: hex.EncodeToString(token[:]), SQL: sql, TraceParent: parent}
	j, err = scanJob(tx.QueryRow(ctx, `UPDATE migration_jobs SET status='running',attempt_count=attempt_count+1,
 worker_id=$2,lease_token_hash=$3,lease_expires_at=clock_timestamp()+$4::bigint*interval '1 millisecond',
 started_at=COALESCE(started_at,clock_timestamp()),error_code=NULL,
 batch_root_execution_id=CASE WHEN execution_mode='batch' THEN COALESCE(batch_root_execution_id,$5) ELSE NULL END WHERE id=$1 RETURNING `+jobColumns,
		j.ID, worker, leaseHash(c.LeaseToken), lease.Milliseconds(), c.ExecutionID))
	if err != nil {
		return nil, err
	}
	c.Job = j
	if _, err = tx.Exec(ctx, `INSERT INTO migration_job_attempts(job_id,execution_id,attempt_no,worker_id,sql_snapshot)
 VALUES($1,$2,$3,$4,$5)`, j.ID, c.ExecutionID, j.AttemptCount, worker, sql); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(batch_root_execution_id,'') FROM migration_jobs WHERE id=$1`, j.ID).Scan(&c.RootExecutionID); err != nil {
		return nil, err
	}
	var root any
	if c.RootExecutionID != "" {
		root = c.RootExecutionID
	}
	err = tx.QueryRow(ctx, `INSERT INTO migration_history(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,receipt_version,batch_root_execution_id)
 SELECT $1,$2,$3,COALESCE(MAX(version),0)+1,$4,$5,'running',$6,1,$7 FROM migration_history WHERE database_id=$1 RETURNING version`,
		j.DatabaseID, j.DraftID, j.DraftRevision, sql, j.RequestedBy, c.ExecutionID, root).Scan(&c.Version)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	telemetry.Event(telemetry.Restore(ctx, parent), "job_claimed", "actor_id", j.RequestedBy, "database_id", dbID, "job_id", j.ID, "execution_id", c.ExecutionID, "attempt", j.AttemptCount, "status", j.Status)
	return c, nil
}

func leaseHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}

func validClaim(c *Claim) bool {
	if c == nil || c.Job.ID < 1 || c.Job.AttemptCount < 1 || !workerPattern.MatchString(c.WorkerID) {
		return false
	}
	token, err := hex.DecodeString(c.LeaseToken)
	if err != nil || len(token) != 32 {
		return false
	}
	execution, err := hex.DecodeString(c.ExecutionID)
	return err == nil && len(execution) == 16
}

func (s *Service) Heartbeat(ctx context.Context, c *Claim, lease time.Duration) error {
	if !validClaim(c) || !validLease(c.WorkerID, lease) {
		return ErrInvalid
	}
	result, err := s.db.Exec(ctx, `UPDATE migration_jobs j
 SET lease_expires_at=clock_timestamp()+$5::bigint*interval '1 millisecond'
 WHERE j.id=$1 AND j.status='running' AND j.worker_id=$2 AND j.lease_token_hash=$3 AND j.lease_expires_at>clock_timestamp()
 AND EXISTS(SELECT 1 FROM migration_job_attempts a WHERE a.job_id=j.id AND a.attempt_no=j.attempt_count AND a.execution_id=$4 AND a.status='running')`,
		c.Job.ID, c.WorkerID, leaseHash(c.LeaseToken), c.ExecutionID, lease.Milliseconds())
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

// ReportProgress publishes only target-committed counts while the worker still owns its lease.
func (s *Service) ReportProgress(ctx context.Context, c *Claim, batches, rows int64) error {
	if !validClaim(c) || batches < 0 || rows < 0 || rows < batches {
		return ErrInvalid
	}
	result, err := s.db.Exec(ctx, `UPDATE migration_jobs j SET committed_batches=$5,committed_rows=$6
 WHERE j.id=$1 AND j.status='running' AND j.worker_id=$2 AND j.lease_token_hash=$3 AND j.lease_expires_at>clock_timestamp()
 AND j.committed_batches<=$5 AND j.committed_rows<=$6
 AND EXISTS(SELECT 1 FROM migration_job_attempts a WHERE a.job_id=j.id AND a.attempt_no=j.attempt_count AND a.execution_id=$4 AND a.status='running')`, c.Job.ID, c.WorkerID, leaseHash(c.LeaseToken), c.ExecutionID, batches, rows)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Finish records a worker outcome. Target commit evidence is verified by the executor before this call.
func (s *Service) Finish(ctx context.Context, c *Claim, out Outcome) error {
	if !validClaim(c) || !validOutcome(out) {
		return ErrInvalid
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var locked int64
	err = tx.QueryRow(ctx, `SELECT id FROM migration_jobs WHERE id=$1 AND status='running'
 AND worker_id=$2 AND lease_token_hash=$3 AND lease_expires_at>clock_timestamp() FOR UPDATE`,
		c.Job.ID, c.WorkerID, leaseHash(c.LeaseToken)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	// Recovery may verify COMMIT between executor return and this finalization.
	if out.Status == Unknown {
		var state Status
		var code *string
		err = tx.QueryRow(ctx, `SELECT status,error_code FROM migration_history WHERE execution_id=$1`, c.ExecutionID).Scan(&state, &code)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && (state == Success || state == Failed) {
			out = Outcome{Status: state}
			if code != nil {
				out.ErrorCode = *code
			}
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_history SET status=$2,error_code=NULLIF($3,''),finished_at=CASE WHEN $2 IN ('success','failed') THEN clock_timestamp() ELSE NULL END WHERE execution_id=$1 AND status IN ('running','unknown')`, c.ExecutionID, out.Status, out.ErrorCode); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE migration_job_attempts a SET status=$3,error_code=NULLIF($4,''),finished_at=clock_timestamp()
 WHERE job_id=$1 AND execution_id=$2 AND status='running'
 AND attempt_no=(SELECT attempt_count FROM migration_jobs WHERE id=$1)`, c.Job.ID, c.ExecutionID, out.Status, out.ErrorCode)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	_, err = tx.Exec(ctx, `UPDATE migration_jobs SET status=$2,error_code=NULLIF($3,''),
 finished_at=CASE WHEN $2 IN ('success','failed') THEN clock_timestamp() ELSE NULL END,
 worker_id=NULL,lease_token_hash=NULL,lease_expires_at=NULL WHERE id=$1`, c.Job.ID, out.Status, out.ErrorCode)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	telemetry.Event(ctx, "job_finished", "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "execution_id", c.ExecutionID, "attempt", c.Job.AttemptCount, "status", out.Status, "error_code", out.ErrorCode)
	return nil
}

// CheckOwnedTx fences executor metadata changes against the current immutable claim.
func CheckOwnedTx(ctx context.Context, tx pgx.Tx, c *Claim) error {
	if !validClaim(c) {
		return ErrInvalid
	}
	var id int64
	err := tx.QueryRow(ctx, `SELECT j.id FROM migration_jobs j WHERE j.id=$1 AND j.status='running' AND j.worker_id=$2
 AND j.lease_token_hash=$3 AND j.lease_expires_at>clock_timestamp() AND j.database_id=$5 AND j.review_id=$6
 AND j.draft_id=$7 AND j.draft_revision=$8 AND j.requested_by=$9 AND j.sql_snapshot=$10 AND COALESCE(j.batch_root_execution_id,'')=$11
 AND EXISTS(SELECT 1 FROM migration_job_attempts a WHERE a.job_id=j.id AND a.execution_id=$4 AND a.attempt_no=j.attempt_count AND a.status='running') FOR UPDATE`,
		c.Job.ID, c.WorkerID, leaseHash(c.LeaseToken), c.ExecutionID, c.Job.DatabaseID, c.Job.ReviewID, c.Job.DraftID, c.Job.DraftRevision, c.Job.RequestedBy, c.SQL, c.RootExecutionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

func (s *Service) EnqueueDraft(ctx context.Context, p auth.Principal, dbID, draftID, revision int64) (Job, bool, error) {
	if err := s.authorize(ctx, p, dbID, true); err != nil {
		return Job{}, false, err
	}
	var reviewID int64
	err := s.db.QueryRow(ctx, `SELECT id FROM migration_reviews WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3
 AND action_type='up' AND status IN ('approved','queued') ORDER BY id DESC LIMIT 1`, dbID, draftID, revision).Scan(&reviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, approval.ErrNotApproved
	}
	if err != nil {
		return Job{}, false, err
	}
	return s.Enqueue(ctx, p, dbID, reviewID, telemetry.Parent(ctx))
}

// ResolveUnknownTx is called only after the target lock and verified receipt prove the outcome.
func ResolveUnknownTx(ctx context.Context, tx pgx.Tx, execution string, out Outcome) error {
	if !validOutcome(out) || (out.Status != Success && out.Status != Failed) {
		return ErrInvalid
	}
	var jobID int64
	err := tx.QueryRow(ctx, `SELECT j.id FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id
 WHERE a.execution_id=$1 AND a.attempt_no=j.attempt_count AND j.status='unknown' FOR UPDATE OF j`, execution).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET status=$2,error_code=NULLIF($3,''),finished_at=clock_timestamp() WHERE execution_id=$1 AND status='unknown'`, execution, out.Status, out.ErrorCode); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE migration_jobs SET status=$2,error_code=NULLIF($3,''),finished_at=clock_timestamp() WHERE id=$1`, jobID, out.Status, out.ErrorCode)
	return err
}

// ResolveBatchTx accepts only counts/outcomes proven under both target locks.
func ResolveBatchTx(ctx context.Context, tx pgx.Tx, execution string, out Outcome, batches, rows int64) error {
	if !validOutcome(out) || batches < 0 || rows < batches || (out.Status == Partial && batches == 0) {
		return ErrInvalid
	}
	var id, storedBatches, storedRows int64
	err := tx.QueryRow(ctx, `SELECT j.id,j.committed_batches,j.committed_rows FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id
 WHERE a.execution_id=$1 AND a.attempt_no=j.attempt_count AND j.execution_mode='batch' FOR UPDATE OF j`, execution).Scan(&id, &storedBatches, &storedRows)
	if err != nil {
		return err
	}
	if storedBatches > batches || storedRows > rows {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE migration_job_attempts SET status=$2,error_code=NULLIF($3,''),finished_at=clock_timestamp() WHERE execution_id=$1`, execution, out.Status, out.ErrorCode); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE migration_jobs SET status=$2,error_code=NULLIF($3,''),committed_batches=$4,committed_rows=$5,
 finished_at=CASE WHEN $2 IN ('success','failed') THEN clock_timestamp() ELSE NULL END,
 worker_id=NULL,lease_token_hash=NULL,lease_expires_at=NULL WHERE id=$1`, id, out.Status, out.ErrorCode, batches, rows)
	return err
}
