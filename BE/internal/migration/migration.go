package migration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/policy"
)

const MaxSQLLen = 1 << 20 // 1 MiB limit for draft SQL

var (
	ErrNotFound     = errors.New("resource not found")
	ErrDraftLocked  = errors.New("draft has already been executed")
	ErrInvalidInput = errors.New("invalid input")
)

type Draft struct {
	ID                int64     `json:"id,string"`
	DatabaseID        int64     `json:"database_id,string"`
	Title             string    `json:"title"`
	SQL               string    `json:"sql"`
	CreatedBy         int64     `json:"created_by,string"`
	CreatedByUsername string    `json:"created_by_username"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type HistoryEntry struct {
	ID                 int64      `json:"id,string"`
	DatabaseID         int64      `json:"database_id,string"`
	DraftID            *string    `json:"draft_id"`
	Version            int64      `json:"version"`
	SQL                string     `json:"sql"`
	ExecutedBy         int64      `json:"executed_by,string"`
	ExecutedByUsername string     `json:"executed_by_username"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at"`
	Status             string     `json:"status"`
	ErrorCode          *string    `json:"error_code"`
}

type UpdateDraftParams struct {
	Title *string
	SQL   *string
}

type Store interface {
	ListDrafts(ctx context.Context, databaseID int64) ([]Draft, error)
	GetDraft(ctx context.Context, databaseID, draftID int64) (Draft, error)
	CreateDraft(ctx context.Context, databaseID, createdBy int64, createdByUsername, title, sqlText string, now time.Time) (Draft, error)
	UpdateDraft(ctx context.Context, databaseID, draftID int64, params UpdateDraftParams, now time.Time) (Draft, error)
	ListHistory(ctx context.Context, databaseID int64) ([]HistoryEntry, error)
	GetHistoryByVersion(ctx context.Context, databaseID, version int64) (HistoryEntry, error)
}

type Service struct {
	store      Store
	loadGrants policy.GrantFunc
	now        func() time.Time
}

func NewService(store Store, loadGrants policy.GrantFunc, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		store:      store,
		loadGrants: loadGrants,
		now:        now,
	}
}

func (s *Service) authorize(ctx context.Context, w http.ResponseWriter, r *http.Request, write bool) (auth.Principal, int64, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication required")
		return auth.Principal{}, 0, false
	}
	dbID, err := parsePositiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid database ID")
		return auth.Principal{}, 0, false
	}
	if _, err := policy.CheckAccess(ctx, s.loadGrants, p.UserID, dbID, write); err != nil {
		if errors.Is(err, policy.ErrForbidden) {
			writeError(w, http.StatusForbidden, "forbidden", "Insufficient database permission")
			return auth.Principal{}, 0, false
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Permission check failed")
		return auth.Principal{}, 0, false
	}
	return p, dbID, true
}

func (s *Service) HandleListDrafts(w http.ResponseWriter, r *http.Request) {
	_, dbID, ok := s.authorize(r.Context(), w, r, true)
	if !ok {
		return
	}
	drafts, err := s.store.ListDrafts(r.Context(), dbID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot list migration drafts")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": drafts})
}

func (s *Service) HandleGetDraft(w http.ResponseWriter, r *http.Request) {
	_, dbID, ok := s.authorize(r.Context(), w, r, true)
	if !ok {
		return
	}
	draftID, err := parsePositiveInt(r.PathValue("draftID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid draft ID")
		return
	}
	draft, err := s.store.GetDraft(r.Context(), dbID, draftID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot load migration draft")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"draft": draft})
}

func (s *Service) HandleCreateDraft(w http.ResponseWriter, r *http.Request) {
	p, dbID, ok := s.authorize(r.Context(), w, r, true)
	if !ok {
		return
	}
	var req struct {
		Title string `json:"title"`
		SQL   string `json:"sql"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	title := strings.TrimSpace(req.Title)
	sqlText := strings.TrimSpace(req.SQL)
	if len(title) < 1 || len(title) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_input", "Title must be 1..200 characters")
		return
	}
	if len(sqlText) < 1 || len(sqlText) > MaxSQLLen {
		writeError(w, http.StatusBadRequest, "invalid_input", "SQL must not be empty and must not exceed 1 MiB")
		return
	}
	// Identitas pembuat selalu diambil dari Principal session terotentikasi, bukan input client.
	draft, err := s.store.CreateDraft(r.Context(), dbID, p.UserID, p.Username, title, sqlText, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Database not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot create migration draft")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"draft": draft})
}

func (s *Service) HandleUpdateDraft(w http.ResponseWriter, r *http.Request) {
	_, dbID, ok := s.authorize(r.Context(), w, r, true)
	if !ok {
		return
	}
	draftID, err := parsePositiveInt(r.PathValue("draftID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid draft ID")
		return
	}
	var req struct {
		Title *string `json:"title"`
		SQL   *string `json:"sql"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid JSON payload")
		return
	}
	if req.Title == nil && req.SQL == nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "At least one of title or sql must be provided")
		return
	}
	var params UpdateDraftParams
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if len(t) < 1 || len(t) > 200 {
			writeError(w, http.StatusBadRequest, "invalid_input", "Title must be 1..200 characters")
			return
		}
		params.Title = &t
	}
	if req.SQL != nil {
		sq := strings.TrimSpace(*req.SQL)
		if len(sq) < 1 || len(sq) > MaxSQLLen {
			writeError(w, http.StatusBadRequest, "invalid_input", "SQL must not be empty and must not exceed 1 MiB")
			return
		}
		params.SQL = &sq
	}

	draft, err := s.store.UpdateDraft(r.Context(), dbID, draftID, params, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Draft not found")
			return
		}
		if errors.Is(err, ErrDraftLocked) {
			writeError(w, http.StatusConflict, "conflict", "Executed migration draft cannot be modified")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot update migration draft")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"draft": draft})
}

