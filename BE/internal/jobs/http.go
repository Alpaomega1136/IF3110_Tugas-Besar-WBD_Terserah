package jobs

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/httpx"
)

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	base := "/api/v1/databases/{id}/jobs"
	mux.HandleFunc("POST "+base, sessions.Require(s.enqueue))
	mux.HandleFunc("GET "+base, sessions.Require(s.list))
	mux.HandleFunc("GET "+base+"/{jobID}", sessions.Require(s.get))
}

func positiveID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, ErrInvalid
	}
	return id, nil
}

func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		httpx.Error(w, 401, "unauthenticated", "Login required")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, 403, "forbidden", "Database access denied")
	case errors.Is(err, ErrInvalid):
		httpx.Error(w, 400, "invalid_input", "Invalid job ID, review ID or pagination")
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, 404, "not_found", "Job or review not found")
	case errors.Is(err, approval.ErrNotApproved), errors.Is(err, approval.ErrLeaderMissing):
		httpx.Error(w, 409, "approval_incomplete", "Approval from current group leaders is required")
	case errors.Is(err, ErrConflict):
		httpx.Error(w, 409, "execution_conflict", "Review snapshot changed or this revision has already been executed")
	case errors.Is(err, ErrUnsupported):
		httpx.Error(w, 409, "unsupported_job_action", "Only approved normal up migrations can be queued at this stage")
	default:
		httpx.Error(w, 503, "jobs_unavailable", "Cannot process job request; retry submission with the same review ID")
	}
}

func (s *Service) enqueue(w http.ResponseWriter, r *http.Request) {
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	var in struct {
		ReviewID int64 `json:"review_id,string"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	p, _ := auth.User(r.Context())
	j, replayed, err := s.Enqueue(r.Context(), p, dbID, in.ReviewID, r.Header.Get("traceparent"))
	if err != nil {
		respondError(w, err)
		return
	}
	code := http.StatusAccepted
	if replayed {
		code = http.StatusOK
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/databases/%d/jobs/%d", dbID, j.ID))
	httpx.JSON(w, code, map[string]any{"job": j, "replayed": replayed})
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	limit := 50
	var before int64
	if r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			respondError(w, ErrInvalid)
			return
		}
	}
	if r.URL.Query().Has("before") {
		before, err = positiveID(r.URL.Query().Get("before"))
		if err != nil {
			respondError(w, err)
			return
		}
	}
	p, _ := auth.User(r.Context())
	items, more, next, err := s.List(r.Context(), p, dbID, before, limit)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]any{"jobs": items, "has_more": more, "next_before": next})
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	dbID, err := positiveID(r.PathValue("id"))
	if err != nil {
		respondError(w, err)
		return
	}
	jobID, err := positiveID(r.PathValue("jobID"))
	if err != nil {
		respondError(w, err)
		return
	}
	p, _ := auth.User(r.Context())
	j, attempts, err := s.Get(r.Context(), p, dbID, jobID)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]any{"job": j, "attempts": attempts})
}
