package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/httpx"
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
	Title    string `json:"title"`
	SQL      string `json:"sql"`
	Revision int64  `json:"revision,string"`
}
type Result struct {
	ExecutionID string  `json:"execution_id"`
	Version     int64   `json:"version,string"`
	Status      string  `json:"status"`
	ErrorCode   *string `json:"error_code"`
}

var errConflict = errors.New("draft revision changed or an unresolved execution exists")

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	mux.HandleFunc("GET /api/v1/databases/{id}/migrations", sessions.Require(s.history))
	mux.HandleFunc("GET /api/v1/databases/{id}/migrations/{version}", sessions.Require(s.history))
	mux.HandleFunc("POST /api/v1/databases/{id}/migrations/reconcile", sessions.Require(s.reconcile))
	base := "/api/v1/databases/{id}/drafts"
	mux.HandleFunc("GET "+base, sessions.Require(s.listDrafts))
	mux.HandleFunc("POST "+base, sessions.Require(s.draft))
	mux.HandleFunc("GET "+base+"/{draftID}", sessions.Require(s.draft))
	mux.HandleFunc("PATCH "+base+"/{draftID}", sessions.Require(s.draft))
	mux.HandleFunc("POST "+base+"/{draftID}/execute", sessions.Require(s.execute))
	mux.HandleFunc("POST "+base+"/{draftID}/explain", sessions.Require(s.explain))
}

func (s *Service) authorize(r *http.Request, write bool) (auth.Principal, int64, error) {
	p, _ := auth.User(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return p, 0, connections.ErrInvalid
	}
	access, err := policy.DatabaseAccess(r.Context(), s.db, p.ID, id)
	if err != nil {
		return p, id, err
	}
	if !policy.Allows(access, write) {
		return p, id, connections.ErrForbidden
	}
	return p, id, nil
}

func fail(w http.ResponseWriter, err error) {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "55P03" {
		httpx.Error(w, 409, "migration_busy", "A target migration is still active; retry later")
		return
	}
	switch {
	case errors.Is(err, connections.ErrForbidden):
		httpx.Error(w, 403, "forbidden", "Database access denied")
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, 404, "not_found", "Database object not found")
	case errors.Is(err, errConflict):
		httpx.Error(w, 409, "execution_conflict", "Reload draft or resolve the pending execution before retrying")
	case errors.Is(err, connections.ErrInvalid), errors.Is(err, errSQL):
		httpx.Error(w, 400, "invalid_input", "Invalid ID, revision, title or unsupported SQL")
	case errors.Is(err, errHistorySetup):
		httpx.Error(w, 409, "target_history_unprotected", "Provision target history and use a dedicated restricted executor per database")
	case errors.Is(err, errTargetIdentity):
		httpx.Error(w, 409, "target_identity_conflict", "Target identity changed or is already bound to another connection")
	case errors.Is(err, connections.ErrMetadataTarget):
		httpx.Error(w, 409, "metadata_target_forbidden", "Metadata database cannot be a target")
	default:
		httpx.Error(w, 503, "migration_unavailable", "Cannot process migration; retry with the same draft revision")
	}
}

