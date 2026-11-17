package admin

import (
	"context"
	"errors"
	"time"

	"terserah/backend/internal/policy"
)

var (
	ErrNotFound  = errors.New("resource not found")
	ErrConflict  = errors.New("resource conflict")
	ErrLastAdmin = errors.New("cannot remove the last active platform admin")
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

// Store defines metadata persistence for users, groups, memberships, and grants.
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
