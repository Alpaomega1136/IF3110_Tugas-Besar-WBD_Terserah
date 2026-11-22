package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SQLTracer covers metadata and target queries, including reads and transaction commands.
// Parameter values, returned data, connection strings and raw driver errors are never logged.
type SQLTracer struct {
	Role       string
	DatabaseID int64
}

type queryKey struct{}
type queryState struct {
	started time.Time
	span    trace.Span
	fields  []any
	command string
}

// Rollback keeps request correlation but must finish cleanup after request cancellation.
func Rollback(parent context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func (t SQLTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return t.start(ctx, data.SQL, len(data.Args))
}

func (t SQLTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	finishSQL(ctx, data.CommandTag, data.Err)
}

func (t SQLTracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	columns := make([]string, len(data.ColumnNames))
	for i, column := range data.ColumnNames {
		columns[i] = pgx.Identifier{column}.Sanitize()
	}
	return t.start(ctx, "COPY "+data.TableName.Sanitize()+" ("+strings.Join(columns, ",")+") FROM STDIN", 0)
}

func (t SQLTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	finishSQL(ctx, data.CommandTag, data.Err)
}

// BeginSQL also covers ExecParams, which deliberately bypasses pgx.QueryTracer.
func (t SQLTracer) BeginSQL(ctx context.Context, query string, parameterCount int) (context.Context, func(pgconn.CommandTag, error)) {
	ctx = t.start(ctx, query, parameterCount)
	return ctx, func(tag pgconn.CommandTag, err error) { finishSQL(ctx, tag, err) }
}

func (t SQLTracer) start(ctx context.Context, query string, parameterCount int) context.Context {
	if t.DatabaseID > 0 {
		ctx = WithFields(ctx, "database_id", t.DatabaseID)
	}
	ctx, span := Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "postgresql"), attribute.String("db.role", t.Role)))
	sum := sha256.Sum256([]byte(query))
	fields := []any{"db_role", t.Role, "query", RedactSQL(query), "query_sha256", hex.EncodeToString(sum[:]), "parameters", "[redacted]", "parameter_count", parameterCount}
	command := ""
	if words := strings.Fields(query); len(words) > 0 {
		switch strings.ToUpper(words[0]) {
		case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE":
			command = strings.ToLower(words[0])
		}
	}
	ctx = context.WithValue(ctx, queryKey{}, queryState{time.Now(), span, fields, command})
	Event(ctx, "sql_started", append(fields, "status", "running")...)
	return ctx
}

func finishSQL(ctx context.Context, tag pgconn.CommandTag, err error) {
	state, ok := ctx.Value(queryKey{}).(queryState)
	if !ok {
		return
	}
	defer state.span.End()
	status, code := "success", ""
	if err != nil {
		status, code = "failed", SQLErrorCode(err)
		Failure(state.span, code)
	}
	state.span.SetAttributes(attribute.Int64("db.response.rows", tag.RowsAffected()))
	fields := append(append([]any{}, state.fields...), "status", status, "duration_ms", time.Since(state.started).Milliseconds(), "row_count", tag.RowsAffected(), "error_code", code)
	Event(ctx, "sql_result", fields...)
	if state.command != "" {
		// A failed COMMIT response is not evidence that the transaction rolled back.
		if state.command == "commit" && err != nil {
			status = "unknown"
			if strings.HasPrefix(code, "23") || code == "40001" || code == "40P01" {
				status = "rolled_back"
			}
		} else if state.command == "commit" && tag.String() == "ROLLBACK" {
			status = "rolled_back"
		}
		Event(ctx, "sql_transaction_result", "db_role", state.fields[1], "command", state.command, "status", status, "error_code", code)
	}
}

func SQLErrorCode(err error) string {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && len(pgerr.Code) == 5 {
		for _, c := range pgerr.Code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				return "database_error"
			}
		}
		return pgerr.Code
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "database_error"
	}
}

// RedactSQL keeps SQL structure, masking literals, comments and quoted identifiers.
// Ambiguous or malformed lexical forms fail closed. The hash identifies the exact original.
func RedactSQL(query string) string {
	var out strings.Builder
	for i := 0; i < len(query); {
		switch {
		case strings.HasPrefix(query[i:], "--"):
			end := strings.IndexAny(query[i:], "\r\n")
			if end < 0 {
				i = len(query)
			} else {
				i += end
			}
			out.WriteByte(' ')
		case strings.HasPrefix(query[i:], "/*"):
			i += 2
			depth := 1
			for i < len(query) && depth > 0 {
				if strings.HasPrefix(query[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(query[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return "[redacted SQL]"
			}
			out.WriteByte(' ')
		case query[i] == '\'' || query[i] == '"':
			quote, closed := query[i], false
			i++
			for i < len(query) {
				if query[i] == '\\' {
					return "[redacted SQL]"
				}
				if query[i] == quote {
					i++
					if i < len(query) && query[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return "[redacted SQL]"
			}
			out.WriteByte('?')
		case query[i] == '$':
			j := i + 1
			if j < len(query) && query[j] >= '0' && query[j] <= '9' {
				for j < len(query) && query[j] >= '0' && query[j] <= '9' {
					j++
				}
				out.WriteString(query[i:j])
				i = j
				continue
			}
			for j < len(query) && (query[j] == '_' || query[j] >= 'a' && query[j] <= 'z' || query[j] >= 'A' && query[j] <= 'Z' || j > i+1 && query[j] >= '0' && query[j] <= '9') {
				j++
			}
			if j >= len(query) || query[j] != '$' {
				return "[redacted SQL]"
			}
			delim := query[i : j+1]
			end := strings.Index(query[j+1:], delim)
			if end < 0 {
				return "[redacted SQL]"
			}
			i = j + 1 + end + len(delim)
			out.WriteByte('?')
		case query[i] >= '0' && query[i] <= '9':
			i++
			for i < len(query) && (query[i] >= '0' && query[i] <= '9' || strings.ContainsRune(".eExXaAbBcCdDfF_+-", rune(query[i]))) {
				i++
			}
			out.WriteByte('?')
		case query[i] == '_' || query[i] >= 'a' && query[i] <= 'z' || query[i] >= 'A' && query[i] <= 'Z':
			j := i + 1
			for j < len(query) && (query[j] == '_' || query[j] == '$' || query[j] >= 'a' && query[j] <= 'z' || query[j] >= 'A' && query[j] <= 'Z' || query[j] >= '0' && query[j] <= '9') {
				j++
			}
			out.WriteString(query[i:j])
			i = j
		default:
			if query[i] >= 128 || query[i] < 32 && !strings.ContainsRune("\t\n\r\f", rune(query[i])) || query[i] == '\\' {
				return "[redacted SQL]"
			}
			out.WriteByte(query[i])
			i++
		}
	}
	safe := out.String()
	if len(safe) > 8192 {
		safe = safe[:8192] + " [truncated]"
	}
	return safe
}
