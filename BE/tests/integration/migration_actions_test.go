//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/runner"
)

// setujuiAksiMigration menguji bahwa seluruh leader menerima SQL dan target yang benar.
func setujuiAksiMigration(t *testing.T, a *m1App, editor *browser, dbID, draftID, revision int64, action, target, expectedSQL string) {
	t.Helper()
	path := fmt.Sprintf("/api/v1/databases/%d/reviews", dbID)
	w := editor.request("POST", path, map[string]string{
		"draft_id": fmt.Sprint(draftID), "revision": fmt.Sprint(revision),
		"action_type": action, "target_execution_id": target,
	})
	status(t, w, 201)
	var out struct {
		Review approval.ReviewView `json:"review"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Review.SQLSnapshot != expectedSQL || out.Review.TargetExecutionID == nil || *out.Review.TargetExecutionID != target {
		t.Fatalf("review tidak terikat pada SQL/target: %+v", out.Review)
	}
	for _, group := range out.Review.Approvals {
		leader := a.leaders[group.LeaderUsername]
		if leader == nil {
			leader = a.browser()
			leader.login(t, group.LeaderUsername)
			a.leaders[group.LeaderUsername] = leader
		}
		status(t, leader.request("POST", fmt.Sprintf("%s/%d/approve", path, out.Review.ID), map[string]string{"note": "compatible"}), 200)
	}
}

// TestRollbackForwardAPI memeriksa perubahan target nyata, approval, history, dan idempotensi.
func TestRollbackForwardAPI(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "rollback-forward-target"))
	grant(t, a, "editor", target.ID, "edit")
	grant(t, a, "viewer", target.ID, "view")
	editor := a.browser()
	editor.login(t, "editor")
	viewer := a.browser()
	viewer.login(t, "viewer")

	table := fmt.Sprintf("stage6_%d", time.Now().UnixNano())
	up, down := "CREATE TABLE "+table+"(id integer)", "DROP TABLE "+table
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	w := editor.request("POST", base+"/drafts", map[string]string{"title": "Reversible", "sql": up, "sql_down": down})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	w = editor.request("POST", fmt.Sprintf("%s/drafts/%d/execute", base, draft.ID), map[string]string{"revision": fmt.Sprint(draft.Revision)})
	status(t, w, 200)
	var applied runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &applied); err != nil || applied.Status != "success" {
		t.Fatalf("Up harus sukses: %s", w.Body.String())
	}
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) }()
	terdaftar := func() bool {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	}
	if !terdaftar() {
		t.Fatal("SQL Up tidak mengubah database target")
	}

	rollbackPath := base + "/migration-executions/" + applied.ExecutionID + "/rollback"
	status(t, viewer.request("POST", rollbackPath, nil), 403)
	status(t, editor.request("POST", rollbackPath, nil), 409) // Belum disetujui.
	status(t, editor.request("POST", base+"/migration-executions/tidak-ada/rollback", nil), 404)
	setujuiAksiMigration(t, a, editor, target.ID, draft.ID, draft.Revision, "rollback", applied.ExecutionID, down)
	w = editor.request("POST", rollbackPath, nil)
	status(t, w, 200)
	var rolled runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &rolled); err != nil || rolled.Status != "success" || rolled.ExecutionID == applied.ExecutionID {
		t.Fatalf("rollback gagal: %s", w.Body.String())
	}
	if terdaftar() {
		t.Fatal("SQL Down tidak membatalkan schema")
	}
	w = editor.request("POST", rollbackPath, nil)
	status(t, w, 200)
	var replay runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.ExecutionID != rolled.ExecutionID {
		t.Fatal("rollback duplikat harus mengembalikan hasil sebelumnya")
	}
	var pairCount int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM migration_rollback_pairs WHERE database_id=$1 AND applied_execution_id=$2 AND rollback_execution_id=$3`,
		target.ID, applied.ExecutionID, rolled.ExecutionID).Scan(&pairCount); err != nil || pairCount != 1 {
		t.Fatalf("pasangan rollback tidak dicatat: %d %v", pairCount, err)
	}
	w = viewer.request("GET", base+"/migrations?hide_rollback=true", nil)
	status(t, w, 200)
	var filtered struct {
		Migrations []runner.History `json:"migrations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &filtered); err != nil || len(filtered.Migrations) != 0 {
		t.Fatalf("hide rollback harus menyembunyikan pasangan: %s", w.Body.String())
	}

	forwardPath := base + "/migration-executions/" + rolled.ExecutionID + "/forward"
	status(t, editor.request("POST", forwardPath, nil), 409)
	setujuiAksiMigration(t, a, editor, target.ID, draft.ID, draft.Revision, "forward", rolled.ExecutionID, up)
	w = editor.request("POST", forwardPath, nil)
	status(t, w, 200)
	var forwarded runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &forwarded); err != nil || forwarded.Status != "success" || forwarded.ExecutionID == rolled.ExecutionID {
		t.Fatalf("forward gagal: %s", w.Body.String())
	}
	if !terdaftar() {
		t.Fatal("forward tidak menerapkan kembali SQL Up")
	}
	w = viewer.request("GET", base+"/migration-executions/"+forwarded.ExecutionID, nil)
	status(t, w, 200)
	var detail runner.History
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.ActionType != "forward" || detail.TargetExecutionID == nil || *detail.TargetExecutionID != rolled.ExecutionID || detail.SQL != up {
		t.Fatalf("detail forward salah: %s", w.Body.String())
	}
	w = viewer.request("GET", base+"/migration-state", nil)
	status(t, w, 200)
	var state struct {
		Active []struct {
			ExecutionID *string `json:"execution_id"`
		} `json:"active"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || len(state.Active) != 1 || state.Active[0].ExecutionID == nil || *state.Active[0].ExecutionID != forwarded.ExecutionID {
		t.Fatalf("state setelah forward salah: %s", w.Body.String())
	}
	var count int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE database_id=$1`, target.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("riwayat Up/rollback/forward harus tepat 3 attempt: count=%d err=%v", count, err)
	}
	// Bersihkan perubahan target untuk test lain, tanpa memodifikasi riwayat metadata.
	if _, err := conn.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackForwardRetryAPI memverifikasi attempt gagal tidak mengubah posisi
// dan retry eksplisit membuat execution baru tanpa menghilangkan riwayat lama.
func TestRollbackForwardRetryAPI(t *testing.T) {
	a := newM1App(t)
	ctx := context.Background()
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "rollback-retry-target"))
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	table := fmt.Sprintf("stage6_retry_%d", time.Now().UnixNano())
	up, down := "CREATE TABLE "+table+"(id integer)", "DROP TABLE "+table
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	w := editor.request("POST", base+"/drafts", map[string]string{"title": "Retry reversibel", "sql": up, "sql_down": down})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	w = editor.request("POST", fmt.Sprintf("%s/drafts/%d/execute", base, draft.ID), map[string]string{"revision": fmt.Sprint(draft.Revision)})
	status(t, w, 200)
	var applied runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &applied); err != nil || applied.Status != "success" {
		t.Fatalf("Up gagal: %s", w.Body.String())
	}
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	defer func() { _, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) }()
	if _, err := conn.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	setujuiAksiMigration(t, a, editor, target.ID, draft.ID, draft.Revision, "rollback", applied.ExecutionID, down)
	path := base + "/migration-executions/" + applied.ExecutionID + "/rollback"
	w = editor.request("POST", path, nil)
	status(t, w, 200)
	var failed runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &failed); err != nil || failed.Status != "failed" {
		t.Fatalf("rollback seharusnya gagal: %s", w.Body.String())
	}
	var pairs int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM migration_rollback_pairs WHERE database_id=$1`, target.ID).Scan(&pairs); err != nil || pairs != 0 {
		t.Fatalf("rollback gagal tidak boleh menghasilkan pasangan: %d %v", pairs, err)
	}
	if _, err := conn.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	w = editor.request("POST", path, map[string]string{"failed_execution_id": failed.ExecutionID})
	status(t, w, 200)
	var rolled runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &rolled); err != nil || rolled.Status != "success" || rolled.ExecutionID == failed.ExecutionID {
		t.Fatalf("retry rollback gagal: %s", w.Body.String())
	}
	w = editor.request("POST", path, map[string]string{"failed_execution_id": failed.ExecutionID})
	status(t, w, 200)
	var replay runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil || replay.ExecutionID != rolled.ExecutionID {
		t.Fatal("retry rollback duplikat membuat attempt baru")
	}
	setujuiAksiMigration(t, a, editor, target.ID, draft.ID, draft.Revision, "forward", rolled.ExecutionID, up)
	forwardPath := base + "/migration-executions/" + rolled.ExecutionID + "/forward"
	if _, err := conn.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	w = editor.request("POST", forwardPath, nil)
	status(t, w, 200)
	var forwardFailed runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &forwardFailed); err != nil || forwardFailed.Status != "failed" {
		t.Fatalf("forward seharusnya gagal: %s", w.Body.String())
	}
	if _, err := conn.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	w = editor.request("POST", forwardPath, map[string]string{"failed_execution_id": forwardFailed.ExecutionID})
	status(t, w, 200)
	var forwardSuccess runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &forwardSuccess); err != nil || forwardSuccess.Status != "success" || forwardSuccess.ExecutionID == forwardFailed.ExecutionID {
		t.Fatalf("retry forward gagal: %s", w.Body.String())
	}
	var count int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE database_id=$1`, target.ID).Scan(&count); err != nil || count != 5 {
		t.Fatalf("semua 5 attempt harus tersimpan: count=%d err=%v", count, err)
	}
	if _, err := conn.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
}
