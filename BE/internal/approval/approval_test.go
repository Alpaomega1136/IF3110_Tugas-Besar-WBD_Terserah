package approval

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubReviewStore struct {
	submitErr error
	decideErr error
	review    ReviewView
}

func (s stubReviewStore) SubmitReview(context.Context, int64, int64, SubmitInput, time.Time) (ReviewView, error) {
	return s.review, s.submitErr
}
func (s stubReviewStore) ListDatabaseReviews(context.Context, int64, int64, int64, int) ([]ReviewView, bool, *string, error) {
	return []ReviewView{s.review}, false, nil, nil
}
func (s stubReviewStore) ListAssignedReviews(context.Context, int64) ([]ReviewView, error) {
	return []ReviewView{s.review}, nil
}
func (s stubReviewStore) GetReview(context.Context, int64, int64, int64) (ReviewView, error) {
	return s.review, nil
}
func (s stubReviewStore) DecideReview(context.Context, int64, int64, int64, bool, string, time.Time) (ReviewView, error) {
	return s.review, s.decideErr
}
func (s stubReviewStore) EnsureApproved(context.Context, int64, int64, int64, string) (ReviewView, error) {
	return s.review, nil
}

func TestEvaluateCompleteness(t *testing.T) {
	if all, rejected := EvaluateCompleteness(nil, nil); all || rejected {
		t.Fatal("empty required groups must not be approved")
	}

	approvals := []GroupApprovalView{
		{GroupID: 10, LeaderUserID: 2, Decision: DecisionApproved},
		{GroupID: 20, LeaderUserID: 3, Decision: DecisionPending},
	}
	if all, rejected := EvaluateCompleteness([]int64{10, 20}, approvals); all || rejected {
		t.Fatal("partial approval must remain pending")
	}

	approvals[1].Decision = DecisionApproved
	if all, rejected := EvaluateCompleteness([]int64{10, 20}, approvals); !all || rejected {
		t.Fatal("all required groups approved should return allApproved=true")
	}

	if all, rejected := EvaluateCompleteness([]int64{10, 20, 30}, approvals); all || rejected {
		t.Fatal("missing newly granted group must not be treated as complete")
	}

	approvals[1].Decision = DecisionRejected
	if all, rejected := EvaluateCompleteness([]int64{10, 20}, approvals); all || !rejected {
		t.Fatal("any rejected group must return anyRejected=true")
	}
}

func TestReviewHTTPHandlers(t *testing.T) {
	missingLeaderSvc := NewService(stubReviewStore{submitErr: ErrLeaderMissing}, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/databases/5/reviews", strings.NewReader(`{"draft_id":"2","revision":"1"}`))
	r.SetPathValue("id", "5")
	missingLeaderSvc.HandleSubmitReview(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "group_leader_required") {
		t.Fatalf("expected 409 group_leader_required, got %d %s", w.Code, w.Body.String())
	}

	nonLeaderSvc := NewService(stubReviewStore{decideErr: ErrNotApprover}, nil)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/v1/databases/5/reviews/9/approve", strings.NewReader(`{"note":"LGTM"}`))
	r.SetPathValue("id", "5")
	r.SetPathValue("reviewID", "9")
	nonLeaderSvc.HandleApproveReview(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "not_group_leader") {
		t.Fatalf("expected 403 not_group_leader, got %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/api/v1/databases/5/reviews/9/reject", strings.NewReader(`{"note":"   "}`))
	r.SetPathValue("id", "5")
	r.SetPathValue("reviewID", "9")
	nonLeaderSvc.HandleRejectReview(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_input") {
		t.Fatalf("expected 400 when rejection reason is empty, got %d %s", w.Code, w.Body.String())
	}
}
