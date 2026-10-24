package runner

import (
	"context"
	"crypto/hmac"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/httpx"
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

func (s *Service) history(w http.ResponseWriter, r *http.Request) {
	_, id, err := s.authorize(r, false)
	if err != nil {
		fail(w, err)
		return
	}
	var version int64
	detail := r.PathValue("version") != ""
	if detail {
		if r.URL.Query().Has("limit") || r.URL.Query().Has("before") {
			fail(w, connections.ErrInvalid)
			return
		}
		version, err = strconv.ParseInt(r.PathValue("version"), 10, 64)
		if err != nil || version < 1 {
			fail(w, connections.ErrInvalid)
			return
		}
	}
	limit := 50
	var before int64
	if r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			fail(w, connections.ErrInvalid)
			return
		}
	}
	if r.URL.Query().Has("before") {
		before, err = strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		if err != nil || before < 1 {
			fail(w, connections.ErrInvalid)
			return
		}
	}
	rows, err := s.db.Query(r.Context(), `SELECT h.database_id,h.draft_id,h.draft_revision,h.execution_id,h.version,
 CASE WHEN $2::bigint>0 THEN h.sql ELSE '' END,h.executed_by,u.username,h.started_at,h.finished_at,h.status,h.error_code
 FROM migration_history h JOIN users u ON u.id=h.executed_by WHERE h.database_id=$1
 AND ($2::bigint=0 OR h.version=$2) AND ($3::bigint=0 OR h.version<$3)
 ORDER BY h.version DESC LIMIT $4`, id, version, before, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	items := []History{}
	for rows.Next() {
		var item History
		if err = rows.Scan(&item.DatabaseID, &item.DraftID, &item.Revision, &item.ExecutionID, &item.Version, &item.SQL, &item.ExecutedBy, &item.Username, &item.StartedAt, &item.FinishedAt, &item.Status, &item.ErrorCode); err != nil {
			fail(w, err)
			return
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		fail(w, err)
		return
	}
	if detail {
		if len(items) == 0 {
			fail(w, pgx.ErrNoRows)
			return
		}
		httpx.JSON(w, 200, items[0])
		return
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
	httpx.JSON(w, 200, map[string]any{"migrations": items, "next_before": next, "has_more": more})
}

func (s *Service) reconcile(w http.ResponseWriter, r *http.Request) {
	p, id, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	if r.ContentLength != 0 {
		var in struct{}
		if !httpx.Decode(w, r, &in) {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()
	pool, err := s.targets.OpenTarget(ctx, p, id, true)
	if err != nil {
		fail(w, err)
		return
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='2s'`); err != nil {
		fail(w, err)
		return
	}
	// This is the same lock acquired BEFORE publishing a running ledger row.
	// Owning it proves that no runner transaction can still commit this attempt.
	if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(3110003)`); err != nil {
		fail(w, err)
		return
	}
	if err = protectedHistory(ctx, tx); err != nil {
		fail(w, err)
		return
	}
	access, err := policy.DatabaseAccess(ctx, s.db, p.ID, id)
	if err != nil {
		fail(w, err)
		return
	}
	if !policy.Allows(access, true) {
		fail(w, connections.ErrForbidden)
		return
	}
	meta, err := s.db.Begin(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	defer rollback(meta)
	var locked int64
	if err = meta.QueryRow(ctx, `SELECT id FROM database_connections WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		fail(w, err)
		return
	}
	if err = bindTarget(ctx, meta, tx, id); err != nil {
		fail(w, err)
		return
	}
	rows, err := meta.Query(ctx, `SELECT execution_id,version,sql,executed_by,receipt_version FROM migration_history
 WHERE database_id=$1 AND status IN ('running','unknown') ORDER BY version LIMIT 100 FOR UPDATE`, id)
	if err != nil {
		fail(w, err)
		return
	}
	type pending struct {
		execution *string
		version   int64
		sql       string
		actor     int64
		format    int
	}
	items := []pending{}
	for rows.Next() {
		var item pending
		if err = rows.Scan(&item.execution, &item.version, &item.sql, &item.actor, &item.format); err != nil {
			break
		}
		items = append(items, item)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		fail(w, err)
		return
	}
	results := []Result{}
	for _, item := range items {
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
			var at time.Time
			err = tx.QueryRow(ctx, `SELECT version,sql,executed_by,committed_at,signature FROM _terserah.migration_receipts WHERE execution_id=$1`, out.ExecutionID).Scan(&receiptVersion, &sql, &actor, &at, &signature)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				out.Status = "failed"
				code = "reconciled_rollback"
				now := time.Now().UTC()
				finished = &now
			case err != nil:
				fail(w, err)
				return
			case signature != nil && receiptVersion == item.version && sql == item.sql && actor == item.actor && hmac.Equal([]byte(*signature), []byte(s.signature(id, out.ExecutionID, item.version, item.sql, item.actor))):
				out.Status = "success"
				code = ""
				finished = &at
			default:
				code = "receipt_mismatch"
			}
		}
		if code != "" {
			out.ErrorCode = &code
		}
		if _, err = meta.Exec(ctx, `UPDATE migration_history SET status=$1,error_code=$2,finished_at=$3 WHERE database_id=$4 AND version=$5 AND status IN ('running','unknown')`, out.Status, out.ErrorCode, finished, id, item.version); err != nil {
			fail(w, err)
			return
		}
		results = append(results, out)
	}
	if err = meta.Commit(ctx); err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]any{"reconciled": results})
}
