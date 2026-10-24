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

var errHistorySetup = errors.New("target history must be provisioned with a restricted executor")
var errTargetIdentity = errors.New("target identity changed or is already registered")

// Internal queries must not resolve operators/functions from writable schemas.
// Save the user's path so unqualified migration DDL still creates in their schema.
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

// A signature prevents forged INSERTs; privileges prevent deletion/hiding of
// receipts, which would otherwise make committed work look rolled back.
func protectedHistory(ctx context.Context, tx pgx.Tx) error {
	var safe bool
	err := tx.QueryRow(ctx, `SELECT count(*)=2 AND COALESCE(bool_and(c.relkind='r' AND NOT c.relrowsecurity AND NOT c.relhasrules
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid=c.oid AND NOT tgisinternal)
 AND NOT pg_catalog.pg_has_role(current_user,c.relowner,'MEMBER')
 AND NOT pg_catalog.pg_has_role(current_user,n.nspowner,'MEMBER')
 AND NOT pg_catalog.pg_has_role(current_user,d.datdba,'MEMBER')
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles r WHERE (r.rolsuper OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls OR r.rolname IN ('pg_read_server_files','pg_write_server_files','pg_execute_server_program')) AND pg_catalog.pg_has_role(current_user,r.oid,'MEMBER'))
 AND NOT pg_catalog.has_schema_privilege(current_user,n.oid,'CREATE')
 AND pg_catalog.has_schema_privilege(current_user,n.oid,'USAGE')
 AND pg_catalog.has_table_privilege(current_user,c.oid,'SELECT')
 AND ((c.relname='migration_receipts' AND pg_catalog.has_table_privilege(current_user,c.oid,'INSERT')) OR (c.relname='target_identity' AND NOT pg_catalog.has_table_privilege(current_user,c.oid,'INSERT')))
 AND NOT pg_catalog.has_table_privilege(current_user,c.oid,'UPDATE,DELETE,TRUNCATE,TRIGGER')
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped AND pg_catalog.has_column_privilege(current_user,c.oid,a.attnum,'UPDATE'))),false)
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 CROSS JOIN pg_catalog.pg_database d
 WHERE n.nspname='_terserah' AND c.relname IN ('migration_receipts','target_identity') AND d.datname=pg_catalog.current_database()`).Scan(&safe)
	if err != nil || !safe {
		return errHistorySetup
	}
	// Fail closed for an old/malformed store even when it currently has no rows.
	rows, err := tx.Query(ctx, `SELECT execution_id,version,sql,executed_by,committed_at,signature FROM _terserah.migration_receipts LIMIT 0`)
	if err != nil {
		return errHistorySetup
	}
	rows.Close()
	if rows.Err() != nil {
		return errHistorySetup
	}
	return nil
}

// The metadata connection row is already locked. A stable target UUID catches
// aliases and replacement databases before missing receipts can imply rollback.
func bindTarget(ctx context.Context, meta, target pgx.Tx, id int64) error {
	var identity string
	if err := target.QueryRow(ctx, `SELECT id::text FROM _terserah.target_identity WHERE singleton`).Scan(&identity); err != nil {
		return errHistorySetup
	}
	var stored *string
	if err := meta.QueryRow(ctx, `SELECT target_identity::text FROM database_connections WHERE id=$1`, id).Scan(&stored); err != nil {
		return err
	}
	if stored != nil {
		if *stored != identity {
			return errTargetIdentity
		}
		return nil
	}
	_, err := meta.Exec(ctx, `UPDATE database_connections SET target_identity=$1::uuid WHERE id=$2`, identity, id)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return errTargetIdentity
	}
	return err
}
