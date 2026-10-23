package viewer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"terserah/backend/internal/auth"
	"terserah/backend/internal/connections"
	"terserah/backend/internal/httpx"
)

type Service struct{ targets *connections.Service }

var errResultTooLarge = errors.New("viewer result too large")

func New(targets *connections.Service) *Service { return &Service{targets: targets} }

func (s *Service) Register(mux *http.ServeMux, sessions *auth.Service) {
	for _, path := range []string{
		"/api/v1/databases/{id}/schemas",
		"/api/v1/databases/{id}/schemas/{schema}/tables",
		"/api/v1/databases/{id}/schemas/{schema}/tables/{table}",
		"/api/v1/databases/{id}/schemas/{schema}/tables/{table}/rows",
	} {
		mux.HandleFunc("GET "+path, sessions.Require(s.read))
	}
}

func identifier(value string) bool {
	return len(value) > 0 && len(value) <= 63 && !strings.ContainsRune(value, 0)
}

func pagination(r *http.Request) (int, int64, error) {
	limit := 50
	var offset int64
	if r.URL.Query().Has("limit") {
		n, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || n < 1 || n > 200 {
			return 0, 0, errors.New("invalid limit")
		}
		limit = n
	}
	if r.URL.Query().Has("offset") {
		n, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if err != nil || n < 0 {
			return 0, 0, errors.New("invalid offset")
		}
		offset = n
	}
	return limit, offset, nil
}

func fail(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "42501" {
			httpx.Error(w, 403, "forbidden", "Target database denied access")
			return
		}
		if pgErr.Code == "42P01" || pgErr.Code == "3F000" {
			httpx.Error(w, 404, "not_found", "Database object not found")
			return
		}
	}
	switch {
	case errors.Is(err, errResultTooLarge):
		httpx.Error(w, 413, "result_too_large", "Reduce the row limit; result exceeds 4 MiB")
	case errors.Is(err, connections.ErrForbidden):
		httpx.Error(w, 403, "forbidden", "Access denied")
	case errors.Is(err, connections.ErrNotFound):
		httpx.Error(w, 404, "not_found", "Database object not found")
	case errors.Is(err, connections.ErrMetadataTarget):
		httpx.Error(w, 409, "metadata_target_forbidden", "Metadata database cannot be used as a target")
	default:
		httpx.Error(w, 503, "target_unavailable", "Cannot read target database")
	}
}

func jsonQuery(ctx context.Context, tx pgx.Tx, query string, args ...any) (json.RawMessage, error) {
	var value []byte
	err := tx.QueryRow(ctx, query, args...).Scan(&value)
	return json.RawMessage(value), err
}

func (s *Service) read(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, 400, "invalid_input", "Invalid database ID")
		return
	}
	schema, table := r.PathValue("schema"), r.PathValue("table")
	if strings.HasPrefix(schema, "pg_") || schema == "information_schema" || schema == "_terserah" {
		httpx.Error(w, 403, "forbidden", "System schema is not available in the data viewer")
		return
	}
	if (schema != "" && !identifier(schema)) || (table != "" && !identifier(table)) {
		httpx.Error(w, 400, "invalid_input", "Invalid database object name")
		return
	}
	limit, offset, err := pagination(r)
	if err != nil {
		httpx.Error(w, 400, "invalid_input", "Limit must be 1-200 and offset must be nonnegative")
		return
	}
	p, _ := auth.User(r.Context())
	pool, err := s.targets.OpenTarget(r.Context(), p, id, false)
	if err != nil {
		fail(w, err)
		return
	}
	defer pool.Close()
	tx, err := pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(r.Context(), "SET LOCAL statement_timeout='5s'"); err != nil {
		fail(w, err)
		return
	}
	var result any
	if schema == "" {
		items, e := jsonQuery(r.Context(), tx, `SELECT COALESCE(jsonb_agg(nspname ORDER BY nspname),'[]'::jsonb) FROM pg_catalog.pg_namespace WHERE nspname NOT LIKE 'pg\_%' ESCAPE '\' AND nspname NOT IN ('information_schema','_terserah') AND pg_catalog.has_schema_privilege(oid,'USAGE')`)
		err = e
		result = map[string]any{"schemas": items}
	} else {
		var exists bool
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname=$1 AND has_schema_privilege(oid,'USAGE'))", schema).Scan(&exists)
		if err == nil && !exists {
			fail(w, connections.ErrNotFound)
			return
		}
		if err == nil && table == "" {
			items, e := jsonQuery(r.Context(), tx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('name',c.relname,'kind',CASE WHEN c.relkind='p' THEN 'partitioned_table' ELSE 'table' END) ORDER BY c.relname),'[]'::jsonb) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p')`, schema)
			err = e
			result = map[string]any{"tables": items}
		} else if err == nil {
			// Schema and table are separately quoted, never interpolated as SQL.
			qualified := pgx.Identifier{schema, table}.Sanitize()
			var oid uint32
			err = tx.QueryRow(r.Context(), `SELECT c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p')`, schema, table).Scan(&oid)
			if errors.Is(err, pgx.ErrNoRows) {
				fail(w, connections.ErrNotFound)
				return
			}
			if err == nil {
				_, err = tx.Exec(r.Context(), "LOCK TABLE "+qualified+" IN ACCESS SHARE MODE")
			}
			if err == nil && strings.HasSuffix(r.Pattern, "/rows") {
				result, err = readRows(r.Context(), tx, qualified, oid, limit, offset)
			} else if err == nil {
				result, err = metadata(r.Context(), tx, oid)
			}
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		fail(w, err)
		return
	}
	httpx.JSON(w, 200, result)
}

