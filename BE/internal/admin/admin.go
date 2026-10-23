package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
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

type AccessibleDatabase struct {
	ID              int64         `json:"id,string"`
	Name            string        `json:"name"`
	Host            string        `json:"host"`
	Port            int           `json:"port"`
	DatabaseName    string        `json:"database_name"`
	SSLMode         string        `json:"sslmode"`
	EffectiveAccess policy.Access `json:"effective_access"`
	CreatedAt       time.Time     `json:"created_at"`
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

	ListAccessibleDatabases(ctx context.Context, userID int64) ([]AccessibleDatabase, error)
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
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Service) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		IsPlatformAdmin bool   `json:"is_platform_admin"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	username, err := auth.ValidateUsername(req.Username)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	u, err := s.store.CreateUser(r.Context(), username, hash, req.IsPlatformAdmin, s.now())
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", "Username already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot create user")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": u})
}

func (s *Service) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
		return
	}
	var req struct {
		Password        *string `json:"password"`
		IsPlatformAdmin *bool   `json:"is_platform_admin"`
		IsActive        *bool   `json:"is_active"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	if req.Password == nil && req.IsPlatformAdmin == nil && req.IsActive == nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "At least one field must be provided")
		return
	}
	var params UpdateUserParams
	params.IsPlatformAdmin = req.IsPlatformAdmin
	params.IsActive = req.IsActive
	if req.Password != nil {
		h, err := auth.HashPassword(*req.Password)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		params.PasswordHash = &h
	}
	u, err := s.store.UpdateUser(r.Context(), id, params, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "User not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot update user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Service) HandleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot list groups")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Service) HandleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) < 1 || len(name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_input", "Group name must be 1..100 characters")
		return
	}
	g, err := s.store.CreateGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", "Group name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot create group")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"group": g})
}

