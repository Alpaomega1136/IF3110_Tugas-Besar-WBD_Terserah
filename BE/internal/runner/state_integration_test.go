//go:build integration

package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func simpanEventPosisi(t *testing.T, pool *pgxpool.Pool, actor, dbID int64, draft Draft, execution, status, sql string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO migration_history
		(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
		SELECT $1,$2,$3,COALESCE(MAX(version),0)+1,$4,$5,$6,$7
		FROM migration_history WHERE database_id=$1`, dbID, draft.ID, draft.Revision, sql, actor, status, execution)
	if err != nil {
		t.Fatal(err)
	}
}

func simpanSnapshotPosisi(t *testing.T, pool *pgxpool.Pool, d Draft, execution string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO migration_revision_snapshots
		(draft_id,revision,sql_up,sql_down,irreversible,irreversible_reason,first_execution_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, d.ID, d.Revision, d.SQL, d.SQLDown, d.Irreversible, d.IrreversibleReason, execution)
	if err != nil {
		t.Fatal(err)
	}
}

func simpanPairPosisi(t *testing.T, pool *pgxpool.Pool, dbID int64, applied, rollback string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO migration_rollback_pairs
		(database_id,applied_execution_id,rollback_execution_id) VALUES ($1,$2,$3)`, dbID, applied, rollback)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPosisiMigrationDatabaseSuksesRollbackForward(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	down := "SELECT 2"
	a, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "A", SQL: "SELECT 1", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "B", SQL: "SELECT 3", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}

	simpanEventPosisi(t, pool, actor, dbID, a, "apply-A", "success", a.SQL)
	simpanSnapshotPosisi(t, pool, a, "apply-A")
	simpanEventPosisi(t, pool, actor, dbID, b, "apply-B", "success", b.SQL)
	simpanSnapshotPosisi(t, pool, b, "apply-B")
	state, err := bacaPosisiMigration(ctx, pool, dbID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Active) != 2 || state.NextRollback == nil || *state.NextRollback.ExecutionID != "apply-B" {
		t.Fatalf("state awal salah: %+v", state)
	}
	if err := state.cekRollback("apply-A"); !errors.Is(err, errMigrationOrder) {
		t.Fatalf("rollback A belum diizinkan: %v", err)
	}

	simpanEventPosisi(t, pool, actor, dbID, b, "rollback-B", "success", down)
	simpanPairPosisi(t, pool, dbID, "apply-B", "rollback-B")
	state, err = bacaPosisiMigration(ctx, pool, dbID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Active) != 1 || state.NextForward == nil || state.NextForward.RollbackExecutionID != "rollback-B" {
		t.Fatalf("rollback tidak mengubah posisi: %+v", state)
	}
	if err := state.cekForward("rollback-B"); err != nil {
		t.Fatal(err)
	}
	if err := state.cekUp(b.ID); !errors.Is(err, errMigrationMustForward) {
		t.Fatalf("SQL Up untuk B harus melalui forward: %v", err)
	}

	simpanEventPosisi(t, pool, actor, dbID, b, "forward-B", "success", b.SQL)
	state, err = bacaPosisiMigration(ctx, pool, dbID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Active) != 2 || len(state.RolledBack) != 0 || state.NextForward != nil || state.NextRollback == nil || *state.NextRollback.ExecutionID != "forward-B" {
		t.Fatalf("posisi setelah forward tidak sesuai: %+v", state)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE database_id=$1`, dbID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("riwayat up,rollback,forward harus utuh: count=%d err=%v", count, err)
	}
}

func TestPosisiMigrationDatabaseGagalPendingIrreversible(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	reason := "Tidak mempunyai data pemulihan"
	a, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "Tidak dapat dibatalkan", SQL: "SELECT 1", Irreversible: true, IrreversibleReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "Gagal", SQL: "SELECT 3", SQLDown: ptrString("SELECT 4")})
	if err != nil {
		t.Fatal(err)
	}
	simpanEventPosisi(t, pool, actor, dbID, a, "apply-I", "success", a.SQL)
	simpanSnapshotPosisi(t, pool, a, "apply-I")
	simpanEventPosisi(t, pool, actor, dbID, b, "failed-B", "failed", b.SQL)
	state, err := bacaPosisiMigration(ctx, pool, dbID)
	if err != nil {
		t.Fatal(err)
	}
	if state.HasUnresolved || len(state.Active) != 1 || state.NextRollback != nil {
		t.Fatalf("state irreversible/gagal salah: %+v", state)
	}
	if err := state.cekRollback("apply-I"); !errors.Is(err, errMigrationIrreversible) {
		t.Fatalf("irreversible tidak ditolak: %v", err)
	}
	if err := state.cekUp(b.ID); err != nil {
		t.Fatalf("attempt gagal harus tetap membuka migrasi baru: %v", err)
	}
	simpanEventPosisi(t, pool, actor, dbID, b, "pending-B", "unknown", b.SQL)
	state, err = bacaPosisiMigration(ctx, pool, dbID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasUnresolved || state.NextRollback != nil || state.NextForward != nil {
		t.Fatalf("unknown harus memblokir operasi: %+v", state)
	}
	if err := state.cekUp(999); !errors.Is(err, errMigrationUnresolved) {
		t.Fatalf("unknown tidak memblokir: %v", err)
	}
}

func TestPosisiMigrationTerpisahPerDatabase(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID1 := draftTestFixture(t, pool)
	var dbID2 int64
	if err := pool.QueryRow(ctx, `INSERT INTO database_connections
		(name,host,database_name,username,password_encrypted,sslmode)
		VALUES ('target-kedua','localhost','target-kedua','tester',decode('78','hex'),'disable')
		RETURNING id`).Scan(&dbID2); err != nil {
		t.Fatal(err)
	}
	svc := New(pool, nil, nil)
	up, down := "SELECT 1", "SELECT 2"
	d, err := svc.createDraft(ctx, dbID1, actor, draftInput{Title: "Di database pertama", SQL: up, SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	simpanEventPosisi(t, pool, actor, dbID1, d, "apply-db1", "success", d.SQL)
	simpanSnapshotPosisi(t, pool, d, "apply-db1")
	satu, err := bacaPosisiMigration(ctx, pool, dbID1)
	if err != nil {
		t.Fatal(err)
	}
	dua, err := bacaPosisiMigration(ctx, pool, dbID2)
	if err != nil {
		t.Fatal(err)
	}
	if len(satu.Active) != 1 || len(dua.Active) != 0 || dua.NextRollback != nil || dua.NextForward != nil {
		t.Fatalf("posisi migration bercampur antar database: db1=%+v db2=%+v", satu, dua)
	}
}
