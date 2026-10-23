// Admin handlers and PostgreSQL store adapted from BE-01/M1@3dac889.
package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/httpx"
	"terserah/backend/internal/policy"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
)

type UserView struct {
	ID              int64     `json:"id,string"`
	Username        string    `json:"username"`
	IsPlatformAdmin bool      `json:"is_platform_admin"`
	IsActive        bool      `json:"is_active"`
	GroupIDs        []string  `json:"group_ids"`
	CreatedAt       time.Time `json:"created_at"`
}

type GrantView struct {
	GroupID     int64         `json:"group_id,string"`
	DatabaseID  int64         `json:"database_id,string"`
	AccessLevel policy.Access `json:"access_level"`
}

type GroupView struct {
	ID      int64       `json:"id,string"`
	Name    string      `json:"name"`
	UserIDs []string    `json:"user_ids"`
	Grants  []GrantView `json:"grants"`
}

type UpdateUserParams struct {
	PasswordHash    *string
	IsPlatformAdmin *bool
	IsActive        *bool
}

type Store interface {
	ListUsers(ctx context.Context) ([]UserView, error)
	CreateUser(ctx context.Context, username, passwordHash string, isPlatformAdmin bool, now time.Time) (UserView, error)
	UpdateUser(ctx context.Context, id int64, params UpdateUserParams, now time.Time) (UserView, error)

	ListGroups(ctx context.Context) ([]GroupView, error)
	CreateGroup(ctx context.Context, name string) (GroupView, error)
	AddUserToGroup(ctx context.Context, groupID, userID int64) error
	RemoveUserFromGroup(ctx context.Context, groupID, userID int64, now time.Time) error

	SetGroupDatabaseGrant(ctx context.Context, groupID, databaseID int64, access policy.Access, now time.Time) (GrantView, error)
	RevokeGroupDatabaseGrant(ctx context.Context, groupID, databaseID int64, now time.Time) error
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: store, now: now}
}

func (s *Service) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot list users")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Service) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		IsPlatformAdmin bool   `json:"is_platform_admin"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	username, err := auth.ValidateUsername(req.Username)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	u, err := s.store.CreateUser(r.Context(), username, hash, req.IsPlatformAdmin, s.now())
	if err != nil {
		if errors.Is(err, ErrConflict) {
			httpx.Error(w, http.StatusConflict, "conflict", "Username already exists")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot create user")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"user": u})
}

func (s *Service) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
		return
	}
	var req struct {
		Password        *string `json:"password"`
		IsPlatformAdmin *bool   `json:"is_platform_admin"`
		IsActive        *bool   `json:"is_active"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.Password == nil && req.IsPlatformAdmin == nil && req.IsActive == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "At least one field must be provided")
		return
	}
	var params UpdateUserParams
	params.IsPlatformAdmin = req.IsPlatformAdmin
	params.IsActive = req.IsActive
	if req.Password != nil {
		h, err := auth.HashPassword(*req.Password)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		params.PasswordHash = &h
	}
	u, err := s.store.UpdateUser(r.Context(), id, params, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "not_found", "User not found")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot update user")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Service) HandleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot list groups")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Service) HandleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) < 1 || len(name) > 100 {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Group name must be 1..100 characters")
		return
	}
	g, err := s.store.CreateGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			httpx.Error(w, http.StatusConflict, "conflict", "Group name already exists")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot create group")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"group": g})
}

func (s *Service) HandleAddUserToGroup(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	userID, err := parseID(r.PathValue("userID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
		return
	}
	if err := s.store.AddUserToGroup(r.Context(), groupID, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "not_found", "Group or user not found")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot add user to group")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleRemoveUserFromGroup(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	userID, err := parseID(r.PathValue("userID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
		return
	}
	if err := s.store.RemoveUserFromGroup(r.Context(), groupID, userID, s.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "not_found", "Group membership not found")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot remove user from group")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleSetGrant(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	databaseID, err := parseID(r.PathValue("databaseID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return
	}
	var req struct {
		AccessLevel string `json:"access_level"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	access, ok := policy.ParseAccess(req.AccessLevel)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "access_level must be 'view' or 'edit'")
		return
	}
	grant, err := s.store.SetGroupDatabaseGrant(r.Context(), groupID, databaseID, access, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "not_found", "Group or database not found")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot set database grant")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"grant": grant})
}

