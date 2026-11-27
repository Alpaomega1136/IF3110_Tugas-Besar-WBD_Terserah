package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/metrics"
	"terserah/backend/internal/runner"
	"terserah/backend/internal/telemetry"
)

type Options struct {
	Concurrency int
	JobTimeout  time.Duration
}

type Worker struct {
	queue    *jobs.Service
	executor *runner.Service
	id       string
	options  Options
}

func New(queue *jobs.Service, executor *runner.Service, options Options) (*Worker, error) {
	if options.Concurrency == 0 {
		options.Concurrency = 2
	}
	if options.JobTimeout == 0 {
		options.JobTimeout = 15 * time.Minute
	}
	if queue == nil || executor == nil || options.Concurrency < 1 || options.Concurrency > 16 || options.JobTimeout < time.Second || options.JobTimeout > time.Hour {
		return nil, errors.New("invalid worker configuration")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, errors.New("cannot initialize worker identity")
	}
	return &Worker{queue: queue, executor: executor, id: hex.EncodeToString(id[:]), options: options}, nil
}

// Run keeps recovery probes off the execution slots and waits for both on shutdown.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { poll(ctx, "recovery", w.RecoverNext) })
	for range w.options.Concurrency {
		wg.Go(func() { poll(ctx, "dispatch", w.ProcessNext) })
	}
	wg.Wait()
}

func poll(ctx context.Context, operation string, next func(context.Context) (bool, error)) {
	for ctx.Err() == nil {
		found, err := next(ctx)
		if err != nil && ctx.Err() == nil {
			telemetry.Event(ctx, "worker_"+operation+"_failed", "error_code", operation+"_unavailable")
		}
		if found && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (w *Worker) RecoverNext(ctx context.Context) (bool, error) {
	r, err := w.queue.RecoveryCandidate(ctx)
	if err != nil || r == nil {
		return false, err
	}
	ctx, span := telemetry.Start(telemetry.Restore(ctx, r.TraceParent), "migration.recovery", trace.WithSpanKind(trace.SpanKindConsumer), trace.WithAttributes(attribute.Int64("job.id", r.Job.ID), attribute.String("execution.id", r.ExecutionID)))
	defer span.End()
	ctx = telemetry.WithFields(ctx, "actor_id", r.Job.RequestedBy, "database_id", r.Job.DatabaseID, "job_id", r.Job.ID, "migration_id", r.Job.DraftID, "revision", r.Job.DraftRevision, "execution_id", r.ExecutionID, "attempt", r.Job.AttemptCount)
	ctx, cancel := context.WithTimeout(ctx, w.options.JobTimeout)
	defer cancel()
	err = w.executor.RecoverJob(ctx, r)
	if err != nil {
		telemetry.Failure(span, "recovery_unavailable")
	}
	return true, err
}

func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	c, err := w.queue.Claim(ctx, w.id, 30*time.Second)
	if err != nil || c == nil {
		return false, err
	}
	waitFrom := c.Job.CreatedAt
	if c.Job.AttemptCount > 1 {
		waitFrom = c.Job.AvailableAt
	}
	metrics.ObserveQueue(c.Job.AttemptCount > 1, time.Since(waitFrom))
	ctx, span := telemetry.Start(telemetry.Restore(ctx, c.TraceParent), "migration.worker", trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.Int64("job.id", c.Job.ID), attribute.Int64("database.id", c.Job.DatabaseID), attribute.String("execution.id", c.ExecutionID), attribute.Int64("queue.wait_ms", time.Since(c.Job.CreatedAt).Milliseconds())))
	defer span.End()
	ctx = telemetry.WithFields(ctx, "actor_id", c.Job.RequestedBy, "database_id", c.Job.DatabaseID, "job_id", c.Job.ID, "migration_id", c.Job.DraftID, "revision", c.Job.DraftRevision, "execution_id", c.ExecutionID, "attempt", c.Job.AttemptCount, "version", c.Version)
	jobCtx, cancel := context.WithTimeout(ctx, w.options.JobTimeout)
	defer cancel()
	heartbeatDone := make(chan struct{})
	stopHeartbeat := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				beatCtx, stop := context.WithTimeout(jobCtx, 3*time.Second)
				err := w.queue.Heartbeat(beatCtx, c, 30*time.Second)
				stop()
				if err != nil {
					telemetry.Event(ctx, "worker_lease_lost", "job_id", c.Job.ID)
					cancel()
					return
				}
			}
		}
	}()
	started := time.Now()
	result, err := w.executor.ExecuteJob(jobCtx, c)
	outcome := result.Status
	if err != nil {
		outcome = "unknown"
	}
	metrics.ObserveDuration(outcome, time.Since(started))
	close(stopHeartbeat)
	<-heartbeatDone
	if err != nil {
		telemetry.Failure(span, "execution_unresolved")
		return true, err
	}
	out := jobs.Outcome{Status: jobs.Status(result.Status)}
	if result.ErrorCode != nil {
		out.ErrorCode = *result.ErrorCode
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if err = w.queue.Finish(cleanup, c, out); err != nil {
		telemetry.Failure(span, "job_finalize_unavailable")
		return true, err
	}
	if out.Status != jobs.Success {
		telemetry.Failure(span, out.ErrorCode)
	}
	return true, nil
}
