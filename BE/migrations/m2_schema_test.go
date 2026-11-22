//go:build integration

package migrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaTestPool membuat schema sementara yang terpisah agar pengujian tidak mengubah tabel aplikasi maupun data pengujian lain.
func schemaTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL belum diisi; gunakan URL PostgreSQL khusus pengujian")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("m2_schema_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatalf("gagal membuat schema pengujian: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("gagal menghapus schema pengujian: %v", err)
		}
		admin.Close()
	})
	return pool
}

func assertSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("diharapkan SQLSTATE %s, diperoleh %v", code, err)
	}
}

func seedSchemaFixture(t *testing.T, db *pgxpool.Pool, title string) (int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	var userID, dbID, draftID int64
	if err := db.QueryRow(ctx, `INSERT INTO users(username,password_hash)
        VALUES ($1, 'fixture-hash') RETURNING id`, title).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO database_connections
        (name,host,database_name,username,password_encrypted,sslmode)
        VALUES ($1,'localhost',$1,'tester',decode('78','hex'),'disable') RETURNING id`, title).Scan(&dbID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO migration_drafts(database_id,title,sql,created_by)
        VALUES ($1,$2,'SELECT 1',$3) RETURNING id`, dbID, title, userID).Scan(&draftID); err != nil {
		t.Fatal(err)
	}
	return userID, dbID, draftID
}

func TestM2SchemaCleanInstall(t *testing.T) {
	ctx := context.Background()
	db := schemaTestPool(t)
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("migration schema tidak idempotent: %v", err)
	}
	_, _, draftID := seedSchemaFixture(t, db, "clean")
	var sql string
	var down, reason *string
	var irreversible bool
	if err := db.QueryRow(ctx, `SELECT sql,sql_down,irreversible,irreversible_reason
        FROM migration_drafts WHERE id=$1`, draftID).Scan(&sql, &down, &irreversible, &reason); err != nil {
		t.Fatal(err)
	}
	if sql != "SELECT 1" || down != nil || irreversible || reason != nil {
		t.Fatalf("nilai awal draft lama berubah: up=%q down=%v irreversible=%v reason=%v", sql, down, irreversible, reason)
	}
	_, err := db.Exec(ctx, `UPDATE migration_drafts SET irreversible=true WHERE id=$1`, draftID)
	assertSQLState(t, err, "23514")
	_, err = db.Exec(ctx, `INSERT INTO migration_revision_snapshots
        (draft_id,revision,sql_up,irreversible)
        VALUES ($1,1,'SELECT 1',false)`, draftID)
	assertSQLState(t, err, "23514")
	if _, err := db.Exec(ctx, `INSERT INTO migration_revision_snapshots
        (draft_id,revision,sql_up,sql_down,irreversible)
        VALUES ($1,1,'SELECT 1','SELECT 2',false)`, draftID); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `UPDATE migration_revision_snapshots SET sql_up='SELECT 99'
        WHERE draft_id=$1 AND revision=1`, draftID)
	assertSQLState(t, err, "23514")
	_, err = db.Exec(ctx, `DELETE FROM migration_revision_snapshots
        WHERE draft_id=$1 AND revision=1`, draftID)
	assertSQLState(t, err, "23514")
}

