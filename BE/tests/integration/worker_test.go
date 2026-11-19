//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/runner"
	"terserah/backend/internal/worker"
)

func queueWorker(t *testing.T, f *queueFixture) *worker.Worker {
	t.Helper()
	w, err := worker.New(f.service, runner.New(f.a.db, f.a.targets, f.a.key), worker.Options{JobTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func queuedJob(t *testing.T, f *queueFixture, dbID int64, sql string) jobs.Job {
	t.Helper()
	review, _ := f.approved(t, dbID, sql)
	j, _, err := f.service.Enqueue(context.Background(), f.actor, dbID, review, "")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func assertJob(t *testing.T, f *queueFixture, j jobs.Job, state jobs.Status, code string) jobs.Attempt {
	t.Helper()
	current, attempts, err := f.service.Get(context.Background(), f.actor, j.DatabaseID, j.ID)
	if err != nil || current.Status != state || len(attempts) != 1 || attempts[0].Status != state || attempts[0].Version == nil {
		t.Fatalf("inconsistent job result: job=%+v attempts=%+v err=%v", current, attempts, err)
	}
	if code != "" && (current.ErrorCode == nil || *current.ErrorCode != code) {
		t.Fatalf("unexpected outcome code: %+v", current.ErrorCode)
	}
	var historyState string
	if err = f.a.db.QueryRow(context.Background(), `SELECT status FROM migration_history WHERE execution_id=$1`, attempts[0].ExecutionID).Scan(&historyState); err != nil || historyState != string(state) {
		t.Fatal("job and history differ", historyState, err)
	}
	return attempts[0]
}

func TestAsyncExecuteSurvivesRequestCancellation(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	f := newQueueFixture(t)
	_, draft := f.approved(t, f.first, "SELECT pg_sleep(1.5)")
	ctx, cancel := context.WithCancel(context.Background())
	path := fmt.Sprintf("/api/v1/databases/%d/drafts/%d/execute", f.first, draft.ID)
	started := time.Now()
	w := f.editor.requestContext(ctx, "POST", path, map[string]string{"revision": "1"})
	status(t, w, 202)
	cancel()
	if time.Since(started) > time.Second {
		t.Fatal("API waited for SQL")
	}
	var response struct {
		Job jobs.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Job.Status != jobs.Queued {
		t.Fatal("missing queued job")
	}
	var count int
	if err := f.a.db.QueryRow(context.Background(), `SELECT count(*) FROM migration_history`).Scan(&count); err != nil || count != 0 {
		t.Fatal("HTTP handler executed SQL")
	}
	background := queueWorker(t, f)
	if found, err := background.ProcessNext(context.Background()); err != nil || !found {
		t.Fatal("worker did not execute", err)
	}
	attempt := assertJob(t, f, response.Job, jobs.Success, "")
	target, err := database.Open(context.Background(), os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = target.QueryRow(context.Background(), `SELECT count(*) FROM _terserah.migration_receipts WHERE execution_id=$1`, attempt.ExecutionID).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing committed receipt", err)
	}
	status(t, f.editor.request("POST", path, map[string]string{"revision": "1"}), 200)
	if found, err := background.ProcessNext(context.Background()); found || err != nil {
		t.Fatal("replay executed again", err)
	}
	var producer, consumer, query *tracetest.SpanStub
	spans := exporter.GetSpans()
	for i := range spans {
		switch spans[i].Name {
		case "migration.enqueue":
			if producer == nil {
				producer = &spans[i]
			}
		case "migration.worker":
			consumer = &spans[i]
		case "migration.sql":
			query = &spans[i]
		}
	}
	if producer == nil || consumer == nil || query == nil || producer.SpanContext.TraceID() != consumer.SpanContext.TraceID() || consumer.Parent.SpanID() != producer.SpanContext.SpanID() || query.Parent.SpanID() != consumer.SpanContext.SpanID() {
		t.Fatal("API/queue/worker/SQL trace continuity lost")
	}
}

func TestWorkerRevalidatesQueuedRequests(t *testing.T) {
	for _, scenario := range []string{"grant", "actor", "leader", "revision"} {
		t.Run(scenario, func(t *testing.T) {
			f := newQueueFixture(t)
			j := queuedJob(t, f, f.first, "SELECT 1")
			ctx := context.Background()
			var err error
			switch scenario {
			case "grant":
				_, err = f.a.db.Exec(ctx, `UPDATE group_database_permissions SET access_level='view' WHERE database_id=$1 AND group_id IN (SELECT group_id FROM user_groups WHERE user_id=$2)`, f.first, f.actor.ID)
			case "actor":
				_, err = f.a.db.Exec(ctx, `UPDATE users SET is_active=false WHERE id=$1`, f.actor.ID)
			case "leader":
				_, err = f.a.db.Exec(ctx, `UPDATE groups SET leader_user_id=$1 WHERE id IN (SELECT group_id FROM user_groups WHERE user_id=$2)`, f.a.users["outsider"], f.a.users["viewer"])
			case "revision":
				_, err = f.a.db.Exec(ctx, `UPDATE migration_drafts SET revision=revision+1,sql='SELECT 2' WHERE id=$1`, j.DraftID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if found, err := queueWorker(t, f).ProcessNext(ctx); err != nil || !found {
				t.Fatal("worker did not classify rejection", err)
			}
			// Restore only fixture identity/access to inspect the recorded failure.
			if _, err = f.a.db.Exec(ctx, `UPDATE users SET is_active=true WHERE id=$1`, f.actor.ID); err != nil {
				t.Fatal(err)
			}
			code := "access_revoked"
			if scenario == "leader" {
				code = "approval_incomplete"
			}
			if scenario == "revision" {
				code = "snapshot_changed"
			}
			assertJob(t, f, j, jobs.Failed, code)
		})
	}
}

func TestWorkersRunDifferentTargetsAndSerializeSameTarget(t *testing.T) {
	f := newQueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	barriers := []pgx.Tx{}
	for _, name := range []string{"TEST_TARGET_DATABASE_URL", "TEST_SECOND_TARGET_DATABASE_URL"} {
		conn, err := pgx.Connect(ctx, os.Getenv(name))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(3110199)`); err != nil {
			t.Fatal(err)
		}
		barriers = append(barriers, tx)
	}
	first := queuedJob(t, f, f.first, "SELECT pg_advisory_xact_lock(3110199)")
	next := queuedJob(t, f, f.first, "SELECT 2")
	second := queuedJob(t, f, f.second, "SELECT pg_advisory_xact_lock(3110199)")
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		background := queueWorker(t, f)
		go func() {
			found, err := background.ProcessNext(ctx)
			if !found && err == nil {
				err = fmt.Errorf("missing dispatch")
			}
			done <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := f.a.db.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE status='running'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("different targets were not dispatched concurrently")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if found, err := queueWorker(t, f).ProcessNext(ctx); err != nil || found {
		t.Fatal("same target dispatched while active", err)
	}
	for _, tx := range barriers {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	assertJob(t, f, first, jobs.Success, "")
	assertJob(t, f, second, jobs.Success, "")
	if found, err := queueWorker(t, f).ProcessNext(ctx); err != nil || !found {
		t.Fatal("successor did not run", err)
	}
	assertJob(t, f, next, jobs.Success, "")
}

func TestWorkerShutdownRollsBackAndStops(t *testing.T) {
	f := newQueueFixture(t)
	j := queuedJob(t, f, f.first, "SELECT pg_sleep(10)")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); queueWorker(t, f).Run(ctx) }()
	deadline := time.Now().Add(4 * time.Second)
	for {
		var running bool
		if err := f.a.db.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM migration_history WHERE execution_id IN (SELECT execution_id FROM migration_job_attempts WHERE job_id=$1))`, j.ID).Scan(&running); err != nil {
			t.Fatal(err)
		}
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker shutdown did not finish")
	}
	assertJob(t, f, j, jobs.Failed, "execution_timeout")
}

func TestJobFinalizationHonorsConcurrentVerifiedRecovery(t *testing.T) {
	f := newQueueFixture(t)
	input := targetInput(t, "TEST_TARGET_DATABASE_URL", "fault")
	port := commitProxy(t, net.JoinHostPort(input.Host, strconv.Itoa(input.Port)), true, true)
	if _, err := f.a.db.Exec(context.Background(), `UPDATE database_connections SET port=$1 WHERE id=$2`, port, f.first); err != nil {
		t.Fatal(err)
	}
	j := queuedJob(t, f, f.first, "SELECT 1")
	c, err := f.service.Claim(context.Background(), "recovery-race", 30*time.Second)
	if err != nil || c == nil {
		t.Fatal("claim failed", err)
	}
	result, err := runner.New(f.a.db, f.a.targets, f.a.key).ExecuteJob(context.Background(), c)
	if err != nil || result.Status != "unknown" {
		t.Fatal("lost COMMIT response was not unknown", result, err)
	}
	// Recovery proves success before worker finalization while the job is still running.
	w := f.editor.request("POST", fmt.Sprintf("/api/v1/databases/%d/migrations/reconcile", f.first), nil)
	status(t, w, 200)
	if err = f.service.Finish(context.Background(), c, jobs.Outcome{Status: jobs.Unknown, ErrorCode: "commit_unknown"}); err != nil {
		t.Fatal(err)
	}
	assertJob(t, f, j, jobs.Success, "")
}

func TestWorkerMaintainsLeaseForLongSQL(t *testing.T) {
	f := newQueueFixture(t)
	j := queuedJob(t, f, f.first, "SELECT pg_sleep(11.5)")
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 17*time.Second)
	defer cancel()
	go func() { _, err := queueWorker(t, f).ProcessNext(ctx); done <- err }()
	var initial time.Time
	deadline := time.Now().Add(3 * time.Second)
	for initial.IsZero() {
		var lease *time.Time
		if err := f.a.db.QueryRow(ctx, `SELECT lease_expires_at FROM migration_jobs WHERE id=$1`, j.ID).Scan(&lease); err != nil {
			t.Fatal(err)
		}
		if lease != nil {
			initial = *lease
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not claim")
		}
		time.Sleep(10 * time.Millisecond)
	}
	renewed := false
	for !renewed {
		var lease *time.Time
		if err := f.a.db.QueryRow(ctx, `SELECT lease_expires_at FROM migration_jobs WHERE id=$1`, j.ID).Scan(&lease); err != nil {
			t.Fatal(err)
		}
		renewed = lease != nil && lease.Sub(initial) > 5*time.Second
		if time.Now().After(initial.Add(-15 * time.Second)) {
			t.Fatal("periodic heartbeat did not renew lease")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertJob(t, f, j, jobs.Success, "")
}

func TestLostLeaseCannotCommitOrUnblockTarget(t *testing.T) {
	f := newQueueFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schema := fmt.Sprintf("lease_%d", time.Now().UnixNano())
	j := queuedJob(t, f, f.first, "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".proof(id int); SELECT pg_sleep(1)")
	done := make(chan error, 1)
	go func() { _, err := queueWorker(t, f).ProcessNext(ctx); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var running bool
		if err := f.a.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_history WHERE database_id=$1 AND status='running')`, f.first).Scan(&running); err != nil {
			t.Fatal(err)
		}
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.a.db.Exec(ctx, `UPDATE migration_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("expired worker finalized job", err)
	}
	target, err := database.Open(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var absent bool
	if err = target.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, schema+".proof").Scan(&absent); err != nil || !absent {
		t.Fatal("lease loss committed SQL", err)
	}
	if found, err := queueWorker(t, f).ProcessNext(ctx); found || err != nil {
		t.Fatal("expired attempt was automatically retried", err)
	}
	var state string
	if err = f.a.db.QueryRow(ctx, `SELECT status FROM migration_jobs WHERE id=$1`, j.ID).Scan(&state); err != nil || state != "running" {
		t.Fatal("unresolved lease stopped blocking target", err)
	}
}
