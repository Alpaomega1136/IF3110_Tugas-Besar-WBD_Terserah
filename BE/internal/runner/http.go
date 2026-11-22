package runner

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/httpx"
	"terserah/backend/internal/policy"
)

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	mux.HandleFunc("GET /api/v1/databases/{id}/migrations", sessions.Require(s.history))
	mux.HandleFunc("GET /api/v1/databases/{id}/migrations/{version}", sessions.Require(s.history))
	mux.HandleFunc("POST /api/v1/databases/{id}/migrations/reconcile", sessions.Require(s.reconcile))

	base := "/api/v1/databases/{id}/drafts"
	mux.HandleFunc("GET "+base, sessions.Require(s.listDrafts))
	mux.HandleFunc("POST "+base, sessions.Require(s.draft))
	mux.HandleFunc("GET "+base+"/{draftID}", sessions.Require(s.draft))
	mux.HandleFunc("PATCH "+base+"/{draftID}", sessions.Require(s.draft))
	mux.HandleFunc("POST "+base+"/{draftID}/execute", sessions.Require(s.execute))
	mux.HandleFunc("POST "+base+"/{draftID}/explain", sessions.Require(s.explain))
}

func (s *Service) authorize(r *http.Request, write bool) (auth.Principal, int64, error) {
	p, _ := auth.User(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return p, 0, connections.ErrInvalid
	}
	access, err := policy.DatabaseAccess(r.Context(), s.db, p.ID, id)
	if err != nil {
		return p, id, err
	}
	if !policy.Allows(access, write) {
		return p, id, connections.ErrForbidden
	}
	return p, id, nil
}

func fail(w http.ResponseWriter, err error) {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "55P03" {
		httpx.Error(w, http.StatusConflict, "migration_busy", "A target migration is still active; retry later")
		return
	}
	switch {
	case errors.Is(err, connections.ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden", "Database access denied")
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "not_found", "Database object not found")
	case errors.Is(err, errConflict):
		httpx.Error(w, http.StatusConflict, "execution_conflict", "Reload draft or resolve the pending execution before retrying")
	case errors.Is(err, approval.ErrNotApproved), errors.Is(err, approval.ErrLeaderMissing):
		httpx.Error(w, http.StatusConflict, "approval_incomplete", "Approval from current group leaders is required before execution")
	case errors.Is(err, errDraftDefinition):
		httpx.Error(w, http.StatusBadRequest, "invalid_migration_definition", err.Error())
	case errors.Is(err, connections.ErrInvalid), errors.Is(err, errSQL):
		httpx.Error(w, http.StatusBadRequest, "invalid_input", "Invalid ID, revision, title or unsupported SQL")
	case errors.Is(err, errHistorySetup):
		httpx.Error(w, http.StatusConflict, "target_history_unprotected", "Provision target history and use a dedicated restricted executor per database")
	case errors.Is(err, errTargetIdentity):
		httpx.Error(w, http.StatusConflict, "target_identity_conflict", "Target identity changed or is already bound to another connection")
	case errors.Is(err, connections.ErrMetadataTarget):
		httpx.Error(w, http.StatusConflict, "metadata_target_forbidden", "Metadata database cannot be a target")
	default:
		httpx.Error(w, http.StatusServiceUnavailable, "migration_unavailable", "Cannot process migration; retry with the same draft revision")
	}
}

func (s *Service) listDrafts(w http.ResponseWriter, r *http.Request) {
	_, dbID, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	limit, after, err := parseCursorParams(r, "after")
	if err != nil {
		fail(w, err)
		return
	}
	items, more, next, err := s.listDraftsPage(r.Context(), dbID, after, limit)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"drafts":     items,
		"has_more":   more,
		"next_after": next,
	})
}