func metadata(ctx context.Context, tx pgx.Tx, oid uint32) (any, error) {
	columns, err := jsonQuery(ctx, tx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('name',a.attname,'position',a.attnum,'data_type',pg_catalog.format_type(a.atttypid,a.atttypmod),'nullable',NOT a.attnotnull,'default',pg_catalog.pg_get_expr(d.adbin,d.adrelid),'identity',a.attidentity,'generated',a.attgenerated) ORDER BY a.attnum),'[]'::jsonb) FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped`, oid)
	if err != nil {
		return nil, err
	}
	constraints, err := jsonQuery(ctx, tx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('name',conname,'type',contype,'definition',pg_catalog.pg_get_constraintdef(oid,true)) ORDER BY conname),'[]'::jsonb) FROM pg_catalog.pg_constraint WHERE conrelid=$1`, oid)
	if err != nil {
		return nil, err
	}
	indexes, err := jsonQuery(ctx, tx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('name',c.relname,'unique',i.indisunique,'primary',i.indisprimary,'definition',pg_catalog.pg_get_indexdef(i.indexrelid)) ORDER BY c.relname),'[]'::jsonb) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=$1`, oid)
	return map[string]any{"columns": columns, "constraints": constraints, "indexes": indexes}, err
}

func readRows(ctx context.Context, tx pgx.Tx, qualified string, oid uint32, limit int, offset int64) (any, error) {
	var order []string
	keyRows, err := tx.Query(ctx, `SELECT a.attname FROM pg_catalog.pg_index i CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum,pos) JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE i.indrelid=$1 AND i.indisprimary AND k.pos<=i.indnkeyatts ORDER BY k.pos`, oid)
	if err != nil {
		return nil, err
	}
	for keyRows.Next() {
		var name string
		if err = keyRows.Scan(&name); err != nil {
			keyRows.Close()
			return nil, err
		}
		order = append(order, pgx.Identifier{name}.Sanitize())
	}
	err = keyRows.Err()
	keyRows.Close()
	if err != nil {
		return nil, err
	}
	orderBy := "tableoid,ctid"
	ordering := "physical"
	if len(order) > 0 {
		orderBy = strings.Join(order, ",")
		ordering = "primary_key"
	}
	rows, err := tx.Query(ctx, "SELECT * FROM "+qualified+" ORDER BY "+orderBy+" LIMIT $1 OFFSET $2", pgx.QueryExecModeSimpleProtocol, limit+1, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make([]map[string]any, 0)
	for _, field := range rows.FieldDescriptions() {
		columns = append(columns, map[string]any{"name": field.Name, "type_oid": field.DataTypeOID})
	}
	data := make([][]any, 0)
	size := 0
	more := false
	for rows.Next() {
		if len(data) == limit {
			more = true
			break
		}
		values := make([]any, len(columns))
		for i, raw := range rows.RawValues() {
			if raw != nil {
				size += len(raw)
				if size > 4<<20 {
					return nil, errResultTooLarge
				}
				values[i] = string(raw)
			}
		}
		data = append(data, values)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"columns": columns, "rows": data, "limit": limit, "offset": offset, "has_more": more, "ordering": ordering}, nil
}
