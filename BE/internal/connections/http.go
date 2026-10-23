package connections

import (
	"errors"
	"net/http"
	"strconv"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/httpx"
)

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	mux.HandleFunc("GET /api/v1/admin/databases", sessions.Require(s.listAdmin))
	mux.HandleFunc("POST /api/v1/admin/databases", sessions.Require(s.create))
	mux.HandleFunc("PATCH /api/v1/admin/databases/{id}", sessions.Require(s.update))
	mux.HandleFunc("DELETE /api/v1/admin/databases/{id}", sessions.Require(s.remove))
	mux.HandleFunc("POST /api/v1/admin/databases/{id}/test", sessions.Require(s.test))
	mux.HandleFunc("GET /api/v1/databases", sessions.Require(s.listAccessible))
}

func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		httpx.Error(w, 401, "unauthenticated", "Login required")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, 403, "forbidden", "Access denied")
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, 404, "not_found", "Database not found")
	case errors.Is(err, ErrInvalid):
		httpx.Error(w, 400, "invalid_input", "Invalid connection configuration")
	case errors.Is(err, ErrConflict):
		httpx.Error(w, 409, "connection_conflict", "Target already registered, target identity changed, or connection has dependent records")
	case errors.Is(err, ErrMetadataTarget):
		httpx.Error(w, 409, "metadata_target_forbidden", "Metadata database cannot be used as a target")
	case errors.Is(err, ErrTargetUnavailable):
		httpx.Error(w, 503, "target_unavailable", "Cannot connect to target database")
	default:
		httpx.Error(w, 503, "database_unavailable", "Connection service is unavailable")
	}
}

func requestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, 400, "invalid_input", "Invalid database ID")
		return 0, false
	}
	return id, true
}

func (s *Service) listAdmin(w http.ResponseWriter, r *http.Request)      { s.list(w, r, true) }
func (s *Service) listAccessible(w http.ResponseWriter, r *http.Request) { s.list(w, r, false) }
func (s *Service) list(w http.ResponseWriter, r *http.Request, admin bool) {
	p, _ := auth.User(r.Context())
	items, err := s.List(r.Context(), p, admin)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]any{"databases": items})
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.User(r.Context())
	if err := requireAdmin(p); err != nil {
		respondError(w, err)
		return
	}
	var input Input
	if !httpx.Decode(w, r, &input) {
		return
	}
	c, err := s.Create(r.Context(), p, input)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, 201, c)
}

func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.User(r.Context())
	if err := requireAdmin(p); err != nil {
		respondError(w, err)
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	var patch Patch
	if !httpx.Decode(w, r, &patch) {
		return
	}
	c, err := s.Update(r.Context(), p, id, patch)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, 200, c)
}

func (s *Service) remove(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.User(r.Context())
	if err := requireAdmin(p); err != nil {
		respondError(w, err)
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	if err := s.Delete(r.Context(), p, id); err != nil {
		respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) test(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.User(r.Context())
	if err := requireAdmin(p); err != nil {
		respondError(w, err)
		return
	}
	id, ok := requestID(w, r)
	if !ok {
		return
	}
	if err := s.Test(r.Context(), p, id); err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]string{"status": "ok"})
}
