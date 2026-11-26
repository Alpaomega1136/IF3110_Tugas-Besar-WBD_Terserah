//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestBatchEvidenceIsAppendOnlyAndTransactional(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("TEST_TARGET_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, table := range []string{"batch_runs", "batch_scope", "batch_rows", "batch_checkpoints"} {
		var safe bool
		if err = tx.QueryRow(ctx, `SELECT has_table_privilege(current_user,$1,'SELECT') AND has_table_privilege(current_user,$1,'INSERT') AND NOT has_table_privilege(current_user,$1,'UPDATE,DELETE,TRUNCATE,TRIGGER')`, "_terserah."+table).Scan(&safe); err != nil || !safe {
			t.Fatal("batch evidence is not append-only", table, err)
		}
	}
	execution := fmt.Sprintf("ledger_%d", time.Now().UnixNano())
	if _, err = tx.Exec(ctx, `INSERT INTO _terserah.batch_runs(execution_id,database_id,version,executed_by,sql,table_oid,scope_count,scope_hash,signature) VALUES($1,1,1,1,'snapshot',1,1,'hash','signature')`, execution); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO _terserah.batch_scope VALUES($1,-1);`, execution); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO _terserah.batch_rows VALUES($1,1,-1,'{"v":1}','{"v":2}')`, execution); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO _terserah.batch_checkpoints(execution_id,batch_no,last_key,row_count,committed_rows,row_hash,previous_signature,signature) VALUES($1,1,-1,1,1,'rows','previous','signature')`, execution); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`UPDATE _terserah.batch_runs SET signature='forged' WHERE execution_id=$1`, `DELETE FROM _terserah.batch_scope WHERE execution_id=$1`, `UPDATE _terserah.batch_rows SET before_data='{}' WHERE execution_id=$1`, `DELETE FROM _terserah.batch_checkpoints WHERE execution_id=$1`} {
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = savepoint.Exec(ctx, sql, execution); err == nil {
			t.Fatal("executor modified committed evidence")
		}
		if err = savepoint.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM _terserah.batch_runs WHERE execution_id=$1`, execution).Scan(&count); err != nil || count != 0 {
		t.Fatal("evidence survived rollback", err)
	}
}
