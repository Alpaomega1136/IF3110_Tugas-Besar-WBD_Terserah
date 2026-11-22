//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/runner"
)

// TestApprovalRevisiBerubah memastikan review yang sudah stale tidak dapat disetujui.
func TestApprovalRevisiBerubah(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "approval-revision-guard"))
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	w := editor.request("POST", base+"/drafts", map[string]string{
		"title": "Revisi awal", "sql": "CREATE TABLE review_old (id int)", "sql_down": "DROP TABLE review_old",
	})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	w = editor.request("POST", base+"/reviews", map[string]string{"draft_id": fmt.Sprint(draft.ID), "revision": fmt.Sprint(draft.Revision)})
	status(t, w, 201)
	var out struct {
		Review approval.ReviewView `json:"review"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	w = editor.request("PATCH", fmt.Sprintf("%s/drafts/%d", base, draft.ID), map[string]string{
		"revision": fmt.Sprint(draft.Revision), "title": "Revisi baru",
		"sql": "CREATE TABLE review_new (id int)", "sql_down": "DROP TABLE review_new",
	})
	status(t, w, 200)
	w = editor.request("POST", fmt.Sprintf("%s/reviews/%d/approve", base, out.Review.ID), map[string]string{"note": "setuju"})
	status(t, w, 409)
	if !strings.Contains(w.Body.String(), "review_conflict") {
		t.Fatalf("harus mengembalikan review_conflict: %s", w.Body.String())
	}
	w = editor.request("GET", fmt.Sprintf("%s/reviews/%d", base, out.Review.ID), nil)
	status(t, w, 200)
	var detail struct {
		Review approval.ReviewView `json:"review"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Review.Status == approval.StatusApproved {
		t.Fatal("review stale tidak boleh disetujui")
	}
}

// TestApprovalGrantBaru memastikan seluruh group saat ini wajib memberi persetujuan.
func TestApprovalGrantBaru(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "approval-grant-guard"))
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	w := editor.request("POST", base+"/drafts", map[string]string{
		"title": "Perubahan schema", "sql": "CREATE TABLE approval_guard (id int)", "sql_down": "DROP TABLE approval_guard",
	})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	store := approval.NewPostgresStore(a.db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.EnsureApproved(ctx, target.ID, draft.ID, draft.Revision, "up"); err != nil {
		t.Fatal(err)
	}
	grant(t, a, "viewer", target.ID, "view")
	if _, err := store.EnsureApproved(ctx, target.ID, draft.ID, draft.Revision, "up"); !errors.Is(err, approval.ErrNotApproved) {
		t.Fatalf("grant baru wajib menghapus kelengkapan approval lama, err=%v", err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	if _, err := store.EnsureApproved(ctx, target.ID, draft.ID, draft.Revision, "up"); err != nil {
		t.Fatal(err)
	}
}

// TestApprovalSQLDanTargetTepat memastikan persetujuan rollback tidak dipakai untuk aksi atau SQL lain.
func TestApprovalSQLDanTargetTepat(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "approval-target-guard"))
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	table := fmt.Sprintf("stage7_guard_%d", time.Now().UnixNano())
	up, down := "CREATE TABLE "+table+" (id int)", "DROP TABLE "+table
	w := editor.request("POST", base+"/drafts", map[string]string{
		"title": "Reversible", "sql": up, "sql_down": down,
	})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	w = editor.request("POST", fmt.Sprintf("%s/drafts/%d/execute", base, draft.ID), map[string]string{"revision": fmt.Sprint(draft.Revision)})
	status(t, w, 200)
	var executed runner.Result
	if err := json.Unmarshal(w.Body.Bytes(), &executed); err != nil || executed.Status != "success" {
		t.Fatalf("Up gagal: %s", w.Body.String())
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Error(err)
		}
	})
	setujuiAksiMigration(t, a, editor, target.ID, draft.ID, draft.Revision, "rollback", executed.ExecutionID, down)
	ctx := context.Background()
	tx, err := a.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	check := func(action, targetID, sql string, allowed bool) {
		t.Helper()
		_, err := approval.EnsureApprovedExecutionTx(ctx, tx, target.ID, draft.ID, draft.Revision, action, targetID, sql)
		if allowed && err != nil {
			t.Fatalf("approval yang tepat harus diterima: %v", err)
		}
		if !allowed && !errors.Is(err, approval.ErrNotApproved) {
			t.Fatalf("approval salah harus ditolak: %v", err)
		}
	}
	check("rollback", executed.ExecutionID, down, true)
	check("rollback", executed.ExecutionID, "DROP TABLE yang_lain", false)
	check("rollback", "target-yang-lain", down, false)
	check("forward", executed.ExecutionID, up, false)
}

// TestApprovalLeaderDiganti memastikan keputusan leader lama tidak dapat dipakai kembali.
func TestApprovalLeaderDiganti(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")
	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "approval-leader-guard"))
	grant(t, a, "editor", target.ID, "edit")
	editor := a.browser()
	editor.login(t, "editor")
	base := fmt.Sprintf("/api/v1/databases/%d", target.ID)
	w := editor.request("POST", base+"/drafts", map[string]string{
		"title": "Perubahan leader", "sql": "CREATE TABLE approval_leader_guard (id int)", "sql_down": "DROP TABLE approval_leader_guard",
	})
	status(t, w, 201)
	var draft runner.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	ctx := context.Background()
	store := approval.NewPostgresStore(a.db)
	if _, err := store.EnsureApproved(ctx, target.ID, draft.ID, draft.Revision, "up"); err != nil {
		t.Fatal(err)
	}
	var groupID int64
	if err := a.db.QueryRow(ctx, `SELECT p.group_id FROM group_database_permissions p
		JOIN user_groups ug ON ug.group_id=p.group_id
		WHERE p.database_id=$1 AND ug.user_id=$2 AND p.access_level='edit'`, target.ID, a.users["editor"]).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/users/%d", groupID, a.users["viewer"]), nil), 200)
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/leader", groupID), map[string]string{"user_id": fmt.Sprint(a.users["viewer"])}), 200)
	if _, err := store.EnsureApproved(ctx, target.ID, draft.ID, draft.Revision, "up"); !errors.Is(err, approval.ErrNotApproved) {
		t.Fatalf("pergantian leader wajib membatalkan kelengkapan persetujuan lama, err=%v", err)
	}
	approveDraft(t, a, editor, target.ID, draft.ID, draft.Revision)
	if _, err := store.EnsureApproved(ctx, target.ID, draft.ID, draft.Revision, "up"); err != nil {
		t.Fatal(err)
	}
}
