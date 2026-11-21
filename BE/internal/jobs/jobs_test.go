package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"terserah/backend/internal/auth"
)

func TestSnapshotBindsApprovedExecution(t *testing.T) {
	j := Job{DatabaseID: 1, ReviewID: 2, DraftID: 3, DraftRevision: 4, ActionType: "up", ExecutionMode: "normal", RequestedBy: 5}
	hash := snapshotHash(j, "SELECT 1")
	changes := []func(*Job){
		func(j *Job) { j.DatabaseID++ }, func(j *Job) { j.ReviewID++ },
		func(j *Job) { j.DraftID++ }, func(j *Job) { j.DraftRevision++ },
		func(j *Job) { j.ActionType = "rollback" }, func(j *Job) { j.ExecutionMode = "batch" },
		func(j *Job) { j.RequestedBy++ },
	}
	for i, change := range changes {
		copy := j
		change(&copy)
		if snapshotHash(copy, "SELECT 1") == hash {
			t.Fatalf("execution field %d is not bound", i)
		}
	}
	if snapshotHash(j, "SELECT 2") == hash {
		t.Fatal("SQL was not bound")
	}
	j.Status, j.AttemptCount = Running, 1
	if snapshotHash(j, "SELECT 1") != hash {
		t.Fatal("job lifecycle must not change its approved payload")
	}
}

func TestResumeSnapshotBindsParentAndTargetExecution(t *testing.T) {
	parent, target := int64(10), "original-execution"
	j := Job{DatabaseID: 1, ReviewID: 2, DraftID: 3, DraftRevision: 4, ActionType: "resume", ExecutionMode: "batch", RequestedBy: 5, ParentJobID: &parent, TargetExecutionID: &target}
	hash := snapshotHash(j, "approved SQL")
	otherParent, otherTarget := int64(11), "different-execution"
	j.ParentJobID = &otherParent
	if hash == snapshotHash(j, "approved SQL") {
		t.Fatal("parent job not bound")
	}
	j.ParentJobID, j.TargetExecutionID = &parent, &otherTarget
	if hash == snapshotHash(j, "approved SQL") {
		t.Fatal("target execution not bound")
	}
	j.TargetExecutionID = &target
	j.Status, j.AttemptCount = Running, 1
	if hash != snapshotHash(j, "approved SQL") {
		t.Fatal("resume lifecycle changed snapshot identity")
	}
}

func TestTraceParentValidation(t *testing.T) {
	valid := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	if traceParent(valid) != valid {
		t.Fatal("valid trace context was discarded")
	}
	for _, invalid := range []string{"", "short", strings.ToUpper(valid), valid + "\r\nX-Injected: value",
		"00-00000000000000000000000000000000-0123456789abcdef-01",
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01"} {
		if traceParent(invalid) != "" {
			t.Fatal("invalid trace context was accepted")
		}
	}
}

func TestWorkerBoundaries(t *testing.T) {
	if !validLease("worker-1", time.Second) || !validLease("worker-1", 5*time.Minute) {
		t.Fatal("valid lease rejected")
	}
	for _, worker := range []string{"", "worker\nsecret", "worker one", strings.Repeat("a", 101)} {
		if validLease(worker, time.Minute) {
			t.Fatal("invalid worker ID accepted")
		}
	}
	for _, duration := range []time.Duration{0, -time.Second, time.Millisecond, 5*time.Minute + time.Millisecond} {
		if validLease("worker-1", duration) {
			t.Fatal("invalid lease duration accepted")
		}
	}
	for _, out := range []Outcome{{Success, ""}, {Failed, "23505"}, {Unknown, "commit_unknown"}} {
		if !validOutcome(out) {
			t.Fatal("valid outcome rejected")
		}
	}
	for _, out := range []Outcome{{Queued, ""}, {Running, ""}, {Success, "error"}, {Failed, ""}, {Unknown, ""}, {Failed, "raw connection password"}} {
		if validOutcome(out) {
			t.Fatal("invalid transition or unsafe error code accepted")
		}
	}
	service := New(nil)
	if _, err := service.Claim(context.Background(), "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid claim must fail before database access")
	}
	if err := service.Finish(context.Background(), nil, Outcome{Success, ""}); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil claim accepted")
	}
}

func TestJobJSONKeepsIDsAndSecretsSafe(t *testing.T) {
	j := Job{ID: 9007199254740993, CommittedRows: 9007199254740995}
	data, err := json.Marshal(j)
	if err != nil || !strings.Contains(string(data), `"id":"9007199254740993"`) || !strings.Contains(string(data), `"committed_rows":"9007199254740995"`) {
		t.Fatal("int64 JSON contract lost precision")
	}
	c := Claim{Job: j, SQL: "private SQL", LeaseToken: "private lease", TraceParent: "private trace"}
	data, err = json.Marshal(c)
	if err != nil || string(data) != "{}" {
		t.Fatal("internal claim can leak into a JSON response")
	}
	w := httptest.NewRecorder()
	respondError(w, errors.New("postgres://user:private-password@server"))
	if w.Code != 503 || strings.Contains(w.Body.String(), "private-password") {
		t.Fatal("driver failure leaked a credential")
	}
}

func TestJobRoutesRequireSession(t *testing.T) {
	sessions, err := auth.New(nil, auth.Options{Origin: "http://localhost:3000", Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(nil).Register(mux, sessions)
	for _, request := range []struct{ method, path string }{
		{"POST", "/api/v1/databases/1/jobs"}, {"GET", "/api/v1/databases/1/jobs"}, {"GET", "/api/v1/databases/1/jobs/2"},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(request.method, request.path, nil))
		if w.Code != 401 {
			t.Fatalf("unprotected route: %s %s", request.method, request.path)
		}
	}
}