func TestM2SchemaUpgradeExistingHistory(t *testing.T) {
	ctx := context.Background()
	db := schemaTestPool(t)
	if _, err := db.Exec(ctx, `CREATE TABLE platform_schema_migrations
        (version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"000001_initial.sql", "000002_m1_dependencies.sql", "000003_m3_runner.sql",
		"000004_m4_history.sql", "000005_m2_approval_workflow.sql",
	} {
		body, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(body)); err != nil {
			t.Fatalf("gagal menerapkan migration awal %s: %v", name, err)
		}
		if _, err := db.Exec(ctx,
			"INSERT INTO platform_schema_migrations(version) VALUES($1)", name); err != nil {
			t.Fatal(err)
		}
	}
	userID, dbID, draftID := seedSchemaFixture(t, db, "legacy")
	const upExec = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := db.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
        VALUES ($1,$2,1,1,'SELECT 1',$3,'success',$4)`, dbID, draftID, userID, upExec); err != nil {
		t.Fatal(err)
	}
	// Riwayat lama tanpa revisi tidak boleh ditebak sebagai revisi terbaru.
	if _, err := db.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,version,sql,executed_by,status)
        VALUES ($1,$2,2,'SELECT 9',$3,'failed')`, dbID, draftID, userID); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("upgrade schema tidak idempotent: %v", err)
	}
	var retainedLegacy bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM migration_history
        WHERE database_id=$1 AND version=2 AND draft_revision IS NULL
        AND status='failed')`, dbID).Scan(&retainedLegacy); err != nil || !retainedLegacy {
		t.Fatalf("riwayat lama tanpa revisi hilang: %v", err)
	}
	var up, legacyReason, firstExecution string
	var isIrreversible bool
	if err := db.QueryRow(ctx, `SELECT sql_up,irreversible,irreversible_reason,first_execution_id
        FROM migration_revision_snapshots WHERE draft_id=$1 AND revision=1`, draftID).
		Scan(&up, &isIrreversible, &legacyReason, &firstExecution); err != nil {
		t.Fatal(err)
	}
	if up != "SELECT 1" || !isIrreversible || legacyReason == "" || firstExecution != upExec {
		t.Fatal("snapshot revisi lama tidak dipertahankan sebagai irreversible")
	}
	const rollbackExec = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := db.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
        VALUES ($1,$2,1,3,'SELECT 2',$3,'success',$4)`, dbID, draftID, userID, rollbackExec); err != nil {
		t.Fatal("revisi draft yang sama harus dapat memiliki attempt lain:", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO migration_rollback_pairs
        (database_id,applied_execution_id,rollback_execution_id)
        VALUES ($1,$2,$3)`, dbID, upExec, rollbackExec); err != nil {
		t.Fatal(err)
	}
	var found bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM migration_rollback_pairs
        WHERE database_id=$1 AND applied_execution_id=$2 AND rollback_execution_id=$3
        )`, dbID, upExec, rollbackExec).Scan(&found); err != nil || !found {
		t.Fatalf("pasangan rollback sukses tidak ditemukan: %v", err)
	}
	_, err := db.Exec(ctx, `INSERT INTO migration_rollback_pairs
        (database_id,applied_execution_id,rollback_execution_id)
        VALUES ($1,$2,$2)`, dbID, upExec)
	assertSQLState(t, err, "23514")
	_, err = db.Exec(ctx, `DELETE FROM migration_rollback_pairs WHERE rollback_execution_id=$1`, rollbackExec)
	assertSQLState(t, err, "23514")
	// Pasangan rollback hanya boleh mengacu pada execution dengan status sukses.
	const failedExecution = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, err := db.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
        VALUES ($1,$2,1,4,'SELECT 3',$3,'failed',$4)`, dbID, draftID, userID, failedExecution); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO migration_rollback_pairs
        (database_id,applied_execution_id,rollback_execution_id)
        VALUES ($1,$2,$3)`, dbID, upExec, failedExecution)
	assertSQLState(t, err, "23514")
	// Attempt gagal harus tetap tercatat meskipun revisi yang sama pernah sukses.
	if _, err := db.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
        VALUES ($1,$2,1,5,'SELECT 1',$3,'failed',$4)`, dbID, draftID, userID,
		"cccccccccccccccccccccccccccccccc"); err != nil {
		t.Fatal("attempt gagal tidak tercatat:", err)
	}
	// Tidak boleh memasangkan execution dari database yang berbeda.
	otherUser, otherDB, otherDraft := seedSchemaFixture(t, db, "other")
	const otherExecution = "dddddddddddddddddddddddddddddddd"
	if _, err := db.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
        VALUES ($1,$2,1,1,'SELECT 1',$3,'success',$4)`, otherDB, otherDraft, otherUser, otherExecution); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO migration_rollback_pairs
        (database_id,applied_execution_id,rollback_execution_id)
        VALUES ($1,$2,$3)`, dbID, otherExecution, "cccccccccccccccccccccccccccccccc")
	assertSQLState(t, err, "23503")
}
