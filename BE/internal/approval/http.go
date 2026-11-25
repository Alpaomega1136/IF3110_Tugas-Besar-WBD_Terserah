package approval

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"terserah/backend/internal/auth"
	"terserah/backend/internal/httpx"
	"terserah/backend/internal/telemetry"
)

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	mux.HandleFunc("GET /api/v1/reviews", sessions.Require(s.HandleListAssignedReviews))
	mux.HandleFunc("GET /api/v1/databases/{id}/reviews", sessions.Require(s.HandleListDatabaseReviews))
	mux.HandleFunc("POST /api/v1/databases/{id}/reviews", sessions.Require(s.HandleSubmitReview))
	mux.HandleFunc("GET /api/v1/databases/{id}/reviews/{reviewID}", sessions.Require(s.HandleGetReview))
	mux.HandleFunc("POST /api/v1/databases/{id}/reviews/{reviewID}/approve", sessions.Require(s.HandleApproveReview))
	mux.HandleFunc("POST /api/v1/databases/{id}/reviews/{reviewID}/reject", sessions.Require(s.HandleRejectReview))
}

func (s *Service) HandleListAssignedReviews(w http.ResponseWriter, r *http.Request) {
	s.listReviews(w, r, 0, 100)
}

func (s *Service) HandleListDatabaseReviews(w http.ResponseWriter, r *http.Request) {
	dbID, err := parsePositiveID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return
	}
	s.listReviews(w, r, dbID, 50)
}

func (s *Service) listReviews(w http.ResponseWriter, r *http.Request, dbID int64, defaultLimit int) {
	p, _ := auth.User(r.Context())
	query := r.URL.Query()
	options := ReviewListOptions{Limit: defaultLimit, Status: query.Get("status")}
	var err error
	if query.Has("limit") {
		options.Limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || options.Limit < 1 || options.Limit > 100 {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid limit")
			return
		}
	}
	if query.Has("before") {
		options.BeforeID, err = parsePositiveID(query.Get("before"))
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid before cursor")
			return
		}
	}
	if query.Has("only_actionable") {
		options.OnlyActionable, err = strconv.ParseBool(query.Get("only_actionable"))
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid only_actionable filter")
			return
		}
	}
	switch options.Status {
	case "", StatusPendingReview, StatusApproved, StatusRejected, StatusQueued, StatusCancelled:
	default:
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid review status")
		return
	}
	page, err := s.store.ListReviews(r.Context(), p.ID, dbID, options)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (s *Service) HandleSubmitReview(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.User(r.Context())
	dbID, err := parsePositiveID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return
	}
	var req struct {
		DraftID           int64   `json:"draft_id,string"`
		Revision          int64   `json:"revision,string"`
		ActionType        string  `json:"action_type"`
		TargetExecutionID *string `json:"target_execution_id"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.ActionType == "" {
		req.ActionType = "up"
	}
	if req.DraftID < 1 || req.Revision < 1 || !ValidActionType(req.ActionType) {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid draft_id, revision, or action_type")
		return
	}
	review, err := s.store.SubmitReview(r.Context(), p.ID, dbID, SubmitInput{
		DraftID:           req.DraftID,
		Revision:          req.Revision,
		ActionType:        req.ActionType,
		TargetExecutionID: req.TargetExecutionID,
	}, s.now())
	if err != nil {
		telemetry.WarnEvent(r.Context(), "migration_review_submit_failed",
			"actor_id", p.ID,
			"database_id", dbID,
			"draft_id", req.DraftID,
			"draft_revision", req.Revision,
			"action_type", req.ActionType,
			"status", "failed",
			"error_code", telemetry.SQLErrorCode(err),
		)
		respondError(w, err)
		return
	}
	telemetry.Event(r.Context(), "migration_review_submitted",
		"actor_id", p.ID,
		"database_id", dbID,
		"review_id", review.ID,
		"draft_id", review.DraftID,
		"draft_revision", review.DraftRevision,
		"action_type", review.ActionType,
		"required_groups", len(review.Approvals),
		"status", review.Status,
	)
	httpx.JSON(w, http.StatusCreated, map[string]any{"review": review})
}

func (s *Service) HandleGetReview(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.User(r.Context())
	dbID, reviewID, ok := parseDBAndReviewID(w, r)
	if !ok {
		return
	}
	review, err := s.store.GetReview(r.Context(), p.ID, dbID, reviewID)
	if err != nil {
		respondError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"review": review})
}

func (s *Service) HandleApproveReview(w http.ResponseWriter, r *http.Request) {
	s.handleDecision(w, r, true)
}

func (s *Service) HandleRejectReview(w http.ResponseWriter, r *http.Request) {
	s.handleDecision(w, r, false)
}

func (s *Service) handleDecision(w http.ResponseWriter, r *http.Request, approve bool) {
	p, _ := auth.User(r.Context())
	dbID, reviewID, ok := parseDBAndReviewID(w, r)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &req) {
		return
	}
	note, err := ValidateNote(req.Note)
	if err != nil || (!approve && note == "") {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Rejection reason is required and notes must be at most 500 bytes without NUL")
		return
	}
	review, err := s.store.DecideReview(r.Context(), p.ID, dbID, reviewID, approve, note, s.now())
	if err != nil {
		telemetry.WarnEvent(r.Context(), "migration_review_decision_failed",
			"actor_id", p.ID,
			"database_id", dbID,
			"review_id", reviewID,
			"approve", approve,
			"status", "failed",
			"error_code", telemetry.SQLErrorCode(err),
		)
		respondError(w, err)
		return
	}
	telemetry.Event(r.Context(), "migration_review_decided",
		"actor_id", p.ID,
		"database_id", dbID,
		"review_id", review.ID,
		"approve", approve,
		"review_status", review.Status,
		"status", "success",
	)
	httpx.JSON(w, http.StatusOK, map[string]any{"review": review})
}

func parseDBAndReviewID(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	dbID, err := parsePositiveID(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return 0, 0, false
	}
	reviewID, err := parsePositiveID(r.PathValue("reviewID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid review ID")
		return 0, 0, false
	}
	return dbID, reviewID, true
}

func parsePositiveID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidInput
	}
	return id, nil
}

func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid review request")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden", "Database access denied")
	case errors.Is(err, ErrNotApprover):
		httpx.Error(w, http.StatusForbidden, "not_group_leader", "Only the assigned leader of a group with access to this database can approve or reject")
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "Review or migration draft not found")
	case errors.Is(err, ErrLeaderMissing):
		httpx.Error(w, http.StatusConflict, "group_leader_required", "Every group with view or edit access to this database must have an active group leader assigned")
	case errors.Is(err, ErrRevisionConflict):
		httpx.Error(w, http.StatusConflict, "review_conflict", "Draft revision changed or a pending/approved review already exists for this revision")
	case errors.Is(err, ErrReviewClosed):
		httpx.Error(w, http.StatusConflict, "review_closed", "Review is no longer pending")
	case errors.Is(err, ErrNotApproved):
		httpx.Error(w, http.StatusConflict, "approval_incomplete", "All group leaders with access to this database must approve before execution")
	default:
		httpx.Error(w, http.StatusServiceUnavailable, "service_unavailable", "Review service is unavailable")
	}
}
