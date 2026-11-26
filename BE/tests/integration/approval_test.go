//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"terserah/backend/internal/admin"
	"terserah/backend/internal/approval"
	"terserah/backend/internal/runner"
)

func TestGroupLeaderAndApprovalWorkflowIntegration(t *testing.T) {
	a := newM1App(t)
	admin := a.browser()
	admin.login(t, "admin")

	target := createTarget(t, admin, targetInput(t, "TEST_TARGET_DATABASE_URL", "approval-target"))

	createGroup := func(name string) int64 {
		w := admin.request("POST", "/api/v1/admin/groups", map[string]string{"name": name})
		status(t, w, 201)
		var out struct {
			Group struct {
				ID int64 `json:"id,string"`
			} `json:"group"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Group.ID
	}

	editGroupID := createGroup("edit-team")
	viewGroupID := createGroup("view-team")

	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/users/%d", editGroupID, a.users["editor"]), nil), 200)
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/databases/%d", editGroupID, target.ID), map[string]string{"access_level": "edit"}), 200)

	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/users/%d", viewGroupID, a.users["viewer"]), nil), 200)
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/databases/%d", viewGroupID, target.ID), map[string]string{"access_level": "view"}), 200)

	editor := a.browser()
	editor.login(t, "editor")
	viewer := a.browser()
	viewer.login(t, "viewer")
	outsider := a.browser()
	outsider.login(t, "outsider")

	wDraft := editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/drafts", target.ID), map[string]string{
		"title":    "add users table",
		"sql":      "CREATE TABLE approval_demo(id int PRIMARY KEY);",
		"sql_down": "DROP TABLE IF EXISTS approval_demo;",
	})
	status(t, wDraft, 201)
	var draft runner.Draft
	if err := json.Unmarshal(wDraft.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}

	reviewsPath := fmt.Sprintf("/api/v1/databases/%d/reviews", target.ID)

	// Cannot submit review before every accessing group has a leader assigned.
	wMissing := editor.request("POST", reviewsPath, map[string]string{
		"draft_id": fmt.Sprint(draft.ID),
		"revision": fmt.Sprint(draft.Revision),
	})
	status(t, wMissing, 409)
	if !strings.Contains(wMissing.Body.String(), "group_leader_required") {
		t.Fatalf("expected group_leader_required, got %s", wMissing.Body.String())
	}

	// Non-member cannot be assigned as group leader.
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/leader", editGroupID), map[string]string{
		"user_id": fmt.Sprint(a.users["outsider"]),
	}), 409)

	// Assign leaders for both edit-team and view-team.
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/leader", editGroupID), map[string]string{
		"user_id": fmt.Sprint(a.users["editor"]),
	}), 200)
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/leader", viewGroupID), map[string]string{
		"user_id": fmt.Sprint(a.users["viewer"]),
	}), 200)

	// Active leader cannot be removed from group or deactivated before replacement.
	status(t, admin.request("DELETE", fmt.Sprintf("/api/v1/admin/groups/%d/users/%d", viewGroupID, a.users["viewer"]), nil), 409)
	status(t, admin.request("PATCH", fmt.Sprintf("/api/v1/admin/users/%d", a.users["viewer"]), map[string]bool{"is_active": false}), 409)

	// Submit review now succeeds and lists both accessing groups.
	wSubmit := editor.request("POST", reviewsPath, map[string]string{
		"draft_id": fmt.Sprint(draft.ID),
		"revision": fmt.Sprint(draft.Revision),
	})
	status(t, wSubmit, 201)
	var submitted struct {
		Review approval.ReviewView `json:"review"`
	}
	if err := json.Unmarshal(wSubmit.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.Review.Status != approval.StatusPendingReview || len(submitted.Review.Approvals) != 2 {
		t.Fatalf("unexpected submitted review: %+v", submitted.Review)
	}
	// Direct requests cannot bypass the approval workflow.
	executePath := fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", target.ID, draft.ID)
	blocked := editor.request("POST", executePath, map[string]string{"revision": fmt.Sprint(draft.Revision)})
	status(t, blocked, 409)
	if !strings.Contains(blocked.Body.String(), "approval_incomplete") {
		t.Fatalf("expected approval_incomplete, got %s", blocked.Body.String())
	}

	reviewPath := fmt.Sprintf("%s/%d", reviewsPath, submitted.Review.ID)

	// Leader 1 (editor) approves -> still pending_review because view-team has not approved yet.
	wApprove1 := editor.request("POST", reviewPath+"/approve", map[string]string{"note": "Schema looks good"})
	status(t, wApprove1, 200)
	if err := json.Unmarshal(wApprove1.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.Review.Status != approval.StatusPendingReview {
		t.Fatalf("expected pending_review after 1 of 2 approvals, got %s", submitted.Review.Status)
	}

	// Outsider cannot approve.
	status(t, outsider.request("POST", reviewPath+"/approve", map[string]string{"note": "intrude"}), 403)

	// Leader 2 (viewer) approves -> transitions to approved!
	wApprove2 := viewer.request("POST", reviewPath+"/approve", map[string]string{"note": "Compatible with reader service"})
	status(t, wApprove2, 200)
	if err := json.Unmarshal(wApprove2.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.Review.Status != approval.StatusApproved {
		t.Fatalf("expected approved after all leaders approved, got %s", submitted.Review.Status)
	}
}

func TestReviewInboxFiltersBeforePagination(t *testing.T) {
	f := newQueueFixture(t)
	draft := f.draft(t, f.first, "SELECT 1")
	hiddenDraft := f.draft(t, f.second, "SELECT 2")
	ctx := context.Background()
	var oldest int64
	if err := f.a.db.QueryRow(ctx, `INSERT INTO migration_reviews
		(database_id, draft_id, draft_revision, requested_by, sql_snapshot)
		VALUES ($1,$2,1,$3,'SELECT 1') RETURNING id`, f.first, draft.ID, f.actor.ID).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.Exec(ctx, `INSERT INTO migration_reviews
		(database_id, draft_id, draft_revision, requested_by, sql_snapshot, status)
		SELECT $1::bigint,$2::bigint,1,$3::bigint,'SELECT 1','cancelled' FROM generate_series(1,101)
		UNION ALL SELECT $4::bigint,$5::bigint,1,$3,'SELECT 2','pending_review'
		UNION ALL SELECT $1,$2,1,$3,'SELECT 1','pending_review'`, f.first, draft.ID, f.actor.ID, f.second, hiddenDraft.ID); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/reviews?only_actionable=true&limit=1"
	request := func(path string) approval.ReviewPage {
		t.Helper()
		w := f.viewer.request("GET", path, nil)
		status(t, w, 200)
		var page approval.ReviewPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	p := request(base)
	if p.Total == nil || *p.Total != 2 || !p.HasMore || p.NextBefore == nil || len(p.Reviews) != 1 ||
		p.Reviews[0].DatabaseID != f.first || p.Reviews[0].ID <= oldest || !p.Reviews[0].CanCurrentUserAct {
		t.Fatalf("unexpected actionable inbox: %+v", p)
	}
	p = request(base + "&before=" + *p.NextBefore)
	if p.HasMore || p.NextBefore != nil || len(p.Reviews) != 1 || p.Reviews[0].ID != oldest || !p.Reviews[0].CanCurrentUserAct {
		t.Fatalf("second page lost the old pending review: %+v", p)
	}
	p = request(fmt.Sprintf("/api/v1/databases/%d/reviews?limit=1&status=cancelled", f.first))
	if !p.HasMore || len(p.Reviews) != 1 || p.Reviews[0].Status != approval.StatusCancelled {
		t.Fatalf("database review filter did not run before pagination: %+v", p)
	}
}

func TestLeaderAssignmentWaitsForUserDeactivation(t *testing.T) {
	a := newM1App(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := admin.NewPostgresStore(a.db)
	group, err := store.CreateGroup(ctx, "concurrent-leader")
	if err != nil {
		t.Fatal(err)
	}
	user := a.users["viewer"]
	if err := store.AddUserToGroup(ctx, group.ID, user); err != nil {
		t.Fatal(err)
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT id FROM users WHERE id=$1 FOR UPDATE", user); err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.SetGroupLeader(ctx, group.ID, user)
		result <- err
	}()
	for {
		var waiting bool
		if err := a.db.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))
		)`, blocker).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("leader assignment completed before the user lock released: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET is_active=false WHERE id=$1", user); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, admin.ErrNotGroupMember) {
		t.Fatalf("leader assignment should reject the now-inactive member: %v", err)
	}
}
