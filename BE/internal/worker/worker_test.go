package worker

import (
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
	"testing"
	"time"
)

func TestOptionsAndWorkerIdentity(t *testing.T) {
	for _, opts := range []Options{{Concurrency: -1}, {Concurrency: 17}, {JobTimeout: time.Millisecond}, {JobTimeout: 2 * time.Hour}} {
		if _, err := New(&jobs.Service{}, &runner.Service{}, opts); err == nil {
			t.Fatal("invalid worker options accepted")
		}
	}
	if _, err := New(nil, &runner.Service{}, Options{}); err == nil {
		t.Fatal("nil queue accepted")
	}
	first, err := New(&jobs.Service{}, &runner.Service{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(&jobs.Service{}, &runner.Service{}, Options{})
	if err != nil || first.id == second.id || len(first.id) != 32 || first.options.Concurrency != 2 || first.options.JobTimeout != 15*time.Minute {
		t.Fatal("unsafe worker defaults or identity")
	}
}
