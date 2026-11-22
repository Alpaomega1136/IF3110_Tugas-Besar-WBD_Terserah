//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"terserah/backend/internal/runner"
)

// TestRetryAttemptAPI memverifikasi retry eksplisit, idempotensi, dan history detail.
func TestRetryAttemptAPI(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "retry-target"))
	grant(t, a, "editor", target.ID, "edit")
	grant(t, a, "viewer", target.ID, "view")
	editor := a.browser()
	editor.login(t, "editor")
	viewer := a.browser()
	viewer.login(t, "viewer")
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	w := editor.request("POST", base+"/drafts", map[string]string{"title": "Percobaan gagal", "sql": "SELECT 1/0", "sql_down": "SELECT 2"})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	path := fmt.Sprintf("%s/drafts/%d", base, draft.ID)
	w = editor.request("POST", path+"/execute", map[string]string{"revision": "1"})
	status(t, w, 200)
	var first runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil || first.Status != "failed" {
		t.Fatalf("eksekusi gagal: %s", w.Body.String())
	}
	status(t, viewer.request("POST", path+"/retry", map[string]string{"revision": "1", "failed_execution_id": first.ExecutionID}), 403)
	status(t, editor.request("POST", path+"/retry", map[string]string{"revision": "1", "failed_execution_id": "tidak-ada"}), 409)
	w = editor.request("POST", path+"/retry", map[string]string{"revision": "1", "failed_execution_id": first.ExecutionID})
	status(t, w, 200)
	var second runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || second.Status != "failed" || second.ExecutionID == first.ExecutionID || second.Version != first.Version+1 {
		t.Fatalf("retry tidak membuat attempt baru: %s", w.Body.String())
	}
	w = editor.request("POST", path+"/retry", map[string]string{"revision": "1", "failed_execution_id": first.ExecutionID})
	status(t, w, 200)
	var replay runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.ExecutionID != second.ExecutionID {
		t.Fatal("retry duplikat menjalankan SQL ulang")
	}
	w = editor.request("POST", path+"/execute", map[string]string{"revision": "1"})
	status(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.ExecutionID != second.ExecutionID {
		t.Fatal("endpoint execute tidak mengembalikan attempt terbaru")
	}
	w = viewer.request("GET", base+"/migration-executions/"+second.ExecutionID, nil)
	status(t, w, 200)
	var detail runner.History
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.SQL != draft.SQL || detail.RetryOfExecutionID == nil || *detail.RetryOfExecutionID != first.ExecutionID {
		t.Fatalf("detail attempt salah: %s", w.Body.String())
	}
	w = viewer.request("GET", base+"/migrations?hide_rollback=true", nil)
	status(t, w, 200)
	var page struct {
		Migrations []runner.History `json:"migrations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Migrations) != 2 {
		t.Fatalf("history harus berisi dua attempt gagal: %s", w.Body.String())
	}
	status(t, viewer.request("GET", base+"/migrations?hide_rollback=invalid", nil), 400)
	status(t, editor.request("GET", base+"/migration-executions/tidak-ada", nil), 404)
	var count int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE database_id=$1`, target.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("retry duplicate membuat history baru: %d %v", count, err)
	}
}
