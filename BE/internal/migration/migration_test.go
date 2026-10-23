package migration

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

func TestDraftAndHistoryPermissionsAndIsolation(t *testing.T) {
	now := time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC)
	store := NewMemoryStore()

	// Hak akses pengujian:
	// User 1 = Admin platform (tanpa izin database)
	// User 2 = Editor pada DB 1
	// User 3 = Viewer pada DB 1, Editor pada DB 2
	grants := map[[2]int64][]policy.Access{
		{2, 1}: {policy.Edit},
		{3, 1}: {policy.View},
		{3, 2}: {policy.Edit},
	}
	loadGrants := func(_ context.Context, userID, databaseID int64) ([]policy.Access, error) {
		return grants[[2]int64{userID, databaseID}], nil
	}

	svc := NewService(store, loadGrants, func() time.Time { return now })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/databases/{id}/drafts", svc.HandleListDrafts)
	mux.HandleFunc("POST /api/v1/databases/{id}/drafts", svc.HandleCreateDraft)
	mux.HandleFunc("GET /api/v1/databases/{id}/drafts/{draftID}", svc.HandleGetDraft)
	mux.HandleFunc("PATCH /api/v1/databases/{id}/drafts/{draftID}", svc.HandleUpdateDraft)
	mux.HandleFunc("GET /api/v1/databases/{id}/migrations", svc.HandleListHistory)
	mux.HandleFunc("GET /api/v1/databases/{id}/migrations/{version}", svc.HandleGetHistoryByVersion)

	reqAs := func(p auth.Principal, method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, r)
		return rr
	}

	adminUser := auth.Principal{UserID: 1, Username: "admin", IsPlatformAdmin: true, IsActive: true}
	editorDB1 := auth.Principal{UserID: 2, Username: "editor1", IsPlatformAdmin: false, IsActive: true}
	viewerDB1 := auth.Principal{UserID: 3, Username: "viewer1", IsPlatformAdmin: false, IsActive: true}

	// Admin tanpa grant ditolak
	if rr := reqAs(adminUser, "POST", "/api/v1/databases/1/drafts", `{"title":"init","sql":"SELECT 1;"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for ungranted platform admin creating draft, got %d", rr.Code)
	}
	if rr := reqAs(adminUser, "GET", "/api/v1/databases/1/migrations", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for ungranted platform admin reading history, got %d", rr.Code)
	}

	// Pengguna View Only dilarang membuat draft
	if rr := reqAs(viewerDB1, "POST", "/api/v1/databases/1/drafts", `{"title":"hack","sql":"DROP TABLE users;"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for view-only user creating draft, got %d", rr.Code)
	}

	// Pengguna Edit membuat draft; identitas wajib dari session
	rr := reqAs(editorDB1, "POST", "/api/v1/databases/1/drafts", `{"title":"add index","sql":"CREATE INDEX idx ON products(name);","user_id":"999","created_by":"999"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating draft, got %d %s", rr.Code, rr.Body.String())
	}
	var createResp struct {
		Draft Draft `json:"draft"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&createResp); err != nil {
		t.Fatal(err)
	}
	if createResp.Draft.CreatedBy != 2 || createResp.Draft.CreatedByUsername != "editor1" {
		t.Fatalf("expected created_by=2 (editor1) from session, got %+v", createResp.Draft)
	}

	// Pengguna View Only dilarang mengubah draft
	if rr := reqAs(viewerDB1, "PATCH", "/api/v1/databases/1/drafts/1", `{"sql":"DROP TABLE products;"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for view-only user patching draft, got %d", rr.Code)
	}

	// Isolasi database: akses lintas database ditolak
	if rr := reqAs(viewerDB1, "PATCH", "/api/v1/databases/2/drafts/1", `{"title":"tamper"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on cross-database draft patch, got %d", rr.Code)
	}
	if rr := reqAs(editorDB1, "GET", "/api/v1/databases/2/migrations", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on unauthorized database ID, got %d", rr.Code)
	}

	// Pengguna Edit mengubah draft
	rr = reqAs(editorDB1, "PATCH", "/api/v1/databases/1/drafts/1", `{"title":"add product index","sql":"CREATE INDEX CONCURRENTLY idx ON products(name);"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 updating draft, got %d %s", rr.Code, rr.Body.String())
	}

	// Draft yang sudah dieksekusi dikunci dari perubahan
	draftIDStr := "1"
	finished := now.Add(2 * time.Second)
	store.AddHistoryEntry(HistoryEntry{
		DatabaseID:         1,
		DraftID:            &draftIDStr,
		Version:            1,
		SQL:                "CREATE INDEX CONCURRENTLY idx ON products(name);",
		ExecutedBy:         2,
		ExecutedByUsername: "editor1",
		StartedAt:          now,
		FinishedAt:         &finished,
		Status:             "success",
	})

	if rr := reqAs(editorDB1, "PATCH", "/api/v1/databases/1/drafts/1", `{"sql":"SELECT 2;"}`); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 when updating already-executed draft, got %d", rr.Code)
	}

	// Pengguna View Only dapat membaca riwayat dan detail migration
	rr = reqAs(viewerDB1, "GET", "/api/v1/databases/1/migrations", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"version":1`) {
		t.Fatalf("expected 200 reading history as viewer, got %d %s", rr.Code, rr.Body.String())
	}
	rr = reqAs(viewerDB1, "GET", "/api/v1/databases/1/migrations/1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"executed_by_username":"editor1"`) {
		t.Fatalf("expected 200 reading migration detail as viewer, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := reqAs(viewerDB1, "GET", "/api/v1/databases/1/migrations/99", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown version, got %d", rr.Code)
	}
}
