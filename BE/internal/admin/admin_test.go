package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/policy"
)

func TestAdminUsersGroupsGrantsAndPrivilegeSessionEffects(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	authStore := auth.NewMemoryStore()
	authSvc := auth.NewService(authStore, auth.Config{Now: clock})
	adminStore := NewMemoryStore(authStore)
	adminSvc := NewService(adminStore, clock)

	adminStore.PutDatabase(AccessibleDatabase{ID: 1, Name: "db-one", Host: "localhost", Port: 5432, DatabaseName: "one", SSLMode: "disable", CreatedAt: now})
	adminStore.PutDatabase(AccessibleDatabase{ID: 2, Name: "db-two", Host: "localhost", Port: 5432, DatabaseName: "two", SSLMode: "disable", CreatedAt: now})

	// Bootstrap akun Platform Admin pertama
	adminHash, _ := auth.HashPassword("adminpass123")
	if _, err := adminStore.CreateUser(context.Background(), "rootadmin", adminHash, true, now); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", authSvc.HandleLogin)
	mux.Handle("GET /api/v1/auth/csrf", authSvc.RequireAuth(http.HandlerFunc(authSvc.HandleCSRF)))
	mux.Handle("GET /api/v1/auth/me", authSvc.RequireAuth(http.HandlerFunc(authSvc.HandleMe)))
	mux.Handle("GET /api/v1/databases", authSvc.RequireAuth(http.HandlerFunc(adminSvc.HandleListAccessibleDatabases)))

	adminWrap := func(h http.HandlerFunc) http.Handler {
		return authSvc.RequireAuth(auth.RequireAdmin(h))
	}
	mux.Handle("GET /api/v1/admin/users", adminWrap(adminSvc.HandleListUsers))
	mux.Handle("POST /api/v1/admin/users", adminWrap(adminSvc.HandleCreateUser))
	mux.Handle("PATCH /api/v1/admin/users/{id}", adminWrap(adminSvc.HandleUpdateUser))
	mux.Handle("GET /api/v1/admin/groups", adminWrap(adminSvc.HandleListGroups))
	mux.Handle("POST /api/v1/admin/groups", adminWrap(adminSvc.HandleCreateGroup))
	mux.Handle("PUT /api/v1/admin/groups/{id}/users/{userID}", adminWrap(adminSvc.HandleAddUserToGroup))
	mux.Handle("DELETE /api/v1/admin/groups/{id}/users/{userID}", adminWrap(adminSvc.HandleRemoveUserFromGroup))
	mux.Handle("PUT /api/v1/admin/groups/{id}/databases/{databaseID}", adminWrap(adminSvc.HandleSetGrant))
	mux.Handle("DELETE /api/v1/admin/groups/{id}/databases/{databaseID}", adminWrap(adminSvc.HandleRevokeGrant))

	loginAndCSRF := func(username, password string) (*http.Cookie, string) {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`)))
		if rr.Code != http.StatusOK {
			t.Fatalf("login %s failed: %d %s", username, rr.Code, rr.Body.String())
		}
		ck := rr.Result().Cookies()[0]

		rr = httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/auth/csrf", nil)
		req.AddCookie(ck)
		mux.ServeHTTP(rr, req)
		var out struct {
			CSRFToken string `json:"csrf_token"`
		}
		_ = json.NewDecoder(rr.Body).Decode(&out)
		return ck, out.CSRFToken
	}

	adminCookie, adminCSRF := loginAndCSRF("rootadmin", "adminpass123")

	// Admin platform tidak otomatis memiliki izin akses database
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/databases", nil)
	req.AddCookie(adminCookie)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"databases":[]`) {
		t.Fatalf("expected empty accessible databases for ungranted admin, got %d %s", rr.Code, rr.Body.String())
	}

	// Buat user biasa alice
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/admin/users", strings.NewReader(`{"username":"alice","password":"alicepassword123","is_platform_admin":false}`))
	req.AddCookie(adminCookie)
	req.Header.Set(auth.CSRFHeader, adminCSRF)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create user failed: %d %s", rr.Code, rr.Body.String())
	}

	aliceCookie, aliceCSRF := loginAndCSRF("alice", "alicepassword123")

	// Endpoint admin ditolak (403) untuk user non-admin
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/admin/users", nil)
	req.AddCookie(aliceCookie)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin allowed to list users: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/admin/groups", strings.NewReader(`{"name":"hackers"}`))
	req.AddCookie(aliceCookie)
	req.Header.Set(auth.CSRFHeader, aliceCSRF)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin allowed to create group: %d", rr.Code)
	}

	// Buat dua group: readers dan editors
	for _, gname := range []string{"readers", "editors"} {
		rr = httptest.NewRecorder()
		req = httptest.NewRequest("POST", "/api/v1/admin/groups", strings.NewReader(`{"name":"`+gname+`"}`))
		req.AddCookie(adminCookie)
		req.Header.Set(auth.CSRFHeader, adminCSRF)
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create group %s failed: %d %s", gname, rr.Code, rr.Body.String())
		}
	}

	// Pasang izin group ke database
	for _, tc := range []struct {
		gid, dbid, level string
	}{
		{"1", "1", "view"},
		{"1", "2", "view"},
		{"2", "1", "edit"},
	} {
		rr = httptest.NewRecorder()
		req = httptest.NewRequest("PUT", "/api/v1/admin/groups/"+tc.gid+"/databases/"+tc.dbid, strings.NewReader(`{"access_level":"`+tc.level+`"}`))
		req.AddCookie(adminCookie)
		req.Header.Set(auth.CSRFHeader, adminCSRF)
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("set grant failed: %d %s", rr.Code, rr.Body.String())
		}
	}

	// Peningkatan privilege menandai rotasi session
	for _, gid := range []string{"1", "2"} {
		rr = httptest.NewRecorder()
		req = httptest.NewRequest("PUT", "/api/v1/admin/groups/"+gid+"/users/2", nil)
		req.AddCookie(adminCookie)
		req.Header.Set(auth.CSRFHeader, adminCSRF)
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("add user to group %s failed: %d", gid, rr.Code)
		}
	}

	// Validasi rotasi session dan kalkulasi izin tertinggi (edit > view)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/databases", nil)
	req.AddCookie(aliceCookie)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("alice GET /databases failed: %d %s", rr.Code, rr.Body.String())
	}
	if len(rr.Result().Cookies()) != 1 || rr.Result().Cookies()[0].Value == aliceCookie.Value {
		t.Fatal("expected alice session cookie to rotate after group membership elevation")
	}
	aliceCookie = rr.Result().Cookies()[0]

	var dbResp struct {
		Databases []AccessibleDatabase `json:"databases"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&dbResp); err != nil {
		t.Fatal(err)
	}
	if len(dbResp.Databases) != 2 || dbResp.Databases[0].EffectiveAccess != policy.Edit || dbResp.Databases[1].EffectiveAccess != policy.View {
		t.Fatalf("unexpected effective databases: %+v", dbResp.Databases)
	}

	// Penurunan hak akses mencabut session aktif
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/api/v1/admin/groups/2/databases/1", strings.NewReader(`{"access_level":"view"}`))
	req.AddCookie(adminCookie)
	req.Header.Set(auth.CSRFHeader, adminCSRF)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("downgrade grant failed: %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(aliceCookie)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected alice session revoked after grant downgrade, got %d", rr.Code)
	}
}
