package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/httpx"
	"terserah/backend/internal/policy"
)

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	routes := map[string]http.HandlerFunc{
		"GET /api/v1/admin/users":                                 s.HandleListUsers,
		"POST /api/v1/admin/users":                                s.HandleCreateUser,
		"PATCH /api/v1/admin/users/{id}":                          s.HandleUpdateUser,
		"GET /api/v1/admin/groups":                                s.HandleListGroups,
		"POST /api/v1/admin/groups":                               s.HandleCreateGroup,
		"PUT /api/v1/admin/groups/{id}/leader":                    s.HandleSetGroupLeader,
		"PUT /api/v1/admin/groups/{id}/leader/{userID}":           s.HandleSetGroupLeader,
		"PUT /api/v1/admin/groups/{id}/users/{userID}":            s.HandleAddUserToGroup,
		"DELETE /api/v1/admin/groups/{id}/users/{userID}":         s.HandleRemoveUserFromGroup,
		"PUT /api/v1/admin/groups/{id}/databases/{databaseID}":    s.HandleSetGrant,
		"DELETE /api/v1/admin/groups/{id}/databases/{databaseID}": s.HandleRevokeGrant,
	}
	for route, handler := range routes {
		mux.HandleFunc(route, sessions.Require(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.User(r.Context())
			if !ok || !p.IsPlatformAdmin {
				httpx.Error(w, http.StatusForbidden, "forbidden", "Platform Admin access required")
				return
			}
			handler(w, r)
		}))
	}
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
	user, err := s.store.CreateUser(r.Context(), username, hash, req.IsPlatformAdmin, s.now())
	if err != nil {
		if errors.Is(err, ErrConflict) {
			httpx.Error(w, http.StatusConflict, "conflict", "Username already exists")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot create user")
		return
	}
	actor, _ := auth.User(r.Context())
	slog.InfoContext(r.Context(), "admin_user_created",
		"event", "admin_user_created",
		"actor_id", actor.ID,
		"target_user_id", user.ID,
		"target_username", user.Username,
		"is_platform_admin", user.IsPlatformAdmin,
		"status", "success",
	)
	httpx.JSON(w, http.StatusCreated, map[string]any{"user": user})
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
	params := UpdateUserParams{
		IsPlatformAdmin: req.IsPlatformAdmin,
		IsActive:        req.IsActive,
	}
	if req.Password != nil {
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		params.PasswordHash = &hash
	}
	user, err := s.store.UpdateUser(r.Context(), id, params, s.now())
	switch {
	case errors.Is(err, ErrLastAdmin):
		httpx.Error(w, http.StatusConflict, "last_active_admin", "At least one active Platform Admin must remain")
	case errors.Is(err, ErrLeaderActive):
		httpx.Error(w, http.StatusConflict, "group_leader_active", "Cannot deactivate user while assigned as a group leader; replace the group leader first")
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "User not found")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot update user")
	default:
		actor, _ := auth.User(r.Context())
		slog.InfoContext(r.Context(), "admin_user_updated",
			"event", "admin_user_updated",
			"actor_id", actor.ID,
			"target_user_id", user.ID,
			"is_active", user.IsActive,
			"is_platform_admin", user.IsPlatformAdmin,
			"status", "success",
		)
		httpx.JSON(w, http.StatusOK, map[string]any{"user": user})
	}
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
	if len(name) < 1 || len(name) > 100 || strings.ContainsRune(name, 0) {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Group name must be 1..100 bytes without NUL")
		return
	}
	group, err := s.store.CreateGroup(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			httpx.Error(w, http.StatusConflict, "conflict", "Group name already exists")
			return
		}
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot create group")
		return
	}
	actor, _ := auth.User(r.Context())
	slog.InfoContext(r.Context(), "admin_group_created",
		"event", "admin_group_created",
		"actor_id", actor.ID,
		"group_id", group.ID,
		"group_name", group.Name,
		"status", "success",
	)
	httpx.JSON(w, http.StatusCreated, map[string]any{"group": group})
}

