//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"terserah/backend/internal/auth"
)

func TestAdminIntegration(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	const usersPath = "/api/v1/admin/users"
	const groupsPath = "/api/v1/admin/groups"
	w := admin.request("POST", usersPath, map[string]any{"username": "api-user", "password": testPassword})
	status(t, w, 201)
	var user struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil || user.User.ID == "" {
		t.Fatal("created user ID missing")
	}
	userPath := usersPath + "/" + user.User.ID
	status(t, admin.request("POST", usersPath, map[string]string{"username": "api-user", "password": testPassword}), 409)
	status(t, admin.request("POST", usersPath, map[string]string{"username": "invalid user", "password": testPassword}), 400)
	status(t, admin.request("POST", usersPath, map[string]string{"username": "short-password", "password": "short"}), 400)
	w = admin.request("POST", groupsPath, map[string]string{"name": "api-group"})
	status(t, w, 201)
	var group struct {
		Group struct {
			ID string `json:"id"`
		} `json:"group"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &group); err != nil || group.Group.ID == "" {
		t.Fatal("created group ID missing")
	}
	groupPath := groupsPath + "/" + group.Group.ID
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "api-target"))
	other := createTarget(t, admin, targetInput(t, "TEST_SECOND_TARGET_DATABASE_URL", "api-other-target"))
	grantPath := fmt.Sprintf("%s/databases/%d", groupPath, target.ID)
	membershipPath := groupPath + "/users/" + user.User.ID
	member := a.browser()
	member.login(t, "api-user")
	status(t, member.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", target.ID), nil), 403)
	status(t, admin.request("PUT", membershipPath, nil), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 200)
	old := *member.cookies[auth.CookieName]
	status(t, admin.request("PUT", grantPath, map[string]string{"access_level": "view"}), 200)
	status(t, member.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", target.ID), nil), 200)
	if member.cookies[auth.CookieName].Value == old.Value || !member.cookies[auth.CookieName].Expires.Equal(old.Expires) {
		t.Fatal("grant increase did not rotate or changed absolute expiry")
	}
	w = member.request("GET", "/api/v1/databases", nil)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"access":"view"`) || strings.Contains(w.Body.String(), "effective_access") {
		t.Fatal("database permission payload changed")
	}
	w = admin.request("POST", groupsPath, map[string]string{"name": "api-extra-group"})
	status(t, w, 201)
	var extra struct {
		Group struct {
			ID string `json:"id"`
		} `json:"group"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &extra); err != nil || extra.Group.ID == "" {
		t.Fatal("second group missing")
	}
	extraPath := groupsPath + "/" + extra.Group.ID
	status(t, admin.request("PUT", extraPath+"/users/"+user.User.ID, nil), 200)
	status(t, admin.request("PUT", fmt.Sprintf("%s/databases/%d", extraPath, target.ID), map[string]string{"access_level": "edit"}), 200)
	w = member.request("GET", "/api/v1/databases", nil)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"access":"edit"`) {
		t.Fatal("highest grant across API-created groups was not applied")
	}
	status(t, admin.request("DELETE", extraPath+"/users/"+user.User.ID, nil), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 401)
	member.login(t, "api-user")
	w = member.request("GET", "/api/v1/databases", nil)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"access":"view"`) {
		t.Fatal("remaining group grant was lost")
	}
	status(t, member.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", other.ID), nil), 403)
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{"GET", usersPath, nil}, {"POST", usersPath, map[string]string{"username": "intruder", "password": testPassword}},
		{"PATCH", userPath, map[string]bool{"is_platform_admin": true}},
		{"GET", groupsPath, nil}, {"POST", groupsPath, map[string]string{"name": "intruder"}},
		{"PUT", membershipPath, nil}, {"DELETE", membershipPath, nil},
		{"PUT", grantPath, map[string]string{"access_level": "edit"}}, {"DELETE", grantPath, nil},
	} {
		status(t, member.request(test.method, test.path, test.body), 403)
	}
	status(t, admin.request("PUT", grantPath, map[string]string{"access_level": "owner"}), 400)
	csrf := admin.csrf
	admin.csrf = ""
	status(t, admin.request("DELETE", grantPath, nil), 403)
	admin.csrf = csrf
	status(t, member.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", target.ID), nil), 200)
	old = *member.cookies[auth.CookieName]
	status(t, admin.request("PUT", grantPath, map[string]string{"access_level": "edit"}), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 200)
	if old.Value == member.cookies[auth.CookieName].Value {
		t.Fatal("edit increase did not rotate")
	}
	grace := a.browser()
	grace.cookies[auth.CookieName] = &old
	status(t, grace.request("GET", "/api/v1/auth/me", nil), 200)
	status(t, admin.request("PUT", grantPath, map[string]string{"access_level": "view"}), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 401)
	grace.cookies[auth.CookieName] = &old
	status(t, grace.request("GET", "/api/v1/auth/me", nil), 401)
	member.login(t, "api-user")
	status(t, admin.request("DELETE", grantPath, nil), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 401)
	status(t, admin.request("PUT", grantPath, map[string]string{"access_level": "view"}), 200)
	member.login(t, "api-user")
	status(t, admin.request("DELETE", membershipPath, nil), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 401)
	member.login(t, "api-user")
	status(t, admin.request("PATCH", userPath, map[string]bool{"is_platform_admin": true}), 200)
	status(t, member.request("GET", usersPath, nil), 200)
	status(t, admin.request("PATCH", userPath, map[string]bool{"is_platform_admin": false}), 200)
	status(t, member.request("GET", usersPath, nil), 401)
	member.login(t, "api-user")
	status(t, admin.request("PATCH", userPath, map[string]string{"password": "new-test-password-123"}), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 401)
	member.refreshCSRF(t)
	status(t, member.request("POST", "/api/v1/auth/login", map[string]string{"username": "api-user", "password": testPassword}), 401)
	status(t, member.request("POST", "/api/v1/auth/login", map[string]string{"username": "api-user", "password": "new-test-password-123"}), 200)
	member.refreshCSRF(t)
	status(t, admin.request("PATCH", userPath, map[string]bool{"is_active": false}), 200)
	status(t, member.request("GET", "/api/v1/auth/me", nil), 401)
	status(t, admin.request("PATCH", usersPath+"/999999", map[string]bool{"is_active": false}), 404)
	status(t, admin.request("PUT", groupsPath+"/999999/users/"+user.User.ID, nil), 404)
	status(t, admin.request("GET", groupsPath, nil), 200)
	w = admin.request("GET", usersPath, nil)
	status(t, w, 200)
	if strings.Contains(w.Body.String(), "password") {
		t.Fatal("user response leaked password/hash")
	}
	var count int
	if err := a.db.QueryRow(ctx, "SELECT count(*) FROM users WHERE username='intruder'").Scan(&count); err != nil || count != 0 {
		t.Fatal("forbidden admin request changed users")
	}
	status(t, admin.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", target.ID), nil), 403)
}

func TestLegacyLoginUpgrade(t *testing.T) {
	a := newM1App(t)
	const legacy = "$pbkdf2-sha256$i=210000$AAECAwQFBgcICQoLDA0ODw$2QNDrl+R+uQV/xJCW4gZazJPobMhzqlqdv5qX/CgJFY"
	if _, err := a.db.Exec(context.Background(), "INSERT INTO users(username,password_hash) VALUES('legacy',$1)", legacy); err != nil {
		t.Fatal(err)
	}
	b := a.browser()
	b.refreshCSRF(t)
	status(t, b.request("POST", "/api/v1/auth/login", map[string]string{"username": "legacy", "password": "old-pass"}), 200)
	var upgraded string
	if err := a.db.QueryRow(context.Background(), "SELECT password_hash FROM users WHERE username='legacy'").Scan(&upgraded); err != nil || !strings.HasPrefix(upgraded, "pbkdf2-sha256$600000$") || !auth.CheckPassword(upgraded, "old-pass") {
		t.Fatal("legacy login did not upgrade hash")
	}
}
