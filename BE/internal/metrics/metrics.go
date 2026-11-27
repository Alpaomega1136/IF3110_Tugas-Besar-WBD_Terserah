// Package metrics exposes bounded-cardinality application metrics in Prometheus text format.
package metrics

import (
	"bytes"
	"fmt"
	"sync"
	"time"
)

var bounds = [...]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 900}

type histogram struct {
	counts [len(bounds)]uint64
	count  uint64
	sum    float64
}

func (h *histogram) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	v := d.Seconds()
	h.count++
	h.sum += v
	for i, bound := range bounds {
		if v <= bound {
			h.counts[i]++
		}
	}
}

var (
	mu        sync.Mutex
	queueWait [2]histogram
	lockWait  [3][2]histogram
	duration  [5]histogram
	batches   [2]uint64
	rows      [2]uint64
)
var attemptKinds = [...]string{"initial", "retry"}
var lockScopes = [...]string{"session", "transaction", "batch"}
var lockResults = [...]string{"success", "failed"}
var outcomes = [...]string{"success", "failed", "partial", "partially_reverted", "unknown"}
var batchKinds = [...]string{"migration", "compensation"}

func ObserveQueue(retry bool, d time.Duration) {
	i := 0
	if retry {
		i = 1
	}
	mu.Lock()
	defer mu.Unlock()
	queueWait[i].observe(d)
}

func ObserveLock(scope string, err error, d time.Duration) {
	i := -1
	for k, s := range lockScopes {
		if scope == s {
			i = k
			break
		}
	}
	if i < 0 {
		return
	}
	j := 0
	if err != nil {
		j = 1
	}
	mu.Lock()
	defer mu.Unlock()
	lockWait[i][j].observe(d)
}

// ObserveDuration measures one executor attempt; unresolved attempts use unknown.
func ObserveDuration(outcome string, d time.Duration) {
	i := len(outcomes) - 1
	for k, s := range outcomes {
		if outcome == s {
			i = k
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	duration[i].observe(d)
}

func BatchCommitted(compensation bool, rowCount int64) {
	if rowCount < 0 {
		return
	}
	i := 0
	if compensation {
		i = 1
	}
	mu.Lock()
	defer mu.Unlock()
	batches[i]++
	rows[i] += uint64(rowCount)
}

func header(b *bytes.Buffer, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}
func writeHistogram(b *bytes.Buffer, name, labels string, h histogram) {
	for i, bound := range bounds {
		fmt.Fprintf(b, "%s_bucket{%s,le=\"%g\"} %d\n", name, labels, bound, h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{%s,le=\"+Inf\"} %d\n%s_sum{%s} %g\n%s_count{%s} %d\n", name, labels, h.count, name, labels, h.sum, name, labels, h.count)
}
func writeActivity(b *bytes.Buffer) {
	mu.Lock()
	q, l, d, bc, rc := queueWait, lockWait, duration, batches, rows
	mu.Unlock()
	name := "terserah_migration_queue_wait_seconds"
	header(b, name, "histogram", "Wait to successful claim; initial from creation, retry from eligibility.")
	for i, h := range q {
		writeHistogram(b, name, fmt.Sprintf("attempt=%q", attemptKinds[i]), h)
	}
	name = "terserah_migration_lock_wait_seconds"
	header(b, name, "histogram", "Elapsed time of explicit advisory lock commands, including failed acquisitions.")
	for i, byResult := range l {
		for j, h := range byResult {
			writeHistogram(b, name, fmt.Sprintf("scope=%q,result=%q", lockScopes[i], lockResults[j]), h)
		}
	}
	name = "terserah_migration_duration_seconds"
	header(b, name, "histogram", "Executor attempt duration including target connection and lock acquisition; excludes queue wait and recovery.")
	for i, h := range d {
		writeHistogram(b, name, fmt.Sprintf("outcome=%q", outcomes[i]), h)
	}
	header(b, "terserah_migration_batches_total", "counter", "New batch checkpoints with acknowledged target commits in this process.")
	for i, n := range bc {
		fmt.Fprintf(b, "terserah_migration_batches_total{kind=%q} %d\n", batchKinds[i], n)
	}
	header(b, "terserah_migration_rows_total", "counter", "Rows in newly acknowledged batch commits in this process.")
	for i, n := range rc {
		fmt.Fprintf(b, "terserah_migration_rows_total{kind=%q} %d\n", batchKinds[i], n)
	}
}
