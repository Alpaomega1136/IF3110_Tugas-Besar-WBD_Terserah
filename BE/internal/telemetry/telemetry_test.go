package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPTracePropagatesWithoutSensitiveInput(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := trace.NewTracerProvider(trace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	const parent = "00-12345678901234567890123456789012-1234567890123456-01"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := Start(r.Context(), "migration.enqueue")
		defer span.End()
		if !strings.Contains(Parent(ctx), "12345678901234567890123456789012") {
			t.Error("trace continuity lost")
		}
		w.WriteHeader(202)
	})
	req := httptest.NewRequest("POST", "/jobs/1?password=secret-marker", strings.NewReader("private-sql-marker"))
	req.Header.Set("traceparent", parent)
	req.Header.Set("Cookie", "session=private-cookie-marker")
	HTTP(mux).ServeHTTP(httptest.NewRecorder(), req)
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].SpanContext.TraceID() != spans[1].SpanContext.TraceID() || spans[0].Parent.SpanID() != spans[1].SpanContext.SpanID() {
		t.Fatal("request-to-queue trace was not linked")
	}
	for _, span := range spans {
		data := span.Name
		for _, attr := range span.Attributes {
			data += attr.Value.AsString()
		}
		if strings.Contains(data, "marker") || strings.Contains(data, "password") {
			t.Fatal("trace leaked request data")
		}
	}
	if spans[1].Name != "POST /jobs/{id}" {
		t.Fatalf("route pattern missing: %s", spans[1].Name)
	}
}

func TestOTLPExporterFlushesOnShutdown(t *testing.T) {
	received := make(chan int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("unexpected OTLP request")
		}
		received <- len(data)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
	previous := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	previousHandler := otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		otel.SetTextMapPropagator(previousPropagator)
		otel.SetErrorHandler(previousHandler)
	})
	shutdown, err := Init(context.Background(), "test-worker")
	if err != nil {
		t.Fatal(err)
	}
	_, span := Start(context.Background(), "migration.worker")
	span.SetAttributes(attribute.String("execution.id", "test-execution"))
	span.End()
	if err = shutdown(context.Background()); err != nil {
		t.Fatal("OTLP shutdown did not flush", err)
	}
	select {
	case n := <-received:
		if n == 0 {
			t.Fatal("empty OTLP trace")
		}
	default:
		t.Fatal("OTLP collector received no spans")
	}
}
