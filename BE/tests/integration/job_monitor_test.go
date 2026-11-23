//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"terserah/backend/internal/jobs"
)

func TestJobMonitorExposesExactResumeReviewWithoutSQL(t *testing.T) {
	f, _, _, parent, original := partialResumeFixture(t)
	path := fmt.Sprintf("/api/v1/databases/%d/jobs/%d", f.first, parent.ID)
	read := func() jobs.Job {
		w := f.viewer.request("GET", path, nil)
		status(t, w, 200)
		if strings.Contains(w.Body.String(), "terserah-batch-v1") || strings.Contains(w.Body.String(), "sql_snapshot") {
			t.Fatal("job monitor leaked SQL")
		}
		var out struct {
			Job jobs.Job `json:"job"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Job
	}
	if read().ResumeReview != nil {
		t.Fatal("unexpected resume review before submission")
	}
	r := submitResumeReview(t, f, f.editor, parent, original.ExecutionID)
	current := read()
	if current.ResumeReview == nil || current.ResumeReview.ID != r.ID || current.ResumeReview.Status != "pending_review" || current.ResumeReview.TargetExecutionID != original.ExecutionID {
		t.Fatal("monitor lost pending resume target")
	}
	approveResumeReview(t, f, r)
	if current = read(); current.ResumeReview == nil || current.ResumeReview.Status != "approved" {
		t.Fatal("monitor did not expose approved resume")
	}
	child := enqueueApprovedResume(t, f, f.editor, r)
	if current = read(); current.Status != jobs.Resumed || current.ResumedByJobID == nil || *current.ResumedByJobID != child.ID || current.ResumeReview != nil {
		t.Fatal("monitor lost handoff to child")
	}
	status(t, f.viewer.request("GET", fmt.Sprintf("/api/v1/databases/%d/jobs/%d", f.second, parent.ID), nil), 403)
}
