package runner

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
)

var (
	errBatchSetup    = errors.New("batch evidence must be provisioned by target owner")
	errBatchTable    = errors.New("batch requires an ordinary trigger-free table with a single integer primary key and writable non-key columns")
	errBatchScope    = errors.New("batch target rows changed outside the frozen scope")
	errBatchPayload  = errors.New("batch recovery images exceed the bounded memory budget")
	errBatchEvidence = errors.New("batch commit evidence could not be verified")
)

func protectedBatchHistory(ctx context.Context, tx pgx.Tx) error {
	if err := protectedHistory(ctx, tx); err != nil {
		return err
	}
	if err := protectedTables(ctx, tx, []string{"batch_runs", "batch_scope", "batch_rows", "batch_checkpoints"}); err != nil {
		return errBatchSetup
	}
	for _, query := range []string{
		`SELECT execution_id,database_id,version,executed_by,sql,table_oid,scope_count,scope_hash,signature FROM _terserah.batch_runs LIMIT 0`,
		`SELECT execution_id,row_key FROM _terserah.batch_scope LIMIT 0`,
		`SELECT execution_id,batch_no,row_key,before_data,after_data FROM _terserah.batch_rows LIMIT 0`,
		`SELECT execution_id,batch_no,last_key,row_count,committed_rows,row_hash,previous_signature,signature FROM _terserah.batch_checkpoints LIMIT 0`,
	} {
		rows, err := tx.Query(ctx, query)
		if err != nil {
			return errBatchSetup
		}
		rows.Close()
		if rows.Err() != nil {
			return errBatchSetup
		}
	}
	return nil
}

func batchTable(ctx context.Context, tx pgx.Tx, c batch.Config) (uint32, error) {
	if _, err := tx.Exec(ctx, "LOCK TABLE "+c.Relation()+" IN ROW EXCLUSIVE MODE"); err != nil {
		return 0, err
	}
	var oid uint32
	var safe bool
	err := tx.QueryRow(ctx, `SELECT r.oid,r.relkind='r' AND NOT r.relrowsecurity AND NOT r.relhasrules
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid=r.oid AND NOT tgisinternal)
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_inherits WHERE inhrelid=r.oid OR inhparent=r.oid)
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_index i JOIN pg_catalog.pg_attribute a ON a.attrelid=r.oid AND a.attnum=i.indkey[0]
 WHERE i.indrelid=r.oid AND i.indisprimary AND i.indnatts=1 AND a.attname=$3 AND a.atttypid IN (20,21,23))
 AND (SELECT count(*) FROM pg_catalog.pg_attribute a WHERE a.attrelid=r.oid AND a.attnum>0 AND NOT a.attisdropped
 AND a.attname=ANY($4::text[]) AND a.attgenerated='' AND a.attidentity='')=cardinality($4::text[])
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_constraint f JOIN pg_catalog.pg_attribute a ON a.attrelid=r.oid AND a.attnum=ANY(f.confkey)
 WHERE f.contype='f' AND f.confrelid=r.oid AND a.attname=ANY($4::text[]))
 FROM pg_catalog.pg_class r JOIN pg_catalog.pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=$1 AND r.relname=$2`, c.Schema, c.Table, c.Key, c.Columns()).Scan(&oid, &safe)
	if err != nil {
		return 0, errBatchTable
	}
	if !safe {
		return 0, errBatchTable
	}
	return oid, nil
}
