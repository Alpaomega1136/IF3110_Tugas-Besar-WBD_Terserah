//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
