package admin

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/httpx"
)

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	routes := map[string]http.HandlerFunc{
		"GET /api/v1/admin/users":                                 s.HandleListUsers,
		"POST /api/v1/admin/users":                                s.HandleCreateUser,
		"PATCH /api/v1/admin/users/{id}":                          s.HandleUpdateUser,
		"GET /api/v1/admin/groups":                                s.HandleListGroups,
		"POST /api/v1/admin/groups":                               s.HandleCreateGroup,
		"PUT /api/v1/admin/groups/{id}/users/{userID}":            s.HandleAddUserToGroup,
		"DELETE /api/v1/admin/groups/{id}/users/{userID}":         s.HandleRemoveUserFromGroup,
		"PUT /api/v1/admin/groups/{id}/databases/{databaseID}":    s.HandleSetGrant,
		"DELETE /api/v1/admin/groups/{id}/databases/{databaseID}": s.HandleRevokeGrant,
	}
	for route, handler := range routes {
		mux.HandleFunc(route, sessions.Require(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.User(r.Context())
			if !ok || !p.IsPlatformAdmin {
				httpx.Error(w, 403, "forbidden", "Platform Admin access required")
				return
			}
			handler(w, r)
		}))
	}
}

// Serialize group grants and membership changes, including first-time grants
// for which SELECT ... FOR UPDATE on the grant alone cannot lock a row.
func lockGroup(ctx context.Context, tx pgx.Tx, id int64) error {
	var groupID int64
	err := tx.QueryRow(ctx, "SELECT id FROM groups WHERE id=$1 FOR UPDATE", id).Scan(&groupID)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	return err
}

func changeGroupAccess(ctx context.Context, tx pgx.Tx, groupID int64, increase bool) error {
	rows, err := tx.Query(ctx, "SELECT user_id FROM user_groups WHERE group_id=$1 ORDER BY user_id", groupID)
	if err != nil {
		return err
	}
	var users []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		users = append(users, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range users {
		if err = auth.ChangeUserAccess(ctx, tx, id, increase); err != nil {
			return err
		}
	}
	return nil
}
