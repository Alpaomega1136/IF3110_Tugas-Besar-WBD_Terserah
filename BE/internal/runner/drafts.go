package runner

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

const draftColumns = "d.id, d.database_id, d.title, d.sql, d.revision, d.created_by, u.username, d.created_at, d.updated_at"

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	err := row.Scan(
		&d.ID,
		&d.DatabaseID,
		&d.Title,
		&d.SQL,
		&d.Revision,
		&d.CreatedBy,
		&d.CreatedByUsername,
		&d.CreatedAt,
		&d.UpdatedAt,
	)
	return d, err
}

func (s *Service) listDraftsPage(ctx context.Context, dbID, after int64, limit int) ([]Draft, bool, *string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+draftColumns+`
		FROM migration_drafts d
		JOIN users u ON u.id = d.created_by
		WHERE d.database_id = $1 AND d.id > $2
		ORDER BY d.id
		LIMIT $3
	`, dbID, after, limit+1)
	if err != nil {
		return nil, false, nil, err
	}
	defer rows.Close()

	items := []Draft{}
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, false, nil, err
		}
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, nil, err
	}

	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	var next *string
	if more {
		value := strconv.FormatInt(items[len(items)-1].ID, 10)
		next = &value
	}
	return items, more, next, nil
}

func (s *Service) getDraft(ctx context.Context, dbID, draftID int64) (Draft, error) {
	return scanDraft(s.db.QueryRow(ctx, `
		SELECT `+draftColumns+`
		FROM migration_drafts d
		JOIN users u ON u.id = d.created_by
		WHERE d.database_id = $1 AND d.id = $2
	`, dbID, draftID))
}

func (s *Service) createDraft(ctx context.Context, dbID, userID int64, title, sql string) (Draft, error) {
	return scanDraft(s.db.QueryRow(ctx, `
		WITH d AS (
			INSERT INTO migration_drafts (database_id, title, sql, created_by)
			VALUES ($1, $2, $3, $4)
			RETURNING *
		)
		SELECT `+draftColumns+`
		FROM d
		JOIN users u ON u.id = d.created_by
	`, dbID, title, sql, userID))
}

func (s *Service) updateDraft(ctx context.Context, dbID, draftID, revision int64, title, sql string) (Draft, error) {
	d, err := scanDraft(s.db.QueryRow(ctx, `
		WITH d AS (
			UPDATE migration_drafts
			SET title = $1, sql = $2, revision = revision + 1, updated_at = now()
			WHERE database_id = $3 AND id = $4 AND revision = $5
			RETURNING *
		)
		SELECT `+draftColumns+`
		FROM d
		JOIN users u ON u.id = d.created_by
	`, title, sql, dbID, draftID, revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, errConflict
	}
	return d, err
}
