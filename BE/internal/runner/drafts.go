package runner

import (
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/httpx"
)

const draftColumns = "d.id,d.database_id,d.title,d.sql,d.revision,d.created_by,u.username,d.created_at,d.updated_at"

func scanDraft(row pgx.Row) (Draft, error) {
	var d Draft
	err := row.Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.Revision, &d.CreatedBy, &d.CreatedByUsername, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// Adapted from BE-01/M1 draft listing, with the shared revision/payload contract.
func (s *Service) listDrafts(w http.ResponseWriter, r *http.Request) {
	_, id, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	limit := 50
	var after int64
	if r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			fail(w, connections.ErrInvalid)
			return
		}
	}
	if r.URL.Query().Has("after") {
		after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil || after < 1 {
			fail(w, connections.ErrInvalid)
			return
		}
	}
	rows, err := s.db.Query(r.Context(), `SELECT `+draftColumns+` FROM migration_drafts d JOIN users u ON u.id=d.created_by WHERE d.database_id=$1 AND d.id>$2 ORDER BY d.id LIMIT $3`, id, after, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	items := []Draft{}
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			fail(w, err)
			return
		}
		items = append(items, d)
	}
	if err = rows.Err(); err != nil {
		fail(w, err)
		return
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
	httpx.JSON(w, 200, map[string]any{"drafts": items, "has_more": more, "next_after": next})
}
