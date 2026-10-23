package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/admin"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/migration"
	"terserah/backend/internal/policy"
)

type Pinger interface {
	Ping(context.Context) error
}

type Options struct {
	AuthConfig     auth.Config
	AuthStore      auth.Store
	AdminStore     admin.Store
	MigrationStore migration.Store
	GrantLoader    policy.GrantFunc
}

func New(db Pinger) http.Handler {
	return NewWithOptions(db, Options{})
}

func NewWithOptions(db Pinger, opts Options) http.Handler {
	if pool, ok := db.(*pgxpool.Pool); ok && pool != nil {
		if opts.AuthStore == nil {
			opts.AuthStore = auth.NewPostgresStore(pool)
		}
		if opts.AdminStore == nil {
			opts.AdminStore = admin.NewPostgresStore(pool)
		}
		if opts.MigrationStore == nil {
			opts.MigrationStore = migration.NewPostgresStore(pool)
		}
		if opts.GrantLoader == nil {
			opts.GrantLoader = admin.PostgresGrantLoader(pool)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "database_unavailable", "message": "Database is unavailable"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if opts.AuthStore != nil {
		authSvc := auth.NewService(opts.AuthStore, opts.AuthConfig)
		requireAuth := func(h http.HandlerFunc) http.Handler {
			return authSvc.RequireAuth(h)
		}
		requireAdmin := func(h http.HandlerFunc) http.Handler {
			return authSvc.RequireAuth(auth.RequireAdmin(h))
		}

		mux.HandleFunc("POST /api/v1/auth/login", authSvc.HandleLogin)
		mux.Handle("POST /api/v1/auth/logout", requireAuth(authSvc.HandleLogout))
		mux.Handle("GET /api/v1/auth/me", requireAuth(authSvc.HandleMe))
		mux.Handle("GET /api/v1/auth/csrf", requireAuth(authSvc.HandleCSRF))

		if opts.AdminStore != nil {
			adminSvc := admin.NewService(opts.AdminStore, opts.AuthConfig.Now)
			mux.Handle("GET /api/v1/admin/users", requireAdmin(adminSvc.HandleListUsers))
			mux.Handle("POST /api/v1/admin/users", requireAdmin(adminSvc.HandleCreateUser))
			mux.Handle("PATCH /api/v1/admin/users/{id}", requireAdmin(adminSvc.HandleUpdateUser))
			mux.Handle("GET /api/v1/admin/groups", requireAdmin(adminSvc.HandleListGroups))
			mux.Handle("POST /api/v1/admin/groups", requireAdmin(adminSvc.HandleCreateGroup))
			mux.Handle("PUT /api/v1/admin/groups/{id}/users/{userID}", requireAdmin(adminSvc.HandleAddUserToGroup))
			mux.Handle("DELETE /api/v1/admin/groups/{id}/users/{userID}", requireAdmin(adminSvc.HandleRemoveUserFromGroup))
			mux.Handle("PUT /api/v1/admin/groups/{id}/databases/{databaseID}", requireAdmin(adminSvc.HandleSetGrant))
			mux.Handle("DELETE /api/v1/admin/groups/{id}/databases/{databaseID}", requireAdmin(adminSvc.HandleRevokeGrant))
			mux.Handle("GET /api/v1/databases", requireAuth(adminSvc.HandleListAccessibleDatabases))
		}

		if opts.MigrationStore != nil && opts.GrantLoader != nil {
			migSvc := migration.NewService(opts.MigrationStore, opts.GrantLoader, opts.AuthConfig.Now)
			mux.Handle("GET /api/v1/databases/{id}/drafts", requireAuth(migSvc.HandleListDrafts))
			mux.Handle("POST /api/v1/databases/{id}/drafts", requireAuth(migSvc.HandleCreateDraft))
			mux.Handle("GET /api/v1/databases/{id}/drafts/{draftID}", requireAuth(migSvc.HandleGetDraft))
			mux.Handle("PATCH /api/v1/databases/{id}/drafts/{draftID}", requireAuth(migSvc.HandleUpdateDraft))
			mux.Handle("GET /api/v1/databases/{id}/migrations", requireAuth(migSvc.HandleListHistory))
			mux.Handle("GET /api/v1/databases/{id}/migrations/{version}", requireAuth(migSvc.HandleGetHistoryByVersion))
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found", "message": "Endpoint not found"}})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