func (s *Service) HandleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	databaseID, err := parseID(r.PathValue("databaseID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return
	}
	if err := s.store.RevokeGroupDatabaseGrant(r.Context(), groupID, databaseID, s.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "not_found", "Grant not found")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot revoke database grant")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// PostgresStore implementasi Store admin berbasis PostgreSQL metadata platform.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) ListUsers(ctx context.Context) ([]UserView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, username, is_platform_admin, is_active, created_at
		FROM users
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := []UserView{}
	idxByID := make(map[int64]int)
	for rows.Next() {
		var u UserView
		if err := rows.Scan(&u.ID, &u.Username, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.GroupIDs = []string{}
		idxByID[u.ID] = len(users)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mRows, err := s.pool.Query(ctx, `SELECT user_id, group_id FROM user_groups ORDER BY group_id ASC`)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()
	for mRows.Next() {
		var uid, gid int64
		if err := mRows.Scan(&uid, &gid); err != nil {
			return nil, err
		}
		if idx, ok := idxByID[uid]; ok {
			users[idx].GroupIDs = append(users[idx].GroupIDs, strconv.FormatInt(gid, 10))
		}
	}
	return users, mRows.Err()
}

func (s *PostgresStore) CreateUser(ctx context.Context, username, passwordHash string, isPlatformAdmin bool, now time.Time) (UserView, error) {
	var u UserView
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, is_platform_admin, is_active, created_at)
		VALUES ($1, $2, $3, true, $4)
		RETURNING id, username, is_platform_admin, is_active, created_at
	`, username, passwordHash, isPlatformAdmin, now).Scan(&u.ID, &u.Username, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return UserView{}, ErrConflict
		}
		return UserView{}, err
	}
	u.GroupIDs = []string{}
	return u, nil
}

func (s *PostgresStore) UpdateUser(ctx context.Context, id int64, params UpdateUserParams, now time.Time) (UserView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserView{}, err
	}
	defer tx.Rollback(context.Background())

	var u UserView
	var oldHash string
	err = tx.QueryRow(ctx, `
		SELECT id, username, password_hash, is_platform_admin, is_active, created_at
		FROM users
		WHERE id = $1
		FOR UPDATE
	`, id).Scan(&u.ID, &u.Username, &oldHash, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserView{}, ErrNotFound
	}
	if err != nil {
		return UserView{}, err
	}

	newHash := oldHash
	if params.PasswordHash != nil {
		newHash = *params.PasswordHash
	}
	newAdmin := u.IsPlatformAdmin
	if params.IsPlatformAdmin != nil {
		newAdmin = *params.IsPlatformAdmin
	}
	newActive := u.IsActive
	if params.IsActive != nil {
		newActive = *params.IsActive
	}

	err = tx.QueryRow(ctx, `
		UPDATE users
		SET password_hash = $2, is_platform_admin = $3, is_active = $4
		WHERE id = $1
		RETURNING id, username, is_platform_admin, is_active, created_at
	`, id, newHash, newAdmin, newActive).Scan(&u.ID, &u.Username, &u.IsPlatformAdmin, &u.IsActive, &u.CreatedAt)
	if err != nil {
		return UserView{}, err
	}

	privilegeReduced := !newActive || (params.IsPlatformAdmin != nil && !newAdmin) || params.PasswordHash != nil
	privilegeElevated := newActive && (params.IsPlatformAdmin != nil && newAdmin) && params.PasswordHash == nil
	if privilegeReduced {
		if err := auth.ChangeUserAccess(ctx, tx, id, false); err != nil {
			return UserView{}, err
		}
	} else if privilegeElevated {
		if err := auth.ChangeUserAccess(ctx, tx, id, true); err != nil {
			return UserView{}, err
		}
	}

	u.GroupIDs = []string{}
	gRows, err := tx.Query(ctx, `SELECT group_id FROM user_groups WHERE user_id = $1 ORDER BY group_id ASC`, id)
	if err != nil {
		return UserView{}, err
	}
	for gRows.Next() {
		var gid int64
		if err := gRows.Scan(&gid); err != nil {
			gRows.Close()
			return UserView{}, err
		}
		u.GroupIDs = append(u.GroupIDs, strconv.FormatInt(gid, 10))
	}
	gRows.Close()
	if err := gRows.Err(); err != nil {
		return UserView{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return UserView{}, err
	}
	return u, nil
}

func (s *PostgresStore) ListGroups(ctx context.Context) ([]GroupView, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM groups ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := []GroupView{}
	idxByID := make(map[int64]int)
	for rows.Next() {
		var g GroupView
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		g.UserIDs = []string{}
		g.Grants = []GrantView{}
		idxByID[g.ID] = len(groups)
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	uRows, err := s.pool.Query(ctx, `SELECT group_id, user_id FROM user_groups ORDER BY user_id ASC`)
	if err != nil {
		return nil, err
	}
	defer uRows.Close()
	for uRows.Next() {
		var gid, uid int64
		if err := uRows.Scan(&gid, &uid); err != nil {
			return nil, err
		}
		if idx, ok := idxByID[gid]; ok {
			groups[idx].UserIDs = append(groups[idx].UserIDs, strconv.FormatInt(uid, 10))
		}
	}
	if err := uRows.Err(); err != nil {
		return nil, err
	}

	gRows, err := s.pool.Query(ctx, `SELECT group_id, database_id, access_level FROM group_database_permissions ORDER BY database_id ASC`)
	if err != nil {
		return nil, err
	}
	defer gRows.Close()
	for gRows.Next() {
		var gr GrantView
		var lvl string
		if err := gRows.Scan(&gr.GroupID, &gr.DatabaseID, &lvl); err != nil {
			return nil, err
		}
		gr.AccessLevel = policy.Access(lvl)
		if idx, ok := idxByID[gr.GroupID]; ok {
			groups[idx].Grants = append(groups[idx].Grants, gr)
		}
	}
	return groups, gRows.Err()
}

func (s *PostgresStore) CreateGroup(ctx context.Context, name string) (GroupView, error) {
	var g GroupView
	err := s.pool.QueryRow(ctx, `INSERT INTO groups (name) VALUES ($1) RETURNING id, name`, name).Scan(&g.ID, &g.Name)
	if err != nil {
		if isUniqueViolation(err) {
			return GroupView{}, ErrConflict
		}
		return GroupView{}, err
	}
	g.UserIDs = []string{}
	g.Grants = []GrantView{}
	return g, nil
}

func (s *PostgresStore) AddUserToGroup(ctx context.Context, groupID, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := lockGroup(ctx, tx, groupID); err != nil {
		return err
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1) AND EXISTS(SELECT 1 FROM users WHERE id = $2)`, groupID, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_groups (user_id, group_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, groupID); err != nil {
		return err
	}
	if err := auth.ChangeUserAccess(ctx, tx, userID, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) RemoveUserFromGroup(ctx context.Context, groupID, userID int64, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := lockGroup(ctx, tx, groupID); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `DELETE FROM user_groups WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := auth.ChangeUserAccess(ctx, tx, userID, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) SetGroupDatabaseGrant(ctx context.Context, groupID, databaseID int64, access policy.Access, now time.Time) (GrantView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return GrantView{}, err
	}
	defer tx.Rollback(context.Background())
	if err := lockGroup(ctx, tx, groupID); err != nil {
		return GrantView{}, err
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1) AND EXISTS(SELECT 1 FROM database_connections WHERE id = $2)`, groupID, databaseID).Scan(&exists); err != nil {
		return GrantView{}, err
	}
	if !exists {
		return GrantView{}, ErrNotFound
	}

	var prevLevel string
	err = tx.QueryRow(ctx, `SELECT access_level FROM group_database_permissions WHERE group_id = $1 AND database_id = $2 FOR UPDATE`, groupID, databaseID).Scan(&prevLevel)
	hadPrev := true
	if errors.Is(err, pgx.ErrNoRows) {
		hadPrev = false
		err = nil
	}
	if err != nil {
		return GrantView{}, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO group_database_permissions (group_id, database_id, access_level)
		VALUES ($1, $2, $3)
		ON CONFLICT (group_id, database_id) DO UPDATE SET access_level = EXCLUDED.access_level
	`, groupID, databaseID, string(access))
	if err != nil {
		return GrantView{}, err
	}

	if hadPrev && policy.Access(prevLevel) == policy.Edit && access == policy.View {
		if err := changeGroupAccess(ctx, tx, groupID, false); err != nil {
			return GrantView{}, err
		}
	} else if !hadPrev || (policy.Access(prevLevel) == policy.View && access == policy.Edit) {
		if err := changeGroupAccess(ctx, tx, groupID, true); err != nil {
			return GrantView{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return GrantView{}, err
	}
	return GrantView{GroupID: groupID, DatabaseID: databaseID, AccessLevel: access}, nil
}

func (s *PostgresStore) RevokeGroupDatabaseGrant(ctx context.Context, groupID, databaseID int64, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := lockGroup(ctx, tx, groupID); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `DELETE FROM group_database_permissions WHERE group_id = $1 AND database_id = $2`, groupID, databaseID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := changeGroupAccess(ctx, tx, groupID, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid positive integer id")
	}
	return id, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
