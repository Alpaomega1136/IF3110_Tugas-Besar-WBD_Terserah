package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/httpx"
)

type planResult struct {
	SQL   string          `json:"sql"`
	Plan  json.RawMessage `json:"plan,omitempty"`
	Error string          `json:"error,omitempty"`
}

func (s *Service) explain(w http.ResponseWriter, r *http.Request) {
	p, id, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	draftID, err := strconv.ParseInt(r.PathValue("draftID"), 10, 64)
	var in struct {
		Revision int64 `json:"revision,string"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err != nil || draftID < 1 || in.Revision < 1 {
		fail(w, connections.ErrInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()
	var sql string
	var revision int64
	err = s.db.QueryRow(ctx, `SELECT sql,revision FROM migration_drafts WHERE database_id=$1 AND id=$2`, id, draftID).Scan(&sql, &revision)
	if err != nil {
		fail(w, err)
		return
	}
	if revision != in.Revision {
		fail(w, errConflict)
		return
	}
	parts, err := statements(sql)
	if err != nil {
		fail(w, err)
		return
	}
	pool, err := s.targets.OpenTarget(ctx, p, id, true)
	if err != nil {
		fail(w, err)
		return
	}
	defer pool.Close()
	results := make([]planResult, 0, len(parts))
	for _, part := range parts {
		tx, e := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if e != nil {
			fail(w, e)
			return
		}
		result := planResult{SQL: part}
		_, e = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='2s'; SET LOCAL standard_conforming_strings=on`)
		if e == nil {
			e = tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+part).Scan(&result.Plan)
		}
		rollback(tx)
		if e != nil {
			if ctx.Err() != nil {
				fail(w, ctx.Err())
				return
			}
			var pgerr *pgconn.PgError
			if errors.As(e, &pgerr) {
				result.Error = "PostgreSQL tidak dapat menganalisis statement ini (kode " + pgerr.Code + "). Periksa sintaks, dependensi statement sebelumnya, dan dukungan EXPLAIN."
			} else {
				fail(w, e)
				return
			}
		}
		results = append(results, result)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"revision": strconv.FormatInt(revision, 10), "statements": results})
}
