package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Init exports sanitized spans to JSON logs unless an OTLP endpoint is configured.
func Init(ctx context.Context, service string) (func(context.Context) error, error) {
	var exporter sdktrace.SpanExporter = logExporter{}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		var err error
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithTimeout(3*time.Second))
		if err != nil {
			return nil, errors.New("cannot initialize trace exporter")
		}
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes("", attribute.String("service.name", service))))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { slog.Warn("trace_export_failed") }))
	return provider.Shutdown, nil
}

func Start(ctx context.Context, name string, options ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer("terserah/backend").Start(ctx, name, options...)
}

func Parent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

func Restore(ctx context.Context, parent string) context.Context {
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": parent})
}

func Event(ctx context.Context, event string, fields ...any) {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		fields = append(fields, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
	}
	slog.InfoContext(ctx, event, append([]any{"event", event}, fields...)...)
}

func Failure(span trace.Span, code string) {
	span.SetStatus(codes.Error, code)
	span.SetAttributes(attribute.String("error.code", code))
}

type logExporter struct{}

func (logExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, span := range spans {
		slog.InfoContext(ctx, "trace_span", "event", "trace_span", "name", span.Name(),
			"trace_id", span.SpanContext().TraceID().String(), "span_id", span.SpanContext().SpanID().String(),
			"parent_span_id", span.Parent().SpanID().String(), "duration_ms", span.EndTime().Sub(span.StartTime()).Milliseconds(),
			"status", span.Status().Code.String(), "attributes", span.Attributes())
	}
	return nil
}
func (logExporter) Shutdown(context.Context) error { return nil }

type responseStatus struct {
	http.ResponseWriter
	code int
}

func (w *responseStatus) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }

// HTTP deliberately excludes URL queries, headers, cookies, SQL and request bodies.
func HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := Start(Restore(r.Context(), r.Header.Get("traceparent")), "http.request", trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		r = r.WithContext(ctx)
		out := &responseStatus{ResponseWriter: w, code: 200}
		next.ServeHTTP(out, r)
		span.SetName(r.Pattern)
		span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("http.route", r.Pattern), attribute.Int("http.response.status_code", out.code))
		if out.code >= 500 {
			Failure(span, "http_server_error")
		}
	})
}
