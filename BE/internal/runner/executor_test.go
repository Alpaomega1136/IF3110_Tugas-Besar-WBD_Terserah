package runner

import (
	"context"
	"errors"
	"net/http/httptest"
	"terserah/backend/internal/jobs"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"terserah/backend/internal/connections"
)

func TestExecutionErrorsExposeOnlySafeCodes(t *testing.T) {
	for _, entry := range []struct {
		err  error
		code string
	}{
		{errors.New("password=private-credential-marker; SQL=private-query-marker"), "execution_failed"},
		{&pgconn.PgError{Code: "23505", Message: "private-query-marker"}, "23505"},
		{context.DeadlineExceeded, "execution_timeout"},
		{connections.ErrTargetUnavailable, "target_unavailable"},
		{connections.ErrMetadataTarget, "metadata_target_forbidden"},
		{connections.ErrForbidden, "access_revoked"},
	} {
		if got := executionCode(entry.err); got != entry.code {
			t.Fatalf("unsafe or incorrect execution code: %s", got)
		}
	}
}

func TestQueueErrorsPreserveExecutionHTTPContract(t *testing.T) {
	for _, entry := range []struct {
		err    error
		status int
	}{{jobs.ErrForbidden, 403}, {jobs.ErrNotFound, 404}, {jobs.ErrConflict, 409}, {jobs.ErrInvalid, 400}} {
		w := httptest.NewRecorder()
		fail(w, entry.err)
		if w.Code != entry.status {
			t.Fatalf("queue error HTTP status=%d want=%d", w.Code, entry.status)
		}
	}
}