func (s *Service) draft(w http.ResponseWriter, r *http.Request) {
	p, id, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	var d Draft
	draftID, _ := strconv.ParseInt(r.PathValue("draftID"), 10, 64)
	if r.Method != "POST" && draftID < 1 {
		fail(w, connections.ErrInvalid)
		return
	}
	if r.Method == "GET" {
		d, err = scanDraft(s.db.QueryRow(r.Context(), `SELECT `+draftColumns+` FROM migration_drafts d JOIN users u ON u.id=d.created_by WHERE d.database_id=$1 AND d.id=$2`, id, draftID))
	} else {
		var in draftInput
		if !httpx.Decode(w, r, &in) {
			return
		}
		in.Title = strings.TrimSpace(in.Title)
		if len(in.Title) < 1 || len(in.Title) > 200 || strings.ContainsRune(in.Title, 0) {
			fail(w, connections.ErrInvalid)
			return
		}
		if _, err = statements(in.SQL); err != nil {
			fail(w, err)
			return
		}
		if r.Method == "POST" {
			if in.Revision != 0 {
				fail(w, connections.ErrInvalid)
				return
			}
			d, err = scanDraft(s.db.QueryRow(r.Context(), `WITH d AS (INSERT INTO migration_drafts(database_id,title,sql,created_by) VALUES($1,$2,$3,$4) RETURNING *) SELECT `+draftColumns+` FROM d JOIN users u ON u.id=d.created_by`, id, in.Title, in.SQL, p.ID))
		} else {
			if draftID < 1 || in.Revision < 1 {
				fail(w, connections.ErrInvalid)
				return
			}
			d, err = scanDraft(s.db.QueryRow(r.Context(), `WITH d AS (UPDATE migration_drafts SET title=$1,sql=$2,revision=revision+1,updated_at=now() WHERE database_id=$3 AND id=$4 AND revision=$5 RETURNING *) SELECT `+draftColumns+` FROM d JOIN users u ON u.id=d.created_by`, in.Title, in.SQL, id, draftID, in.Revision))
			if errors.Is(err, pgx.ErrNoRows) {
				err = errConflict
			}
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusOK
	if r.Method == "POST" {
		code = http.StatusCreated
	}
	httpx.JSON(w, code, d)
}

func (s *Service) execute(w http.ResponseWriter, r *http.Request) {
	p, id, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	draftID, e := strconv.ParseInt(r.PathValue("draftID"), 10, 64)
	var in struct {
		Revision int64 `json:"revision,string"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if e != nil || draftID < 1 || in.Revision < 1 {
		fail(w, connections.ErrInvalid)
		return
	}
	// Leave time for rollback and ledger persistence inside the session's 10s budget.
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()
	result, err := s.run(ctx, p, id, draftID, in.Revision)
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusOK
	if result.Status == "unknown" || result.Status == "running" {
		code = http.StatusAccepted
	}
	httpx.JSON(w, code, result)
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (s *Service) run(ctx context.Context, p auth.Principal, id, draftID, revision int64) (Result, error) {
	var result Result
	pool, err := s.targets.OpenTarget(ctx, p, id, true)
	if err != nil {
		return result, err
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(tx)
	// A transaction-scoped lock serializes all runner requests on this actual target.
	_, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='2s'; SET LOCAL standard_conforming_strings=on`)
	if err != nil {
		return result, err
	}
	userPath, err := internalSearchPath(ctx, tx)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(3110003)`); err != nil {
		return result, err
	}
	access, err := policy.DatabaseAccess(ctx, s.db, p.ID, id)
	if err != nil {
		return result, err
	}
	if !policy.Allows(access, true) {
		return result, connections.ErrForbidden
	}
	if err = protectedHistory(ctx, tx); err != nil {
		return result, err
	}
	meta, err := s.db.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(meta)
	var locked int64
	if err = meta.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		return result, err
	}
	var sql string
	if err = bindTarget(ctx, meta, tx, id); err != nil {
		return result, err
	}
	var actual int64
	if err = meta.QueryRow(ctx, `SELECT sql,revision FROM migration_drafts WHERE database_id=$1 AND id=$2 FOR UPDATE`, id, draftID).Scan(&sql, &actual); err != nil {
		return result, err
	}
	if actual != revision {
		return result, errConflict
	}
	err = meta.QueryRow(ctx, `SELECT execution_id,version,status,error_code FROM migration_history WHERE database_id=$1 AND draft_id=$2 AND draft_revision=$3`, id, draftID, revision).Scan(&result.ExecutionID, &result.Version, &result.Status, &result.ErrorCode)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	var pending bool
	if err = meta.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history WHERE database_id=$1 AND status IN ('running','unknown'))`, id).Scan(&pending); err != nil {
		return result, err
	}
	if pending {
		return result, errConflict
	}
	parts, err := statements(sql)
	if err != nil {
		return result, err
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return result, err
	}
	result.ExecutionID = hex.EncodeToString(token[:])
	result.Status = "running"
	err = meta.QueryRow(ctx, `INSERT INTO migration_history(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,receipt_version) SELECT $1,$2,$3,COALESCE(MAX(version),0)+1,$4,$5,'running',$6,1 FROM migration_history WHERE database_id=$1 RETURNING version`, id, draftID, revision, sql, p.ID, result.ExecutionID).Scan(&result.Version)
	if err != nil {
		return result, err
	}
	if err = meta.Commit(ctx); err != nil {
		return result, err
	}
	_, err = tx.Exec(ctx, `SELECT pg_catalog.set_config('search_path',$1,true)`, userPath)
	for _, part := range parts {
		if err != nil {
			break
		}
		// pgx.Tx.Exec falls back to simple protocol for parameterless SQL.
		// ExecParams forces one server-parsed command and discards returned rows.
		_, err = tx.Conn().PgConn().ExecParams(ctx, part, nil, nil, nil, nil).Close()
	}
	if err == nil {
		_, err = tx.Exec(ctx, `SET LOCAL search_path=pg_catalog,pg_temp`)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO _terserah.migration_receipts(execution_id,version,sql,executed_by,signature) VALUES($1,$2,$3,$4,$5)`, result.ExecutionID, result.Version, sql, p.ID, s.signature(id, result.ExecutionID, result.Version, sql, p.ID))
	}
	if err != nil {
		rollback(tx)
		result.Status = "failed"
		code := "execution_failed"
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) {
			code = pgerr.Code
		} else if ctx.Err() != nil {
			code = "execution_timeout"
		}
		result.ErrorCode = &code
	} else if err = tx.Commit(ctx); err != nil {
		var pgerr *pgconn.PgError
		code := "commit_unknown"
		if errors.As(err, &pgerr) && pgerr.Severity == "ERROR" && (strings.HasPrefix(pgerr.Code, "23") || pgerr.Code == "40001" || pgerr.Code == "40P01") {
			// Constraint/serialization failures confirm abort; connection failures do not.
			result.Status = "failed"
			code = pgerr.Code
		} else if errors.Is(err, pgx.ErrTxCommitRollback) {
			result.Status = "failed"
			code = "execution_failed"
		} else {
			// A lost COMMIT response cannot be interpreted as rollback or retried.
			result.Status = "unknown"
		}
		result.ErrorCode = &code
	} else {
		result.Status = "success"
	}
	cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = s.db.Exec(cleanup, `UPDATE migration_history SET status=$1,error_code=$2,finished_at=CASE WHEN $1 IN ('success','failed') THEN now() ELSE NULL END WHERE execution_id=$3 AND status IN ('running','unknown')`, result.Status, result.ErrorCode, result.ExecutionID)
	if err != nil {
		result.Status = "unknown"
		code := "ledger_unavailable"
		result.ErrorCode = &code
	}
	return result, nil
}