func (s *Service) HandleListHistory(w http.ResponseWriter, r *http.Request) {
	_, dbID, ok := s.authorize(r.Context(), w, r, false)
	if !ok {
		return
	}
	history, err := s.store.ListHistory(r.Context(), dbID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot list migration history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"migrations": history})
}

func (s *Service) HandleGetHistoryByVersion(w http.ResponseWriter, r *http.Request) {
	_, dbID, ok := s.authorize(r.Context(), w, r, false)
	if !ok {
		return
	}
	version, err := parsePositiveInt(r.PathValue("version"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", "Invalid migration version")
		return
	}
	entry, err := s.store.GetHistoryByVersion(r.Context(), dbID, version)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Migration version not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Cannot read migration detail")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"migration": entry})
}

// PostgresStore implementasi Store migration berbasis PostgreSQL metadata platform.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) ListDrafts(ctx context.Context, databaseID int64) ([]Draft, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.database_id, d.title, d.sql, d.created_by, u.username, d.created_at, d.updated_at
		FROM migration_drafts d
		JOIN users u ON u.id = d.created_by
		WHERE d.database_id = $1
		ORDER BY d.id ASC
	`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	drafts := []Draft{}
	for rows.Next() {
		var d Draft
		if err := rows.Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.CreatedBy, &d.CreatedByUsername, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
	}
	return drafts, rows.Err()
}

func (s *PostgresStore) GetDraft(ctx context.Context, databaseID, draftID int64) (Draft, error) {
	var d Draft
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.database_id, d.title, d.sql, d.created_by, u.username, d.created_at, d.updated_at
		FROM migration_drafts d
		JOIN users u ON u.id = d.created_by
		WHERE d.id = $1 AND d.database_id = $2
	`, draftID, databaseID).Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.CreatedBy, &d.CreatedByUsername, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	return d, err
}

func (s *PostgresStore) CreateDraft(ctx context.Context, databaseID, createdBy int64, createdByUsername, title, sqlText string, now time.Time) (Draft, error) {
	var d Draft
	err := s.pool.QueryRow(ctx, `
		INSERT INTO migration_drafts (database_id, title, sql, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		RETURNING id, database_id, title, sql, created_by, created_at, updated_at
	`, databaseID, title, sqlText, createdBy, now).Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Draft{}, err
	}
	d.CreatedByUsername = createdByUsername
	return d, nil
}

