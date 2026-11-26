package jobs

import (
	"testing"
	"time"
)

func TestRetryPolicyRequiresExplicitTransientCauses(t *testing.T) {
	for _, code := range []string{"40001", "40P01", "55P03", "57P01", "08006", "target_unavailable", "worker_stopped", "worker_interrupted"} {
		if !Transient(code) {
			t.Fatalf("transient cause rejected: %s", code)
		}
	}
	for _, code := range []string{"", "23505", "22012", "42601", "57014", "execution_timeout", "commit_unknown", "progress_unavailable", "receipt_mismatch", "batch_evidence_invalid", "access_revoked", "approval_incomplete"} {
		if Transient(code) {
			t.Fatalf("unverified or permanent cause retried: %s", code)
		}
	}
	for _, entry := range []struct {
		attempt int
		delay   time.Duration
	}{{0, time.Second}, {1, time.Second}, {2, 2 * time.Second}, {5, 16 * time.Second}, {6, 30 * time.Second}, {1000, 30 * time.Second}} {
		if RetryDelay(entry.attempt) != entry.delay {
			t.Fatal("unbounded or incorrect retry delay")
		}
	}
}