func (s *Service) HandleSetGroupLeader(w http.ResponseWriter, r *http.Request) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return
	}
	var userID int64
	if rawUser := r.PathValue("userID"); rawUser != "" {
		userID, err = parseID(rawUser)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
			return
		}
	} else {
		var req struct {
			UserID string `json:"user_id"`
		}
		if !httpx.Decode(w, r, &req) {
			return
		}
		userID, err = parseID(req.UserID)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid user ID")
			return
		}
	}
	group, err := s.store.SetGroupLeader(r.Context(), groupID, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "Group or user not found")
	case errors.Is(err, ErrNotGroupMember):
		httpx.Error(w, http.StatusConflict, "leader_not_member", "Leader must be an active member of the group")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot set group leader")
	default:
		actor, _ := auth.User(r.Context())
		slog.InfoContext(r.Context(), "admin_group_leader_set",
			"event", "admin_group_leader_set",
			"actor_id", actor.ID,
			"group_id", group.ID,
			"leader_user_id", userID,
			"status", "success",
		)
		httpx.JSON(w, http.StatusOK, map[string]any{"group": group})
	}
}

func (s *Service) HandleAddUserToGroup(w http.ResponseWriter, r *http.Request) {
	groupID, userID, ok := parseGroupAndTargetID(w, r, "userID", "Invalid user ID")
	if !ok {
		return
	}
	err := s.store.AddUserToGroup(r.Context(), groupID, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "Group or user not found")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot add user to group")
	default:
		actor, _ := auth.User(r.Context())
		slog.InfoContext(r.Context(), "admin_group_member_added",
			"event", "admin_group_member_added",
			"actor_id", actor.ID,
			"group_id", groupID,
			"user_id", userID,
			"status", "success",
		)
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Service) HandleRemoveUserFromGroup(w http.ResponseWriter, r *http.Request) {
	groupID, userID, ok := parseGroupAndTargetID(w, r, "userID", "Invalid user ID")
	if !ok {
		return
	}
	err := s.store.RemoveUserFromGroup(r.Context(), groupID, userID, s.now())
	switch {
	case errors.Is(err, ErrLeaderActive):
		httpx.Error(w, http.StatusConflict, "group_leader_active", "Cannot remove the current group leader from the group; assign another leader first")
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "Group membership not found")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot remove user from group")
	default:
		actor, _ := auth.User(r.Context())
		slog.InfoContext(r.Context(), "admin_group_member_removed",
			"event", "admin_group_member_removed",
			"actor_id", actor.ID,
			"group_id", groupID,
			"user_id", userID,
			"status", "success",
		)
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Service) HandleSetGrant(w http.ResponseWriter, r *http.Request) {
	groupID, databaseID, ok := parseGroupAndTargetID(w, r, "databaseID", "Invalid database ID")
	if !ok {
		return
	}
	var req struct {
		AccessLevel string `json:"access_level"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	access, valid := policy.ParseAccess(req.AccessLevel)
	if !valid {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "access_level must be 'view' or 'edit'")
		return
	}
	grant, err := s.store.SetGroupDatabaseGrant(r.Context(), groupID, databaseID, access, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "Group or database not found")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot set database grant")
	default:
		actor, _ := auth.User(r.Context())
		slog.InfoContext(r.Context(), "admin_grant_updated",
			"event", "admin_grant_updated",
			"actor_id", actor.ID,
			"group_id", groupID,
			"database_id", databaseID,
			"access_level", access,
			"status", "success",
		)
		httpx.JSON(w, http.StatusOK, map[string]any{"grant": grant})
	}
}

func (s *Service) HandleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	groupID, databaseID, ok := parseGroupAndTargetID(w, r, "databaseID", "Invalid database ID")
	if !ok {
		return
	}
	err := s.store.RevokeGroupDatabaseGrant(r.Context(), groupID, databaseID, s.now())
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "Grant not found")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Cannot revoke database grant")
	default:
		actor, _ := auth.User(r.Context())
		slog.InfoContext(r.Context(), "admin_grant_revoked",
			"event", "admin_grant_revoked",
			"actor_id", actor.ID,
			"group_id", groupID,
			"database_id", databaseID,
			"status", "success",
		)
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func parseGroupAndTargetID(w http.ResponseWriter, r *http.Request, param, errMsg string) (int64, int64, bool) {
	groupID, err := parseID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid group ID")
		return 0, 0, false
	}
	targetID, err := parseID(r.PathValue(param))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", errMsg)
		return 0, 0, false
	}
	return groupID, targetID, true
}

func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid positive integer id")
	}
	return id, nil
}
