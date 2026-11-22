//go:build integration

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/migrations"
)

// draftTestDB mengisolasi seluruh tabel pengujian pada schema PostgreSQL sementara.
func draftTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL belum diisi; gunakan database khusus pengujian")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("m2_draft_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
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
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatalf("gagal memasang schema metadata: %v", err)
	}
	return pool
}

func draftTestFixture(t *testing.T, pool *pgxpool.Pool) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var actorID, dbID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users(username,password_hash)
        VALUES ('orang2','test-hash') RETURNING id`).Scan(&actorID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO database_connections
        (name,host,database_name,username,password_encrypted,sslmode)
        VALUES ('target','localhost','target','tester',decode('78','hex'),'disable') RETURNING id`).Scan(&dbID); err != nil {
		t.Fatal(err)
	}
	return actorID, dbID
}

func TestDraftDefinitionCRUD(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	up := "ALTER TABLE users ADD COLUMN age int"
	down := "ALTER TABLE users DROP COLUMN age"
	d, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "  Tambah age  ", SQL: up, SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	if d.ID < 1 || d.Title != "Tambah age" || d.SQL != up || d.SQLDown == nil || *d.SQLDown != down || d.Irreversible || d.IrreversibleReason != nil || d.Revision != 1 {
		t.Fatalf("hasil create tidak sesuai: %+v", d)
	}
	read, err := svc.getDraft(ctx, dbID, d.ID)
	if err != nil || read.SQLDown == nil || *read.SQLDown != down {
		t.Fatalf("hasil get tidak sesuai: %+v, %v", read, err)
	}
	list, more, _, err := svc.listDraftsPage(ctx, dbID, 0, 10)
	if err != nil || more || len(list) != 1 || list[0].ID != d.ID || list[0].SQLDown == nil {
		t.Fatalf("hasil list tidak sesuai: %+v, %v", list, err)
	}
	reason := "  Tidak ada backup data lama  "
	updated, err := svc.updateDraft(ctx, dbID, d.ID, draftInput{Title: "Hapus data", SQL: "DELETE FROM users", Irreversible: true, IrreversibleReason: &reason, Revision: d.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || !updated.Irreversible || updated.SQLDown != nil || updated.IrreversibleReason == nil || *updated.IrreversibleReason != "Tidak ada backup data lama" {
		t.Fatalf("hasil update tidak sesuai: %+v", updated)
	}
	back, err := svc.updateDraft(ctx, dbID, d.ID, draftInput{Title: "Kembalikan reversible", SQL: up, SQLDown: &down, Revision: updated.Revision})
	if err != nil || back.Revision != 3 || back.Irreversible || back.IrreversibleReason != nil || back.SQLDown == nil || *back.SQLDown != down {
		t.Fatalf("konversi kembali ke reversible gagal: %+v, %v", back, err)
	}
	_, err = svc.updateDraft(ctx, dbID, d.ID, draftInput{Title: "Revisi lama", SQL: up, SQLDown: &down, Revision: d.Revision})
	if !errors.Is(err, errConflict) {
		t.Fatalf("update revisi usang tidak ditolak: %v", err)
	}
	var storedDown *string
	var storedReason *string
	if err := pool.QueryRow(ctx, `SELECT sql_down, irreversible_reason FROM migration_drafts WHERE id=$1`, d.ID).Scan(&storedDown, &storedReason); err != nil || storedDown == nil || *storedDown != down || storedReason != nil {
		t.Fatalf("metadata tidak tersimpan: down=%v reason=%v err=%v", storedDown, storedReason, err)
	}
}

func TestDraftDefinitionRejectInvalid(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	_, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "Tidak lengkap", SQL: "SELECT 1"})
	if !errors.Is(err, errDraftDefinition) {
		t.Fatalf("draft tanpa down/alasan diterima: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM migration_drafts").Scan(&count); err != nil || count != 0 {
		t.Fatalf("input invalid tersimpan: count=%d err=%v", count, err)
	}
}
