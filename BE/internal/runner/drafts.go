package runner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"terserah/backend/internal/batch"
)

const draftColumns = "d.id, d.database_id, d.title, d.sql, d.sql_down, d.irreversible, d.irreversible_reason, d.revision, d.created_by, u.username, d.created_at, d.updated_at, EXISTS (SELECT 1 FROM migration_history h WHERE h.database_id=d.database_id AND h.draft_id=d.id)"

var errDraftDefinition = errors.New("definisi migration tidak valid")
var errDraftLocked = errors.New("definisi migration yang pernah dieksekusi tidak dapat diubah; buat migration baru")

// validasiDefinisiDraft memastikan SQL Up/Down dan alasan irreversible konsisten sebelum disimpan.
func validasiDefinisiDraft(in draftInput) (draftInput, error) {
	in.Title = strings.TrimSpace(in.Title)
	if len(in.Title) < 1 || len(in.Title) > 200 || strings.ContainsRune(in.Title, 0) {
		return in, fmt.Errorf("%w: judul harus 1-200 byte dan tidak boleh mengandung NUL", errDraftDefinition)
	}
	if err := validateDraftSQL(in.SQL); err != nil {
		return in, fmt.Errorf("%w: SQL Up kosong, terlalu panjang, atau tidak didukung", errDraftDefinition)
	}
	if in.Irreversible {
		if in.SQLDown != nil {
			return in, fmt.Errorf("%w: migration irreversible tidak boleh memiliki SQL Down", errDraftDefinition)
		}
		if in.IrreversibleReason == nil {
			return in, fmt.Errorf("%w: alasan irreversible wajib diisi", errDraftDefinition)
		}
		alasan := strings.TrimSpace(*in.IrreversibleReason)
		if len(alasan) < 1 || len(alasan) > 500 || strings.ContainsRune(alasan, 0) {
			return in, fmt.Errorf("%w: alasan irreversible harus 1-500 byte dan tidak boleh mengandung NUL", errDraftDefinition)
		}
		in.IrreversibleReason = &alasan
		return in, nil
	}
	if in.IrreversibleReason != nil {
		return in, fmt.Errorf("%w: alasan irreversible hanya boleh diisi untuk migration irreversible", errDraftDefinition)
	}
	if in.SQLDown == nil {
		return in, fmt.Errorf("%w: SQL Down wajib diisi untuk migration reversible", errDraftDefinition)
	}
	config, _ := batch.Parse(in.SQL)
	down, downErr := batch.ParseDown(*in.SQLDown)
	if downErr != nil {
		return in, fmt.Errorf("%w: SQL Down batch tidak valid", errDraftDefinition)
	}
	if config != nil {
		expected, err := batch.DownSQL(*config)
		if err != nil || down == nil || strings.TrimSpace(*in.SQLDown) != expected {
			return in, fmt.Errorf("%w: batch reversible memerlukan SQL Down pemulihan yang sesuai konfigurasi Up", errDraftDefinition)
		}
		// Only this exact generated statement may bind recovery parameters.
		return in, nil
	} else if down != nil {
		return in, fmt.Errorf("%w: SQL Down pemulihan batch hanya untuk Up batch", errDraftDefinition)
	}
	if _, err := statements(*in.SQLDown); err != nil {
		return in, fmt.Errorf("%w: SQL Down kosong, terlalu panjang, atau tidak didukung", errDraftDefinition)
	}
	return in, nil
}

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	err := row.Scan(
		&d.ID,
		&d.DatabaseID,
		&d.Title,
		&d.SQL,
		&d.SQLDown,
		&d.Irreversible,
		&d.IrreversibleReason,
		&d.Revision,
		&d.CreatedBy,
		&d.CreatedByUsername,
		&d.CreatedAt,
		&d.UpdatedAt,
		&d.Immutable,
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

func (s *Service) createDraft(ctx context.Context, dbID, userID int64, in draftInput) (Draft, error) {
	in, err := validasiDefinisiDraft(in)
	if err != nil {
		return Draft{}, err
	}
	return scanDraft(s.db.QueryRow(ctx, `
		WITH d AS (
			INSERT INTO migration_drafts (database_id, title, sql, sql_down, irreversible, irreversible_reason, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING *
		)
		SELECT `+draftColumns+`
		FROM d
		JOIN users u ON u.id = d.created_by
	`, dbID, in.Title, in.SQL, in.SQLDown, in.Irreversible, in.IrreversibleReason, userID))
}

func (s *Service) updateDraft(ctx context.Context, dbID, draftID int64, in draftInput) (Draft, error) {
	in, err := validasiDefinisiDraft(in)
	if err != nil {
		return Draft{}, err
	}
	d, err := scanDraft(s.db.QueryRow(ctx, `
		WITH d AS (
			UPDATE migration_drafts
			SET title = $1, sql = $2, sql_down = $3, irreversible = $4,
				irreversible_reason = $5, revision = revision + 1, updated_at = now()
			WHERE database_id = $6 AND id = $7 AND revision = $8
			RETURNING *
		)
		SELECT `+draftColumns+`
		FROM d
		JOIN users u ON u.id = d.created_by
	`, in.Title, in.SQL, in.SQLDown, in.Irreversible, in.IrreversibleReason, dbID, draftID, in.Revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, errConflict
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "migration_drafts_executed_immutable" {
		return Draft{}, errDraftLocked
	}
	return d, err
}
