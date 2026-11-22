package approval

import (
	"context"
	"errors"
	"strings"
	"time"

	"terserah/backend/internal/policy"
)

var (
	ErrNotFound         = errors.New("review or draft not found")
	ErrForbidden        = errors.New("access denied")
	ErrInvalidInput     = errors.New("invalid review input")
	ErrRevisionConflict = errors.New("draft revision mismatch or active review already exists")
	ErrLeaderMissing    = errors.New("every group with access to this database must have an active leader")
	ErrNotApprover      = errors.New("only the assigned leader of an accessing group can decide on this review")
	ErrReviewClosed     = errors.New("review is no longer pending")
	ErrNotApproved      = errors.New("migration change has not been approved by all accessing group leaders")
)

const (
	StatusPendingReview = "pending_review"
	StatusApproved      = "approved"
	StatusRejected      = "rejected"
	StatusQueued        = "queued"
	StatusCancelled     = "cancelled"

	DecisionPending  = "pending"
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

func ValidActionType(action string) bool {
	switch action {
	case "up", "rollback", "forward", "resume", "compensate":
		return true
	default:
		return false
	}
}

type GroupApprovalView struct {
	GroupID        int64         `json:"group_id,string"`
	GroupName      string        `json:"group_name"`
	AccessLevel    policy.Access `json:"access_level"`
	LeaderUserID   int64         `json:"leader_user_id,string"`
	LeaderUsername string        `json:"leader_username"`
	Decision       string        `json:"decision"`
	Note           *string       `json:"note,omitempty"`
	DecidedBy      *int64        `json:"decided_by,string,omitempty"`
	DecidedAt      *time.Time    `json:"decided_at,omitempty"`
}

type ReviewView struct {
	ID                  int64               `json:"id,string"`
	DatabaseID          int64               `json:"database_id,string"`
	DatabaseName        string              `json:"database_name"`
	DraftID             int64               `json:"draft_id,string"`
	DraftTitle          string              `json:"draft_title"`
	DraftRevision       int64               `json:"draft_revision,string"`
	ActionType          string              `json:"action_type"`
	TargetExecutionID   *string             `json:"target_execution_id,omitempty"`
	SQLSnapshot         string              `json:"sql_snapshot,omitempty"`
	Status              string              `json:"status"`
	RequestedBy         int64               `json:"requested_by,string"`
	RequestedByUsername string              `json:"requested_by_username"`
	RejectionReason     *string             `json:"rejection_reason,omitempty"`
	CreatedAt           time.Time           `json:"created_at"`
	DecidedAt           *time.Time          `json:"decided_at,omitempty"`
	Approvals           []GroupApprovalView `json:"approvals"`
	CanCurrentUserAct   bool                `json:"can_current_user_act"`
}

type SubmitInput struct {
	DraftID           int64
	Revision          int64
	ActionType        string
	TargetExecutionID *string
}

// EvaluateCompleteness memeriksa persetujuan seluruh group yang saat ini
// memiliki akses view atau edit terhadap database target.
func EvaluateCompleteness(requiredGroupIDs []int64, approvals []GroupApprovalView) (allApproved bool, anyRejected bool) {
	if len(requiredGroupIDs) == 0 {
		return false, false
	}
	// Keputusan group yang aksesnya telah dicabut tidak lagi memengaruhi review.
	required := make(map[int64]bool, len(requiredGroupIDs))
	for _, gid := range requiredGroupIDs {
		required[gid] = true
	}
	byGroup := make(map[int64]GroupApprovalView, len(approvals))
	for _, a := range approvals {
		if !required[a.GroupID] {
			continue
		}
		if a.Decision == DecisionRejected {
			return false, true
		}
		byGroup[a.GroupID] = a
	}
	for _, gid := range requiredGroupIDs {
		entry, ok := byGroup[gid]
		if !ok || entry.LeaderUserID <= 0 || entry.Decision != DecisionApproved {
			return false, false
		}
	}
	return true, false
}

func ValidateNote(note string) (string, error) {
	trimmed := strings.TrimSpace(note)
	if len(trimmed) > 500 || strings.ContainsRune(trimmed, 0) {
		return "", ErrInvalidInput
	}
	return trimmed, nil
}

type Store interface {
	SubmitReview(ctx context.Context, actorID, dbID int64, input SubmitInput, now time.Time) (ReviewView, error)
	ListDatabaseReviews(ctx context.Context, actorID, dbID, beforeID int64, limit int) ([]ReviewView, bool, *string, error)
	ListAssignedReviews(ctx context.Context, actorID int64) ([]ReviewView, error)
	GetReview(ctx context.Context, actorID, dbID, reviewID int64) (ReviewView, error)
	DecideReview(ctx context.Context, actorID, dbID, reviewID int64, approve bool, note string, now time.Time) (ReviewView, error)
	EnsureApproved(ctx context.Context, dbID, draftID, revision int64, actionType string) (ReviewView, error)
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: store, now: now}
}

func (s *Service) EnsureApproved(ctx context.Context, dbID, draftID, revision int64, actionType string) (ReviewView, error) {
	if actionType == "" {
		actionType = "up"
	}
	return s.store.EnsureApproved(ctx, dbID, draftID, revision, actionType)
}
