// Package jobs persists approved migration requests independently of HTTP requests.
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid     = errors.New("invalid job input")
	ErrForbidden   = errors.New("database access denied")
	ErrNotFound    = errors.New("job or review not found")
	ErrConflict    = errors.New("review snapshot changed or execution already exists")
	ErrUnsupported = errors.New("this action or execution mode is not supported yet")
	ErrLeaseLost   = errors.New("worker no longer owns this attempt")
)

type Status string

const (
	Queued  Status = "queued"
	Running Status = "running"
	Success Status = "success"
	Failed  Status = "failed"
	Unknown Status = "unknown"
)

type Job struct {
	ID               int64      `json:"id,string"`
	DatabaseID       int64      `json:"database_id,string"`
	ReviewID         int64      `json:"review_id,string"`
	DraftID          int64      `json:"draft_id,string"`
	DraftRevision    int64      `json:"draft_revision,string"`
	ActionType       string     `json:"action_type"`
	ExecutionMode    string     `json:"execution_mode"`
	RequestedBy      int64      `json:"requested_by,string"`
	Status           Status     `json:"status"`
	AttemptCount     int        `json:"attempt_count"`
	MaxAttempts      int        `json:"max_attempts"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
	ErrorCode        *string    `json:"error_code"`
	CommittedBatches int64      `json:"committed_batches,string"`
	CommittedRows    int64      `json:"committed_rows,string"`
}

type Attempt struct {
	ExecutionID string     `json:"execution_id"`
	Version     *int64     `json:"version,string,omitempty"`
	Number      int        `json:"attempt_no"`
	Status      Status     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	ErrorCode   *string    `json:"error_code"`
}

// Claim is server-internal. SQL and fencing credentials never enter job API responses.
type Claim struct {
	Job         Job    `json:"-"`
	ExecutionID string `json:"-"`
	WorkerID    string `json:"-"`
	LeaseToken  string `json:"-"`
	SQL         string `json:"-"`
	TraceParent string `json:"-"`
}

type Outcome struct {
	Status    Status
	ErrorCode string
}

type Service struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Service { return &Service{db: db} }

var workerPattern = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,100}$`)
var codePattern = regexp.MustCompile(`^[a-zA-Z0-9_:-]{1,64}$`)
var parentPattern = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// Invalid trace headers must not turn a valid business request into an error.
func traceParent(value string) string {
	if !parentPattern.MatchString(value) || value[3:35] == strings.Repeat("0", 32) || value[36:52] == strings.Repeat("0", 16) {
		return ""
	}
	return value
}

func snapshotHash(j Job, sql string) string {
	data, _ := json.Marshal([]any{"terserah-job-v1", j.DatabaseID, j.ReviewID, j.DraftID, j.DraftRevision, j.ActionType, j.ExecutionMode, j.RequestedBy, sql})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func validLease(worker string, duration time.Duration) bool {
	return workerPattern.MatchString(worker) && duration >= time.Second && duration <= 5*time.Minute
}

func validOutcome(out Outcome) bool {
	if out.Status == Success {
		return out.ErrorCode == ""
	}
	return (out.Status == Failed || out.Status == Unknown) && codePattern.MatchString(out.ErrorCode)
}
