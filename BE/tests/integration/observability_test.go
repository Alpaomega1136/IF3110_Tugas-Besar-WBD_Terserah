//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"terserah/backend/internal/database"
	"terserah/backend/internal/jobs"
	"terserah/backend/internal/telemetry"
)

func TestObservabilityCoversReadOnlyQueueWorkerBatchAndFailures(t *testing.T) {
	var output bytes.Buffer
	previousLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLog) })
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	f := newQueueFixture(t, telemetry.SQLTracer{Role: "metadata"})
	admin := f.a.browser()
	admin.login(t, "admin")
	var groupID int64
	if err := f.a.db.QueryRow(context.Background(), "SELECT id FROM groups WHERE leader_user_id=$1", f.a.users["viewer"]).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	status(t, admin.request("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/leader/%d", groupID, f.a.users["viewer"]), nil), 200)
	status(t, f.viewer.request("GET", fmt.Sprintf("/api/v1/databases/%d/schemas", f.first), nil), 200)
	j := queuedJob(t, f, f.first, "SELECT 'private-sql-marker'; SELECT 1/0")
	if _, err := queueWorker(t, f).ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertJob(t, f, j, jobs.Failed, "22012")
	db, schema := batchTarget(t)
	relation := pgx.Identifier{schema, "items"}.Sanitize()
	if _, err := db.Exec(context.Background(), "CREATE TABLE "+relation+"(id bigint PRIMARY KEY,value int); INSERT INTO "+relation+" SELECT n,0 FROM generate_series(1,5) n"); err != nil {
		t.Fatal(err)
	}
	b := enqueueBatch(t, f, batchConfig(schema))
	if _, err := queueWorker(t, f).ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertJob(t, f, b, jobs.Success, "")
	logged := output.String()
	for _, secret := range []string{"private-sql-marker", testPassword} {
		if strings.Contains(logged, secret) {
			t.Fatal("sensitive SQL or password leaked")
		}
	}
	for _, cookie := range f.editor.cookies {
		if cookie.Value != "" && strings.Contains(logged, cookie.Value) {
			t.Fatal("session identifier leaked")
		}
	}
	var read, failed, rollback, commit, batch, snapshot bool
	traceEvents := map[string]bool{"admin_group_leader_set": false, "migration_review_submitted": false, "migration_review_decided": false}
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal("non-JSON application log", err)
		}
		if name, ok := event["event"].(string); ok {
			if _, tracked := traceEvents[name]; tracked && event["trace_id"] != nil && event["span_id"] != nil {
				traceEvents[name] = true
			}
		}
		read = read || event["event"] == "sql_transaction_result" && event["command"] == "commit" && event["status"] == "success" && event["db_role"] == "target" && event["actor_id"] == float64(f.a.users["viewer"]) && event["trace_id"] != nil
		failed = failed || event["event"] == "sql_result" && event["error_code"] == "22012" && event["job_id"] == float64(j.ID) && event["execution_id"] != nil && event["attempt"] == float64(1)
		rollback = rollback || event["event"] == "sql_transaction_result" && event["db_role"] == "target" && event["command"] == "rollback" && event["job_id"] == float64(j.ID) && event["trace_id"] != nil
		commit = commit || event["event"] == "sql_transaction_result" && event["db_role"] == "target" && event["command"] == "commit" && event["status"] == "success" && event["batch"] != nil
		batch = batch || event["event"] == "sql_result" && event["db_role"] == "target" && event["batch"] != nil && event["row_count"] == float64(2) && event["job_id"] == float64(b.ID)
		snapshot = snapshot || event["event"] == "execution_started" && event["query_sha256"] != nil && event["migration_id"] != nil && event["revision"] == float64(1)
	}
	if !read || !failed || !rollback || !commit || !batch || !snapshot {
		t.Fatalf("missing coverage: read=%v failure=%v rollback=%v commit=%v batch=%v snapshot=%v", read, failed, rollback, commit, batch, snapshot)
	}
	for name, traced := range traceEvents {
		if !traced {
			t.Errorf("event %s has no trace or span ID", name)
		}
	}
	spans := exporter.GetSpans()
	var producer, consumer, lock, sql bool
	for _, span := range spans {
		switch span.Name {
		case "migration.enqueue":
			producer = true
		case "migration.worker":
			consumer = true
		case "migration.lock":
			lock = true
		case "db.query":
			sql = true
		}
	}
	if !producer || !consumer || !lock || !sql {
		t.Fatal("missing end-to-end spans")
	}
}

func TestCanceledRequestRollbackRetainsTraceAndReleasesLock(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	provider := sdktrace.NewTracerProvider()
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider); _ = provider.Shutdown(context.Background()) })
	db, err := database.Open(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("cannot open isolated metadata fixture")
	}
	defer db.Close()
	ctx, span := telemetry.Start(context.Background(), "test.request")
	defer span.End()
	ctx, cancel := context.WithCancel(telemetry.WithFields(ctx, "actor_id", int64(21)))
	defer cancel()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Rollback(ctx, tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(3110999)"); err != nil {
		t.Fatal(err)
	}
	cancel()
	telemetry.Rollback(ctx, tx)
	var released bool
	if err = db.QueryRow(context.Background(), "SELECT pg_try_advisory_xact_lock(3110999)").Scan(&released); err != nil || !released {
		t.Fatal("canceled request left a transaction lock", err)
	}
	var correlated bool
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event map[string]any
		if err = json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		correlated = correlated || event["event"] == "sql_transaction_result" && event["command"] == "rollback" && event["status"] == "success" && event["actor_id"] == float64(21) && event["trace_id"] == span.SpanContext().TraceID().String()
	}
	if !correlated {
		t.Fatal("rollback lost request trace or actor")
	}
}
