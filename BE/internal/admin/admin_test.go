package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/policy"
)

type stubAdminStore struct {
	setLeaderErr  error
	removeUserErr error
	updateUserErr error
}

func (s stubAdminStore) ListUsers(context.Context) ([]UserView, error) { return nil, nil }
func (s stubAdminStore) CreateUser(context.Context, string, string, bool, time.Time) (UserView, error) {
	return UserView{}, nil
}
func (s stubAdminStore) UpdateUser(context.Context, int64, UpdateUserParams, time.Time) (UserView, error) {
	return UserView{ID: 2, Username: "u"}, s.updateUserErr
}
func (s stubAdminStore) ListGroups(context.Context) ([]GroupView, error) { return nil, nil }
func (s stubAdminStore) CreateGroup(context.Context, string) (GroupView, error) {
	return GroupView{}, nil
}
func (s stubAdminStore) SetGroupLeader(_ context.Context, groupID, userID int64) (GroupView, error) {
	if s.setLeaderErr != nil {
		return GroupView{}, s.setLeaderErr
	}
	name := "leader"
	return GroupView{ID: groupID, Name: "Core", LeaderUserID: &userID, LeaderUsername: &name}, nil
}
func (s stubAdminStore) AddUserToGroup(context.Context, int64, int64) error { return nil }
func (s stubAdminStore) RemoveUserFromGroup(context.Context, int64, int64, time.Time) error {
	return s.removeUserErr
}
func (s stubAdminStore) SetGroupDatabaseGrant(context.Context, int64, int64, policy.Access, time.Time) (GrantView, error) {
	return GrantView{}, nil
}
func (s stubAdminStore) RevokeGroupDatabaseGrant(context.Context, int64, int64, time.Time) error {
	return nil
}

func TestCreateGroupRejectsNULBeforeStore(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/admin/groups", strings.NewReader(`{"name":"bad\u0000group"}`))
	NewService(nil, nil).HandleCreateGroup(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_input") {
		t.Fatalf("invalid group status=%d", w.Code)
	}
}

func TestAdminInputValidation(t *testing.T) {
	if name, err := auth.ValidateUsername(" user-name "); err != nil || name != "user-name" {
		t.Fatal("valid username rejected")
	}
	for _, name := range []string{"", "a b", "user\x00", "x\ny"} {
		if _, err := auth.ValidateUsername(name); err == nil {
			t.Fatal("invalid username accepted")
		}
	}
	for _, id := range []string{"0", "-1", "abc", "9223372036854775808"} {
		if _, err := parseID(id); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
}

func TestSetGroupLeaderAndLeaderProtection(t *testing.T) {
	svc := NewService(stubAdminStore{}, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/api/v1/admin/groups/3/leader", strings.NewReader(`{"user_id":"7"}`))
	r.SetPathValue("id", "3")
	svc.HandleSetGroupLeader(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"leader_user_id":"7"`) {
		t.Fatalf("expected 200 with leader_user_id, got %d %s", w.Code, w.Body.String())
	}

	nonMemberSvc := NewService(stubAdminStore{setLeaderErr: ErrNotGroupMember}, nil)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("PUT", "/api/v1/admin/groups/3/leader", strings.NewReader(`{"user_id":"7"}`))
	r.SetPathValue("id", "3")
	nonMemberSvc.HandleSetGroupLeader(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "leader_not_member") {
		t.Fatalf("expected 409 leader_not_member, got %d %s", w.Code, w.Body.String())
	}

	protectedSvc := NewService(stubAdminStore{removeUserErr: ErrLeaderActive, updateUserErr: ErrLeaderActive}, nil)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("DELETE", "/api/v1/admin/groups/3/users/7", nil)
	r.SetPathValue("id", "3")
	r.SetPathValue("userID", "7")
	protectedSvc.HandleRemoveUserFromGroup(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "group_leader_active") {
		t.Fatalf("expected 409 group_leader_active on remove member, got %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("PATCH", "/api/v1/admin/users/7", strings.NewReader(`{"is_active":false}`))
	r.SetPathValue("id", "7")
	protectedSvc.HandleUpdateUser(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "group_leader_active") {
		t.Fatalf("expected 409 group_leader_active on deactivate user, got %d %s", w.Code, w.Body.String())
	}
}
