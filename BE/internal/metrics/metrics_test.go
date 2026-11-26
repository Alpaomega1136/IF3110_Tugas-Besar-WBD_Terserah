package metrics

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistogramCumulativeBuckets(t *testing.T) {
	var h histogram
	h.observe(5 * time.Millisecond)
	h.observe(2 * time.Second)
	h.observe(1000 * time.Second)
	if h.count != 3 || h.counts[0] != 0 || h.counts[1] != 1 || h.counts[9] != 2 || h.counts[len(bounds)-1] != 2 {
		t.Fatalf("invalid histogram: %+v", h)
	}
	var b bytes.Buffer
	writeHistogram(&b, "test_seconds", `kind="test"`, h)
	for _, s := range []string{`test_seconds_bucket{kind="test",le="+Inf"} 3`, `test_seconds_count{kind="test"} 3`} {
		if !strings.Contains(b.String(), s) {
			t.Fatalf("missing %s", s)
		}
	}
}

func TestActivityBoundedLabelsAndConcurrentUpdates(t *testing.T) {
	mu.Lock()
	before := queueWait[0].count
	unknown := duration[4].count
	failed := lockWait[0][1].count
	b0, r0 := batches[0], rows[0]
	mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ObserveQueue(false, time.Second); BatchCommitted(false, 5) }()
	}
	wg.Wait()
	ObserveDuration("unexpected-sensitive-value", time.Second)
	ObserveLock("session", errors.New("unexpected-sensitive-value"), time.Millisecond)
	ObserveLock("unexpected-sensitive-value", nil, time.Millisecond)
	var b bytes.Buffer
	writeActivity(&b)
	out := b.String()
	for _, s := range []string{
		fmt.Sprintf(`terserah_migration_queue_wait_seconds_count{attempt="initial"} %d`, before+20),
		fmt.Sprintf(`terserah_migration_duration_seconds_count{outcome="unknown"} %d`, unknown+1),
		fmt.Sprintf(`terserah_migration_lock_wait_seconds_count{scope="session",result="failed"} %d`, failed+1),
		fmt.Sprintf(`terserah_migration_batches_total{kind="migration"} %d`, b0+20),
		fmt.Sprintf(`terserah_migration_rows_total{kind="migration"} %d`, r0+100),
	} {
		if !strings.Contains(out, s+"\n") {
			t.Errorf("missing %s", s)
		}
	}
	if strings.Contains(out, "unexpected-sensitive-value") {
		t.Fatal("unbounded input leaked into metric labels")
	}
}