func (s *Service) HandleAddUserToGroup(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	userID, err := parseID(r.PathValue("userID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
		return
	}
	if err := s.store.AddUserToGroup(r.Context(), groupID, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Group or user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot add user to group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleRemoveUserFromGroup(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	userID, err := parseID(r.PathValue("userID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
		return
	}
	if err := s.store.RemoveUserFromGroup(r.Context(), groupID, userID, s.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Group membership not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot remove user from group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleSetGrant(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	databaseID, err := parseID(r.PathValue("databaseID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return
	}
	var req struct {
		AccessLevel string `json:"access_level"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	access, ok := policy.ParseAccess(req.AccessLevel)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_input", "access_level must be 'view' or 'edit'")
		return
	}
	grant, err := s.store.SetGroupDatabaseGrant(r.Context(), groupID, databaseID, access, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Group or database not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot set database grant")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grant": grant})
}

func (s *Service) HandleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	databaseID, err := parseID(r.PathValue("databaseID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return
	}
	if err := s.store.RevokeGroupDatabaseGrant(r.Context(), groupID, databaseID, s.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Grant not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot revoke database grant")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleListAccessibleDatabases(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return
	}
	dbs, err := s.store.ListAccessibleDatabases(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot list accessible databases")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": dbs})
}

// PostgresStore implementasi Store admin berbasis PostgreSQL metadata platform.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// PostgresGrantLoader menghasilkan policy.GrantFunc berbasis database metadata PostgreSQL.
func PostgresGrantLoader(pool *pgxpool.Pool) policy.GrantFunc {
	return func(ctx context.Context, userID, databaseID int64) ([]policy.Access, error) {
		rows, err := pool.Query(ctx, `
			SELECT gdp.access_level
			FROM group_database_permissions gdp
			JOIN user_groups ug ON ug.group_id = gdp.group_id
			WHERE ug.user_id = $1 AND gdp.database_id = $2
		`, userID, databaseID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var grants []policy.Access
		for rows.Next() {
			var lvl string
			if err := rows.Scan(&lvl); err != nil {
				return nil, err
			}
			grants = append(grants, policy.Access(lvl))
		}
		return grants, rows.Err()
	}
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
		if err := auth.RevokeUserSessionsExec(ctx, tx, id, now); err != nil {
			return UserView{}, err
		}
	} else if privilegeElevated {
		if err := auth.MarkUserSessionsForRotationExec(ctx, tx, id); err != nil {
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
	if err := auth.MarkUserSessionsForRotationExec(ctx, tx, userID); err != nil {
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

	tag, err := tx.Exec(ctx, `DELETE FROM user_groups WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := auth.RevokeUserSessionsExec(ctx, tx, userID, now); err != nil {
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
		if err := auth.RevokeGroupSessionsExec(ctx, tx, groupID, now); err != nil {
			return GrantView{}, err
		}
	} else if !hadPrev || (policy.Access(prevLevel) == policy.View && access == policy.Edit) {
		if err := auth.MarkGroupSessionsForRotationExec(ctx, tx, groupID); err != nil {
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

	tag, err := tx.Exec(ctx, `DELETE FROM group_database_permissions WHERE group_id = $1 AND database_id = $2`, groupID, databaseID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := auth.RevokeGroupSessionsExec(ctx, tx, groupID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListAccessibleDatabases(ctx context.Context, userID int64) ([]AccessibleDatabase, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dc.id, dc.name, dc.host, dc.port, dc.database_name, dc.sslmode, dc.created_at, gdp.access_level
		FROM database_connections dc
		JOIN group_database_permissions gdp ON gdp.database_id = dc.id
		JOIN user_groups ug ON ug.group_id = gdp.group_id
		WHERE ug.user_id = $1
		ORDER BY dc.id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type dbEntry struct {
		db     AccessibleDatabase
		grants []policy.Access
	}
	byID := make(map[int64]*dbEntry)
	var order []int64
	for rows.Next() {
		var d AccessibleDatabase
		var level string
		if err := rows.Scan(&d.ID, &d.Name, &d.Host, &d.Port, &d.DatabaseName, &d.SSLMode, &d.CreatedAt, &level); err != nil {
			return nil, err
		}
		ent, ok := byID[d.ID]
		if !ok {
			ent = &dbEntry{db: d}
			byID[d.ID] = ent
			order = append(order, d.ID)
		}
		ent.grants = append(ent.grants, policy.Access(level))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := []AccessibleDatabase{}
	for _, id := range order {
		ent := byID[id]
		eff := policy.Effective(ent.grants)
		if eff != policy.None {
			ent.db.EffectiveAccess = eff
			result = append(result, ent.db)
		}
	}
	return result, nil
}

// MemoryStore implementasi Store admin in-memory untuk pengujian unit.
type MemoryStore struct {
	mu          sync.Mutex
	authStore   *auth.MemoryStore
	nextUID     int64
	nextGID     int64
	users       map[int64]UserView
	groups      map[int64]GroupView
	memberships map[[2]int64]bool
	grants      map[[2]int64]policy.Access
	databases   map[int64]AccessibleDatabase
}

func NewMemoryStore(authStore *auth.MemoryStore) *MemoryStore {
	return &MemoryStore{
		authStore:   authStore,
		users:       make(map[int64]UserView),
		groups:      make(map[int64]GroupView),
		memberships: make(map[[2]int64]bool),
		grants:      make(map[[2]int64]policy.Access),
		databases:   make(map[int64]AccessibleDatabase),
	}
}

func (m *MemoryStore) PutDatabase(db AccessibleDatabase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.databases[db.ID] = db
}

func (m *MemoryStore) ListUsers(_ context.Context) ([]UserView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UserView, 0, len(m.users))
	for _, u := range m.users {
		u.GroupIDs = m.groupIDsForUserLocked(u.ID)
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) CreateUser(_ context.Context, username, passwordHash string, isPlatformAdmin bool, now time.Time) (UserView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Username == username {
			return UserView{}, ErrConflict
		}
	}
	m.nextUID++
	id := m.nextUID
	if m.authStore != nil {
		au := m.authStore.PutUser(auth.User{
			ID:              id,
			Username:        username,
			PasswordHash:    passwordHash,
			IsPlatformAdmin: isPlatformAdmin,
			IsActive:        true,
			CreatedAt:       now,
		})
		id = au.ID
		if id > m.nextUID {
			m.nextUID = id
		}
	}
	u := UserView{
		ID:              id,
		Username:        username,
		IsPlatformAdmin: isPlatformAdmin,
		IsActive:        true,
		GroupIDs:        []string{},
		CreatedAt:       now,
	}
	m.users[id] = u
	return u, nil
}

func (m *MemoryStore) UpdateUser(ctx context.Context, id int64, params UpdateUserParams, now time.Time) (UserView, error) {
	m.mu.Lock()
	u, ok := m.users[id]
	if !ok {
		m.mu.Unlock()
		return UserView{}, ErrNotFound
	}
	if params.IsPlatformAdmin != nil {
		u.IsPlatformAdmin = *params.IsPlatformAdmin
	}
	if params.IsActive != nil {
		u.IsActive = *params.IsActive
	}
	m.users[id] = u
	u.GroupIDs = m.groupIDsForUserLocked(id)
	m.mu.Unlock()

	if m.authStore != nil {
		if au, err := m.authStore.GetUserByUsername(ctx, u.Username); err == nil {
			au.IsPlatformAdmin = u.IsPlatformAdmin
			au.IsActive = u.IsActive
			if params.PasswordHash != nil {
				au.PasswordHash = *params.PasswordHash
			}
			m.authStore.PutUser(au)
		}
		if !u.IsActive || (params.IsPlatformAdmin != nil && !u.IsPlatformAdmin) || params.PasswordHash != nil {
			_ = m.authStore.RevokeUserSessions(ctx, id, now)
		} else if params.IsPlatformAdmin != nil && u.IsPlatformAdmin {
			_ = m.authStore.MarkUserSessionsForRotation(ctx, id)
		}
	}
	return u, nil
}

func (m *MemoryStore) ListGroups(_ context.Context) ([]GroupView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]GroupView, 0, len(m.groups))
	for _, g := range m.groups {
		g.UserIDs = []string{}
		for pair := range m.memberships {
			if pair[0] == g.ID {
				g.UserIDs = append(g.UserIDs, strconv.FormatInt(pair[1], 10))
			}
		}
		sort.Strings(g.UserIDs)
		g.Grants = []GrantView{}
		for pair, acc := range m.grants {
			if pair[0] == g.ID {
				g.Grants = append(g.Grants, GrantView{GroupID: g.ID, DatabaseID: pair[1], AccessLevel: acc})
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) CreateGroup(_ context.Context, name string) (GroupView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.groups {
		if g.Name == name {
			return GroupView{}, ErrConflict
		}
	}
	m.nextGID++
	g := GroupView{ID: m.nextGID, Name: name, UserIDs: []string{}, Grants: []GrantView{}}
	m.groups[g.ID] = g
	return g, nil
}

func (m *MemoryStore) AddUserToGroup(ctx context.Context, groupID, userID int64) error {
	m.mu.Lock()
	_, gOK := m.groups[groupID]
	_, uOK := m.users[userID]
	if !gOK || !uOK {
		m.mu.Unlock()
		return ErrNotFound
	}
	m.memberships[[2]int64{groupID, userID}] = true
	m.mu.Unlock()
	if m.authStore != nil {
		_ = m.authStore.MarkUserSessionsForRotation(ctx, userID)
	}
	return nil
}

func (m *MemoryStore) RemoveUserFromGroup(ctx context.Context, groupID, userID int64, now time.Time) error {
	m.mu.Lock()
	key := [2]int64{groupID, userID}
	if !m.memberships[key] {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.memberships, key)
	m.mu.Unlock()
	if m.authStore != nil {
		_ = m.authStore.RevokeUserSessions(ctx, userID, now)
	}
	return nil
}

func (m *MemoryStore) SetGroupDatabaseGrant(ctx context.Context, groupID, databaseID int64, access policy.Access, now time.Time) (GrantView, error) {
	m.mu.Lock()
	_, gOK := m.groups[groupID]
	_, dbOK := m.databases[databaseID]
	if !gOK || !dbOK {
		m.mu.Unlock()
		return GrantView{}, ErrNotFound
	}
	key := [2]int64{groupID, databaseID}
	prev, hadPrev := m.grants[key]
	m.grants[key] = access
	var memberIDs []int64
	for pair := range m.memberships {
		if pair[0] == groupID {
			memberIDs = append(memberIDs, pair[1])
		}
	}
	m.mu.Unlock()

	if m.authStore != nil {
		if hadPrev && prev == policy.Edit && access == policy.View {
			for _, uid := range memberIDs {
				_ = m.authStore.RevokeUserSessions(ctx, uid, now)
			}
		} else if !hadPrev || (prev == policy.View && access == policy.Edit) {
			for _, uid := range memberIDs {
				_ = m.authStore.MarkUserSessionsForRotation(ctx, uid)
			}
		}
	}
	return GrantView{GroupID: groupID, DatabaseID: databaseID, AccessLevel: access}, nil
}

func (m *MemoryStore) RevokeGroupDatabaseGrant(ctx context.Context, groupID, databaseID int64, now time.Time) error {
	m.mu.Lock()
	key := [2]int64{groupID, databaseID}
	if _, ok := m.grants[key]; !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.grants, key)
	var memberIDs []int64
	for pair := range m.memberships {
		if pair[0] == groupID {
			memberIDs = append(memberIDs, pair[1])
		}
	}
	m.mu.Unlock()

	if m.authStore != nil {
		for _, uid := range memberIDs {
			_ = m.authStore.RevokeUserSessions(ctx, uid, now)
		}
	}
	return nil
}

func (m *MemoryStore) GrantLoader() policy.GrantFunc {
	return func(_ context.Context, userID, databaseID int64) ([]policy.Access, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var grants []policy.Access
		for pair := range m.memberships {
			if pair[1] == userID {
				if acc, ok := m.grants[[2]int64{pair[0], databaseID}]; ok {
					grants = append(grants, acc)
				}
			}
		}
		return grants, nil
	}
}

func (m *MemoryStore) DatabaseAccess(ctx context.Context, userID, databaseID int64) (policy.Access, error) {
	return policy.ResolveAccess(ctx, m.GrantLoader(), userID, databaseID)
}

func (m *MemoryStore) ListAccessibleDatabases(_ context.Context, userID int64) ([]AccessibleDatabase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []AccessibleDatabase{}
	for dbID, db := range m.databases {
		var grants []policy.Access
		for pair := range m.memberships {
			if pair[1] == userID {
				if acc, ok := m.grants[[2]int64{pair[0], dbID}]; ok {
					grants = append(grants, acc)
				}
			}
		}
		eff := policy.Effective(grants)
		if eff != policy.None {
			db.EffectiveAccess = eff
			out = append(out, db)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) groupIDsForUserLocked(userID int64) []string {
	var ids []int64
	for pair := range m.memberships {
		if pair[1] == userID {
			ids = append(ids, pair[0])
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
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

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
