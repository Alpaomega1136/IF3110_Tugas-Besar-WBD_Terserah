package runner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"terserah/backend/internal/auth"
)

type planResult struct {
	SQL   string          `json:"sql"`
	Plan  json.RawMessage `json:"plan,omitempty"`
	Error string          `json:"error,omitempty"`
}

func (s *Service) explainDraft(ctx context.Context, p auth.Principal, dbID, draftID, revision int64) ([]planResult, error) {
	var sql string
	var actualRevision int64
	err := s.db.QueryRow(ctx, `
		SELECT sql, revision
		FROM migration_drafts
		WHERE database_id = $1 AND id = $2
	`, dbID, draftID).Scan(&sql, &actualRevision)
	if err != nil {
		return nil, err
	}
	if actualRevision != revision {
		return nil, errConflict
	}

	parts, err := statements(sql)
	if err != nil {
		return nil, err
	}

	pool, err := s.targets.OpenTarget(ctx, p, dbID, true)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	results := make([]planResult, 0, len(parts))
	for _, part := range parts {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, err
		}
		result := planResult{SQL: part}
		_, err = tx.Exec(ctx, `SET LOCAL statement_timeout='5s'; SET LOCAL lock_timeout='2s'; SET LOCAL standard_conforming_strings=on`)
		if err == nil {
			err = tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+part).Scan(&result.Plan)
		}
		rollback(ctx, tx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var pgerr *pgconn.PgError
			if errors.As(err, &pgerr) {
				result.Error = "PostgreSQL tidak dapat menganalisis statement ini (kode " + pgerr.Code + "). Periksa sintaks, dependensi statement sebelumnya, dan dukungan EXPLAIN."
			} else {
				return nil, err
			}
		}
		results = append(results, result)
	}
	return results, nil
}