func (s *PostgresStore) UpdateDraft(ctx context.Context, databaseID, draftID int64, params UpdateDraftParams, now time.Time) (Draft, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback(context.Background())

	var d Draft
	err = tx.QueryRow(ctx, `
		SELECT d.id, d.database_id, d.title, d.sql, d.created_by, u.username, d.created_at, d.updated_at
		FROM migration_drafts d
		JOIN users u ON u.id = d.created_by
		WHERE d.id = $1 AND d.database_id = $2
		FOR UPDATE OF d
	`, draftID, databaseID).Scan(&d.ID, &d.DatabaseID, &d.Title, &d.SQL, &d.CreatedBy, &d.CreatedByUsername, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}

	var executed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history WHERE draft_id = $1 AND database_id = $2)`, draftID, databaseID).Scan(&executed); err != nil {
		return Draft{}, err
	}
	if executed {
		return Draft{}, ErrDraftLocked
	}

	newTitle := d.Title
	if params.Title != nil {
		newTitle = *params.Title
	}
	newSQL := d.SQL
	if params.SQL != nil {
		newSQL = *params.SQL
	}

	err = tx.QueryRow(ctx, `
		UPDATE migration_drafts
		SET title = $3, sql = $4, updated_at = $5
		WHERE id = $1 AND database_id = $2
		RETURNING title, sql, updated_at
	`, draftID, databaseID, newTitle, newSQL, now).Scan(&d.Title, &d.SQL, &d.UpdatedAt)
	if err != nil {
		return Draft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Draft{}, err
	}
	return d, nil
}

func (s *PostgresStore) ListHistory(ctx context.Context, databaseID int64) ([]HistoryEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT mh.id, mh.database_id, mh.draft_id, mh.version, mh.sql,
		       mh.executed_by, u.username, mh.started_at, mh.finished_at, mh.status, mh.error_code
		FROM migration_history mh
		JOIN users u ON u.id = mh.executed_by
		WHERE mh.database_id = $1
		ORDER BY mh.version ASC, mh.id ASC
	`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := []HistoryEntry{}
	for rows.Next() {
		e, err := scanHistoryRow(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *PostgresStore) GetHistoryByVersion(ctx context.Context, databaseID, version int64) (HistoryEntry, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT mh.id, mh.database_id, mh.draft_id, mh.version, mh.sql,
		       mh.executed_by, u.username, mh.started_at, mh.finished_at, mh.status, mh.error_code
		FROM migration_history mh
		JOIN users u ON u.id = mh.executed_by
		WHERE mh.database_id = $1 AND mh.version = $2
	`, databaseID, version)
	e, err := scanHistoryRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return HistoryEntry{}, ErrNotFound
	}
	return e, err
}

func scanHistoryRow(row pgx.Row) (HistoryEntry, error) {
	var e HistoryEntry
	var draftID *int64
	if err := row.Scan(
		&e.ID, &e.DatabaseID, &draftID, &e.Version, &e.SQL,
		&e.ExecutedBy, &e.ExecutedByUsername, &e.StartedAt, &e.FinishedAt, &e.Status, &e.ErrorCode,
	); err != nil {
		return HistoryEntry{}, err
	}
	if draftID != nil {
		s := strconv.FormatInt(*draftID, 10)
		e.DraftID = &s
	}
	return e, nil
}

// MemoryStore implementasi Store migration in-memory untuk pengujian unit.
type MemoryStore struct {
	mu      sync.Mutex
	nextDID int64
	nextHID int64
	drafts  map[int64]Draft
	history []HistoryEntry
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		drafts: make(map[int64]Draft),
	}
}

func (m *MemoryStore) AddHistoryEntry(e HistoryEntry) HistoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.ID == 0 {
		m.nextHID++
		e.ID = m.nextHID
	}
	m.history = append(m.history, e)
	return e
}

func (m *MemoryStore) ListDrafts(_ context.Context, databaseID int64) ([]Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Draft{}
	for _, d := range m.drafts {
		if d.DatabaseID == databaseID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) GetDraft(_ context.Context, databaseID, draftID int64) (Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[draftID]
	if !ok || d.DatabaseID != databaseID {
		return Draft{}, ErrNotFound
	}
	return d, nil
}

func (m *MemoryStore) CreateDraft(_ context.Context, databaseID, createdBy int64, createdByUsername, title, sqlText string, now time.Time) (Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextDID++
	d := Draft{
		ID:                m.nextDID,
		DatabaseID:        databaseID,
		Title:             title,
		SQL:               sqlText,
		CreatedBy:         createdBy,
		CreatedByUsername: createdByUsername,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	m.drafts[d.ID] = d
	return d, nil
}

func (m *MemoryStore) UpdateDraft(_ context.Context, databaseID, draftID int64, params UpdateDraftParams, now time.Time) (Draft, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.drafts[draftID]
	if !ok || d.DatabaseID != databaseID {
		return Draft{}, ErrNotFound
	}
	draftStr := strconv.FormatInt(draftID, 10)
	for _, h := range m.history {
		if h.DatabaseID == databaseID && h.DraftID != nil && *h.DraftID == draftStr {
			return Draft{}, ErrDraftLocked
		}
	}
	if params.Title != nil {
		d.Title = *params.Title
	}
	if params.SQL != nil {
		d.SQL = *params.SQL
	}
	d.UpdatedAt = now
	m.drafts[draftID] = d
	return d, nil
}

func (m *MemoryStore) ListHistory(_ context.Context, databaseID int64) ([]HistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []HistoryEntry{}
	for _, h := range m.history {
		if h.DatabaseID == databaseID {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemoryStore) GetHistoryByVersion(_ context.Context, databaseID, version int64) (HistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.history {
		if h.DatabaseID == databaseID && h.Version == version {
			return h, nil
		}
	}
	return HistoryEntry{}, ErrNotFound
}

func parsePositiveInt(raw string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v <= 0 {
		return 0, ErrInvalidInput
	}
	return v, nil
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
