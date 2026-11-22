package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/policy"
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
	Revision           int64     `json:"revision,string"`
	CreatedBy          int64     `json:"created_by,string"`
	CreatedByUsername  string    `json:"created_by_username"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type draftInput struct {
	Title              string  `json:"title"`
	SQL                string  `json:"sql"`
	SQLDown            *string `json:"sql_down"`
	Irreversible       bool    `json:"irreversible"`
	IrreversibleReason *string `json:"irreversible_reason"`
	Revision           int64   `json:"revision,string"`
}

type Result struct {
	ExecutionID string  `json:"execution_id"`
	Version     int64   `json:"version,string"`
	Status      string  `json:"status"`
	ErrorCode   *string `json:"error_code"`
}

var errConflict = errors.New("draft revision changed or an unresolved execution exists")
var errRetryInvalid = errors.New("retry hanya diizinkan untuk execution Up yang telah gagal")

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// run orchestrates a migration across the Target DB and Metadata DB:
//  1. Lock target DB via advisory lock and verify `_terserah` receipt protection.
//  2. Record a 'running' attempt in Metadata DB (`migration_history`).
//  3. Execute migration SQL statements + insert HMAC receipt in one Target DB transaction.
//  4. Commit Target DB and update `migration_history` status in Metadata DB.
func (s *Service) run(ctx context.Context, p auth.Principal, dbID, draftID, revision int64) (Result, error) {
	return s.runAttempt(ctx, p, dbID, draftID, revision, "")
}

// retryFailed hanya melakukan ulang SQL Up untuk attempt gagal yang telah dipilih.
func (s *Service) retryFailed(ctx context.Context, p auth.Principal, dbID, draftID, revision int64, retryOf string) (Result, error) {
	if retryOf == "" || len(retryOf) > 128 {
		return Result{}, errRetryInvalid
	}
	return s.runAttempt(ctx, p, dbID, draftID, revision, retryOf)
}

func (s *Service) runAttempt(ctx context.Context, p auth.Principal, dbID, draftID, revision int64, retryOf string) (Result, error) {
	var result Result

	pool, err := s.targets.OpenTarget(ctx, p, dbID, true)
	if err != nil {
		return result, err
	}
	defer pool.Close()

	targetTx, userPath, err := s.lockAndValidateTarget(ctx, pool, p.ID, dbID)
	if err != nil {
		return result, err
	}
	defer rollback(targetTx)

	sqlText, parts, replayed, err := s.recordRunningAttempt(ctx, targetTx, p.ID, dbID, draftID, revision, retryOf, &result)
	if err != nil || replayed {
		return result, err
	}

	execErr := s.execStatementsWithReceipt(ctx, targetTx, userPath, parts, dbID, result.ExecutionID, result.Version, sqlText, p.ID)
	s.commitAndFinalizeLedger(ctx, targetTx, execErr, &result)
	return result, nil
}

func (s *Service) lockAndValidateTarget(ctx context.Context, pool *pgxpool.Pool, userID, dbID int64) (pgx.Tx, string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	// A transaction-scoped advisory lock serializes all runner requests on this target.
	_, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='2s'; SET LOCAL standard_conforming_strings=on`)
	if err != nil {
		rollback(tx)
		return nil, "", err
	}
	userPath, err := internalSearchPath(ctx, tx)
	if err != nil {
		rollback(tx)
		return nil, "", err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(3110003)`); err != nil {
		rollback(tx)
		return nil, "", err
	}
	access, err := policy.DatabaseAccess(ctx, s.db, userID, dbID)
	if err != nil {
		rollback(tx)
		return nil, "", err
	}
	if !policy.Allows(access, true) {
		rollback(tx)
		return nil, "", connections.ErrForbidden
	}
	if err = protectedHistory(ctx, tx); err != nil {
		rollback(tx)
		return nil, "", err
	}
	return tx, userPath, nil
}

func (s *Service) recordRunningAttempt(ctx context.Context, targetTx pgx.Tx, userID, dbID, draftID, revision int64, retryOf string, result *Result) (string, []string, bool, error) {
	meta, err := s.db.Begin(ctx)
	if err != nil {
		return "", nil, false, err
	}
	defer rollback(meta)

	var locked int64
	if err = meta.QueryRow(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, dbID).Scan(&locked); err != nil {
		return "", nil, false, err
	}
	if err = bindTarget(ctx, meta, targetTx, dbID); err != nil {
		return "", nil, false, err
	}

	var title, sqlText string
	var sqlDown, irreversibleReason *string
	var irreversible bool
	var actualRevision int64
	err = meta.QueryRow(ctx, `
		SELECT title, sql, sql_down, irreversible, irreversible_reason, revision
		FROM migration_drafts
		WHERE database_id = $1 AND id = $2
		FOR UPDATE
	`, dbID, draftID).Scan(&title, &sqlText, &sqlDown, &irreversible, &irreversibleReason, &actualRevision)
	if err != nil {
		return "", nil, false, err
	}
	if actualRevision != revision {
		return "", nil, false, errConflict
	}

	// Endpoint execute lama tetap idempotent. Retry harus eksplisit agar request
	// duplikat tidak menjalankan SQL dua kali secara tidak sengaja.
	if retryOf == "" {
		err = meta.QueryRow(ctx, `
			SELECT execution_id, version, status, error_code
			FROM migration_history
			WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3 AND action_type='up'
			ORDER BY version DESC LIMIT 1
		`, dbID, draftID, revision).Scan(&result.ExecutionID, &result.Version, &result.Status, &result.ErrorCode)
		if err == nil {
			return "", nil, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, err
		}
	} else {
		// Retry kedua atas parent yang sama mengembalikan child sebelumnya.
		err = meta.QueryRow(ctx, `SELECT execution_id,version,status,error_code FROM migration_history
			WHERE database_id=$1 AND retry_of_execution_id=$2`, dbID, retryOf).
			Scan(&result.ExecutionID, &result.Version, &result.Status, &result.ErrorCode)
		if err == nil {
			return "", nil, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, err
		}
		var parentStatus, parentAction, parentSQL string
		err = meta.QueryRow(ctx, `SELECT status, action_type, sql FROM migration_history
			WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3 AND execution_id=$4
			FOR UPDATE`, dbID, draftID, revision, retryOf).Scan(&parentStatus, &parentAction, &parentSQL)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, false, errRetryInvalid
		}
		if err != nil {
			return "", nil, false, err
		}
		if parentStatus != "failed" || parentAction != "up" || parentSQL != sqlText {
			return "", nil, false, errRetryInvalid
		}
		// Receipt yang sudah committed mengalahkan metadata status=failed.
		var committed bool
		if err = targetTx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _terserah.migration_receipts
			WHERE execution_id=$1)`, retryOf).Scan(&committed); err != nil {
			return "", nil, false, err
		}
		if committed {
			return "", nil, false, errMigrationUnresolved
		}
	}

	var hasPending, hasOtherRevision bool
	err = meta.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM migration_history
			WHERE database_id = $1 AND status IN ('running', 'unknown')
		), EXISTS(
			SELECT 1 FROM migration_history
			WHERE database_id = $1 AND draft_id = $2
			  AND draft_revision IS DISTINCT FROM $3
		)
	`, dbID, draftID, revision).Scan(&hasPending, &hasOtherRevision)
	if err != nil {
		return "", nil, false, err
	}
	if hasOtherRevision {
		return "", nil, false, errDraftLocked
	}
	if hasPending {
		return "", nil, false, errConflict
	}
	// Validasi posisi dilakukan setelah database_connections terkunci dan sebelum
	// menambah attempt agar tidak ada dua migration aktif dengan definisi sama.
	state, err := bacaPosisiMigration(ctx, meta, dbID)
	if err != nil {
		return "", nil, false, err
	}
	if err := state.cekUp(draftID); err != nil {
		return "", nil, false, err
	}
	// Definisi lama yang belum mempunyai Down atau alasan irreversible harus
	// dilengkapi sebelum attempt pertama; jangan menyimpan snapshot tidak valid.
	if _, err = validasiDefinisiDraft(draftInput{
		Title: title, SQL: sqlText, SQLDown: sqlDown,
		Irreversible: irreversible, IrreversibleReason: irreversibleReason,
	}); err != nil {
		return "", nil, false, err
	}
	review, err := approval.EnsureApprovedTx(ctx, meta, dbID, draftID, revision, "up")
	if err != nil {
		return "", nil, false, err
	}
	// Review yang disetujui tidak boleh digunakan bila SQL Up berubah diam-diam.
	if review.SQLSnapshot != sqlText {
		return "", nil, false, approval.ErrNotApproved
	}

	parts, err := statements(sqlText)
	if err != nil {
		return "", nil, false, err
	}

	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return "", nil, false, err
	}
	result.ExecutionID = hex.EncodeToString(token[:])
	result.Status = "running"

	if retryOf == "" {
		result.Version, err = catatExecutionPertamaTx(ctx, meta, dbID, draftID, revision,
			userID, result.ExecutionID, sqlText, sqlDown, irreversible, irreversibleReason)
	} else {
		result.Version, err = catatExecutionUlangTx(ctx, meta, dbID, draftID, revision,
			userID, result.ExecutionID, retryOf, sqlText)
	}
	if err != nil {
		return "", nil, false, err
	}
	if err = meta.Commit(ctx); err != nil {
		return "", nil, false, err
	}
	return sqlText, parts, false, nil
}

func (s *Service) execStatementsWithReceipt(ctx context.Context, targetTx pgx.Tx, userPath string, parts []string, dbID int64, executionID string, version int64, sqlText string, actorID int64) error {
	_, err := targetTx.Exec(ctx, `SELECT pg_catalog.set_config('search_path', $1, true)`, userPath)
	for _, part := range parts {
		if err != nil {
			break
		}
		// pgx.Tx.Exec falls back to simple protocol for parameterless SQL.
		// ExecParams forces one server-parsed command and discards returned rows.
		_, err = targetTx.Conn().PgConn().ExecParams(ctx, part, nil, nil, nil, nil).Close()
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
	if execErr != nil {
		rollback(targetTx)
		result.Status = "failed"
		code := "execution_failed"
		var pgerr *pgconn.PgError
		if errors.As(execErr, &pgerr) {
			code = pgerr.Code
		} else if ctx.Err() != nil {
			code = "execution_timeout"
		}
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
}
