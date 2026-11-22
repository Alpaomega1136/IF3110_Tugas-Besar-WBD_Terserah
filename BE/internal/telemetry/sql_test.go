package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRedactSQL(t *testing.T) {
	for _, query := range []string{
		`SELECT 'private-marker', 123456, $1`,
		`SELECT E'private-marker\n', B'10101', X'affe'`,
		`SELECT $$private-marker$$, $tag9$private-marker$tag9$`,
		`SELECT "private-marker", 'it''s private-marker'`,
		"SELECT 1 -- private-marker\n /* outer /* private-marker */ inner */",
		`SELECT 'private-marker`, `SELECT $tag$private-marker`,
		`SELECT /* private-marker`, `SELECT U&'private-marker'`,
		`SELECT 'a\' || 'private-marker'`,
		"SELECT 'private-marker' \x00", `SELECT "private-marker\\"`,
	} {
		if got := RedactSQL(query); strings.Contains(got, "private-marker") || strings.Contains(got, "123456") {
			t.Fatalf("sensitive literal leaked: %q", got)
		}
	}
	if got := RedactSQL(`SELECT id FROM items WHERE id=$1 AND name='test'`); got != `SELECT id FROM items WHERE id=$1 AND name=?` {
		t.Fatalf("query structure lost: %s", got)
	}
	if len(RedactSQL(strings.Repeat("SELECT id;", 10000))) > 8210 {
		t.Fatal("unbounded query log")
	}
}

func TestSQLLogsAndSpansAreCorrelatedAndSanitized(t *testing.T) {
	var output bytes.Buffer
	previousLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLog) })
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	ctx, parent := Start(context.Background(), "migration.worker")
	ctx = WithFields(ctx, "actor_id", int64(4), "job_id", int64(7), "execution_id", "public-execution", "attempt", 2)
	tracer := SQLTracer{Role: "target", DatabaseID: 9}
	query := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT 'private-sql-marker' WHERE id=$1", Args: []any{"private-param-marker"}})
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 3")})
	query = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "COMMIT"})
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: errors.New("private-driver-marker")})
	copyCtx := tracer.TraceCopyFromStart(ctx, nil, pgx.TraceCopyFromStartData{TableName: pgx.Identifier{"private-table-marker"}, ColumnNames: []string{"value"}})
	tracer.TraceCopyFromEnd(copyCtx, nil, pgx.TraceCopyFromEndData{CommandTag: pgconn.NewCommandTag("COPY 2")})
	parent.End()
	if strings.Contains(output.String(), "private-") {
		t.Fatal("log leaked SQL, parameter, table or driver error")
	}
	var start, result, uncertain, copied bool
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event["trace_id"] != parent.SpanContext().TraceID().String() || event["actor_id"] != float64(4) || event["job_id"] != float64(7) || event["database_id"] != float64(9) || event["attempt"] != float64(2) {
			t.Fatal("missing query correlation", event)
		}
		start = start || event["event"] == "sql_started"
		result = result || event["event"] == "sql_result" && event["row_count"] == float64(3) && event["status"] == "success"
		copied = copied || event["event"] == "sql_result" && event["row_count"] == float64(2)
		uncertain = uncertain || event["event"] == "sql_transaction_result" && event["status"] == "unknown"
	}
	if !start || !result || !uncertain || !copied {
		t.Fatal("missing SQL start/result/uncertain COMMIT/COPY event")
	}
	spans := exporter.GetSpans()
	if len(spans) != 4 {
		t.Fatal("missing query spans", len(spans))
	}
	for _, span := range spans[:3] {
		if span.Parent.SpanID() != parent.SpanContext().SpanID() {
			t.Fatal("SQL span disconnected")
		}
	}
}

func TestSQLCodeDoesNotExposeDriverMessages(t *testing.T) {
	if SQLErrorCode(&pgconn.PgError{Code: "23505", Message: "private"}) != "23505" || SQLErrorCode(&pgconn.PgError{Code: "leak!"}) != "database_error" || SQLErrorCode(context.DeadlineExceeded) != "timeout" {
		t.Fatal("unsafe SQL error code")
	}
}
