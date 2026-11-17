package admin

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/policy"
)

// PostgresStore implements Store against the platform metadata database.
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

	// Acquire the shared admin-count lock before locking user rows so concurrent
	// deactivations cannot both observe another active admin and remove the last one.
	if params.IsPlatformAdmin != nil || params.IsActive != nil {
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(3110002)"); err != nil {
			return UserView{}, err
		}
	}

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

	if u.IsPlatformAdmin && u.IsActive && (!newAdmin || !newActive) {
		var hasOtherAdmin bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM users
				WHERE id <> $1 AND is_platform_admin AND is_active
			)
		`, id).Scan(&hasOtherAdmin)
		if err != nil {
			return UserView{}, err
		}
		if !hasOtherAdmin {
			return UserView{}, ErrLastAdmin
		}
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

	u.GroupIDs, err = listUserGroupIDs(ctx, tx, id)
	if err != nil {
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

	gRows, err := s.pool.Query(ctx, `
		SELECT group_id, database_id, access_level
		FROM group_database_permissions
		ORDER BY database_id ASC
	`)
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
	err := s.pool.QueryRow(ctx, `
		INSERT INTO groups (name)
		VALUES ($1)
		RETURNING id, name
	`, name).Scan(&g.ID, &g.Name)
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
	var userExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&userExists); err != nil {
		return err
	}
	if !userExists {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_groups (user_id, group_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, userID, groupID); err != nil {
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
	var dbExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM database_connections WHERE id = $1)`, databaseID).Scan(&dbExists); err != nil {
		return GrantView{}, err
	}
	if !dbExists {
		return GrantView{}, ErrNotFound
	}

	var prevLevel string
	err = tx.QueryRow(ctx, `
		SELECT access_level
		FROM group_database_permissions
		WHERE group_id = $1 AND database_id = $2
		FOR UPDATE
	`, groupID, databaseID).Scan(&prevLevel)
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
		ON CONFLICT (group_id, database_id)
		DO UPDATE SET access_level = EXCLUDED.access_level
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
	tag, err := tx.Exec(ctx, `
		DELETE FROM group_database_permissions
		WHERE group_id = $1 AND database_id = $2
	`, groupID, databaseID)
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

func listUserGroupIDs(ctx context.Context, tx pgx.Tx, userID int64) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT group_id FROM user_groups WHERE user_id = $1 ORDER BY group_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groupIDs := []string{}
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			return nil, err
		}
		groupIDs = append(groupIDs, strconv.FormatInt(gid, 10))
	}
	return groupIDs, rows.Err()
}

// lockGroup serializes group grants and membership changes, including first-time
// grants where SELECT ... FOR UPDATE on the permission row alone has no row to lock.
func lockGroup(ctx context.Context, tx pgx.Tx, id int64) error {
	var groupID int64
	err := tx.QueryRow(ctx, "SELECT id FROM groups WHERE id = $1 FOR UPDATE", id).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func changeGroupAccess(ctx context.Context, tx pgx.Tx, groupID int64, increase bool) error {
	rows, err := tx.Query(ctx, "SELECT user_id FROM user_groups WHERE group_id = $1 ORDER BY user_id", groupID)
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
