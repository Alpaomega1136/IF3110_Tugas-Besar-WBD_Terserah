package runner

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errHistorySetup   = errors.New("target history must be provisioned with a restricted executor")
	errTargetIdentity = errors.New("target identity changed or is already registered")
)

// internalSearchPath pins search_path to pg_catalog,pg_temp so internal queries
// never resolve operators/functions from writable schemas, while returning the
// caller's search_path so unqualified migration DDL still lands in their schema.
func internalSearchPath(ctx context.Context, tx pgx.Tx) (string, error) {
	var path string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.current_setting('search_path')`).Scan(&path); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `SET LOCAL search_path=pg_catalog,pg_temp`)
	return path, err
}

func (s *Service) signature(id int64, execution string, version int64, sql string, actor int64) string {
	payload, _ := json.Marshal([]any{"terserah-receipt-v1", id, execution, version, sql, actor})
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// protectedHistory verifies that `_terserah.migration_receipts` and `_terserah.target_identity`
// exist as ordinary tables without RLS/rules/triggers, that the connected executor role
// is not an owner/DBA/superuser, and that the executor can only SELECT/INSERT receipts
// (never UPDATE/DELETE/TRUNCATE) so committed receipts cannot be erased or forged.
func protectedHistory(ctx context.Context, tx pgx.Tx) error {
	if err := protectedTables(ctx, tx, []string{"migration_receipts", "target_identity"}); err != nil {
		return err
	}

	// Verify table columns match the expected receipt schema even when empty.
	rows, err := tx.Query(ctx, `
		SELECT execution_id, version, sql, executed_by, committed_at, signature
		FROM _terserah.migration_receipts
		LIMIT 0
	`)
	if err != nil {
		return errHistorySetup
	}
	rows.Close()
	if rows.Err() != nil {
		return errHistorySetup
	}
	return nil
}

// bindTarget pins the target's `_terserah.target_identity` UUID to the locked
// `database_connections` row so aliases or replacement databases are caught immediately.
func bindTarget(ctx context.Context, meta, target pgx.Tx, id int64) error {
	var identity string
	if err := target.QueryRow(ctx, `SELECT id::text FROM _terserah.target_identity WHERE singleton`).Scan(&identity); err != nil {
		return errHistorySetup
	}
	var stored *string
	if err := meta.QueryRow(ctx, `SELECT target_identity::text FROM database_connections WHERE id = $1`, id).Scan(&stored); err != nil {
		return err
	}
	if stored != nil {
		if *stored != identity {
			return errTargetIdentity
		}
		return nil
	}
	_, err := meta.Exec(ctx, `UPDATE database_connections SET target_identity = $1::uuid WHERE id = $2`, identity, id)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return errTargetIdentity
	}
	return err
}

func protectedTables(ctx context.Context, tx pgx.Tx, names []string) error {
	var safe bool
	err := tx.QueryRow(ctx, `
		SELECT count(*) = cardinality($1::text[]) AND COALESCE(bool_and(
			-- 1. Must be ordinary tables without RLS, rewrite rules, or user triggers
			c.relkind = 'r'
			AND NOT c.relrowsecurity
			AND NOT c.relhasrules
			AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_trigger
				WHERE tgrelid = c.oid AND NOT tgisinternal
			)
			-- 2. Connected executor must not own the table, schema, or database
			AND NOT pg_catalog.pg_has_role(current_user, c.relowner, 'MEMBER')
			AND NOT pg_catalog.pg_has_role(current_user, n.nspowner, 'MEMBER')
			AND NOT pg_catalog.pg_has_role(current_user, d.datdba, 'MEMBER')
			-- 3. Connected executor must not hold superuser or dangerous system roles
			AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_roles r
				WHERE (
					r.rolsuper OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls
					OR r.rolname IN (
						'pg_read_server_files', 'pg_write_server_files',
						'pg_execute_server_program', 'pg_read_all_stats', 'pg_signal_backend'
					)
				)
				AND pg_catalog.pg_has_role(current_user, r.oid, 'MEMBER')
			)
			-- 4. Connected executor must not be shared across other databases in the cluster
			AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_shdepend dep
				JOIN pg_catalog.pg_roles r ON r.oid = dep.refobjid
				WHERE dep.refclassid = 'pg_catalog.pg_authid'::pg_catalog.regclass
				  AND pg_catalog.pg_has_role(current_user, r.oid, 'MEMBER')
				  AND (
					(dep.dbid <> 0 AND dep.dbid <> d.oid)
					OR (dep.classid = 'pg_catalog.pg_database'::pg_catalog.regclass AND dep.objid <> d.oid)
				  )
			)
			-- 5. Append-only permissions on migration_receipts; read-only on target_identity
			AND NOT pg_catalog.has_schema_privilege(current_user, n.oid, 'CREATE')
			AND pg_catalog.has_schema_privilege(current_user, n.oid, 'USAGE')
			AND pg_catalog.has_table_privilege(current_user, c.oid, 'SELECT')
			AND (
				(c.relname <> 'target_identity' AND pg_catalog.has_table_privilege(current_user, c.oid, 'INSERT'))
				OR (c.relname = 'target_identity' AND NOT pg_catalog.has_table_privilege(current_user, c.oid, 'INSERT'))
			)
			AND NOT pg_catalog.has_table_privilege(current_user, c.oid, 'UPDATE,DELETE,TRUNCATE,TRIGGER')
			AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_attribute a
				WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
				  AND pg_catalog.has_column_privilege(current_user, c.oid, a.attnum, 'UPDATE')
			)
		), false)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN pg_catalog.pg_database d
		WHERE n.nspname = '_terserah'
		  AND c.relname = ANY($1::text[])
		  AND d.datname = pg_catalog.current_database()
	`, names).Scan(&safe)
	if err != nil || !safe {
		return errHistorySetup
	}

	return nil
}