func (s *Service) draft(w http.ResponseWriter, r *http.Request) {
	p, dbID, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	draftID, _ := strconv.ParseInt(r.PathValue("draftID"), 10, 64)
	if r.Method != http.MethodPost && draftID < 1 {
		fail(w, connections.ErrInvalid)
		return
	}
	if r.Method == http.MethodGet {
		d, err := s.getDraft(r.Context(), dbID, draftID)
		if err != nil {
			fail(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, d)
		return
	}

	var in draftInput
	if !httpx.Decode(w, r, &in) {
		return
	}
	if r.Method == http.MethodPost {
		if in.Revision != 0 {
			fail(w, connections.ErrInvalid)
			return
		}
		d, err := s.createDraft(r.Context(), dbID, p.ID, in)
		if err != nil {
			fail(w, err)
			return
		}
		httpx.JSON(w, http.StatusCreated, d)
		return
	}

	if draftID < 1 || in.Revision < 1 {
		fail(w, connections.ErrInvalid)
		return
	}
	d, err := s.updateDraft(r.Context(), dbID, draftID, in)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (s *Service) execute(w http.ResponseWriter, r *http.Request) {
	p, dbID, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	draftID, revision, ok := parseDraftRevisionRequest(w, r)
	if !ok {
		return
	}
	// Leave time for rollback and ledger persistence inside the session's 10s budget.
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()

	result, err := s.run(ctx, p, dbID, draftID, revision)
	if err != nil {
		fail(w, err)
		return
	}
	code := http.StatusOK
	if result.Status == "unknown" || result.Status == "running" {
		code = http.StatusAccepted
	}
	httpx.JSON(w, code, result)
}

func (s *Service) explain(w http.ResponseWriter, r *http.Request) {
	p, dbID, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	draftID, revision, ok := parseDraftRevisionRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()

	results, err := s.explainDraft(ctx, p, dbID, draftID, revision)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"revision":   strconv.FormatInt(revision, 10),
		"statements": results,
	})
}

func (s *Service) history(w http.ResponseWriter, r *http.Request) {
	_, dbID, err := s.authorize(r, false)
	if err != nil {
		fail(w, err)
		return
	}
	if r.PathValue("version") != "" {
		if r.URL.Query().Has("limit") || r.URL.Query().Has("before") {
			fail(w, connections.ErrInvalid)
			return
		}
		version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
		if err != nil || version < 1 {
			fail(w, connections.ErrInvalid)
			return
		}
		item, err := s.getHistoryDetail(r.Context(), dbID, version)
		if err != nil {
			fail(w, err)
			return
		}
		httpx.JSON(w, http.StatusOK, item)
		return
	}

	limit, before, err := parseCursorParams(r, "before")
	if err != nil {
		fail(w, err)
		return
	}
	items, more, next, err := s.listHistoryPage(r.Context(), dbID, before, limit)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"migrations":  items,
		"next_before": next,
		"has_more":    more,
	})
}

func (s *Service) reconcile(w http.ResponseWriter, r *http.Request) {
	p, dbID, err := s.authorize(r, true)
	if err != nil {
		fail(w, err)
		return
	}
	if r.ContentLength != 0 {
		var in struct{}
		if !httpx.Decode(w, r, &in) {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()

	results, err := s.reconcilePending(ctx, p, dbID)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"reconciled": results})
}

func parseDraftRevisionRequest(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	draftID, err := strconv.ParseInt(r.PathValue("draftID"), 10, 64)
	var in struct {
		Revision int64 `json:"revision,string"`
	}
	if !httpx.Decode(w, r, &in) {
		return 0, 0, false
	}
	if err != nil || draftID < 1 || in.Revision < 1 {
		fail(w, connections.ErrInvalid)
		return 0, 0, false
	}
	return draftID, in.Revision, true
}

func parseCursorParams(r *http.Request, cursorParam string) (int, int64, error) {
	limit := 50
	var cursor int64
	var err error
	if r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, connections.ErrInvalid
		}
	}
	if r.URL.Query().Has(cursorParam) {
		cursor, err = strconv.ParseInt(r.URL.Query().Get(cursorParam), 10, 64)
		if err != nil || cursor < 1 {
			return 0, 0, connections.ErrInvalid
		}
	}
	return limit, cursor, nil
}
