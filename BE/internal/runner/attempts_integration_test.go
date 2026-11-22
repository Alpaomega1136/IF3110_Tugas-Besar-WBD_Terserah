//go:build integration

package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestRiwayatAttemptUlang memastikan retry mendapat event baru tanpa mengubah snapshot.
func TestRiwayatAttemptUlang(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	down := "SELECT 2"
	d, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "Percobaan", SQL: "SELECT 1/0", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	v1, err := catatExecutionPertamaTx(ctx, tx, dbID, d.ID, d.Revision, actor, "attempt-01", d.SQL, d.SQLDown, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE migration_history SET status='failed',error_code='22012',finished_at=now()
		WHERE database_id=$1 AND execution_id='attempt-01'`, dbID); err != nil {
		t.Fatal(err)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	v2, err := catatExecutionUlangTx(ctx, tx2, dbID, d.ID, d.Revision, actor, "attempt-02", "attempt-01", d.SQL)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if v1 != 1 || v2 != 2 {
		t.Fatalf("nomor event tidak berurutan: %d %d", v1, v2)
	}
	if _, err := pool.Exec(ctx, `UPDATE migration_history SET status='failed',error_code='22012',finished_at=now()
		WHERE database_id=$1 AND execution_id='attempt-02'`, dbID); err != nil {
		t.Fatal(err)
	}

	items, more, _, err := svc.listHistoryPage(ctx, dbID, 0, 10, false)
	if err != nil || more || len(items) != 2 || items[0].ExecutionID == nil || *items[0].ExecutionID != "attempt-02" ||
		items[0].RetryOfExecutionID == nil || *items[0].RetryOfExecutionID != "attempt-01" || items[1].Status != "failed" {
		t.Fatalf("riwayat retry tidak lengkap: %+v err=%v", items, err)
	}
	detail, err := svc.getHistoryByExecution(ctx, dbID, "attempt-02")
	if err != nil || detail.SQL != d.SQL || detail.ActionType != "up" || detail.Revision == nil || *detail.Revision != d.Revision {
		t.Fatalf("detail salah: %+v err=%v", detail, err)
	}
	_, err = svc.getHistoryByExecution(ctx, dbID+100, "attempt-02")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("execution lintas database terbaca: %v", err)
	}

	// Setiap attempt hanya memiliki satu retry langsung, dan identitasnya tidak boleh diubah.
	_, err = pool.Exec(ctx, `INSERT INTO migration_history
		(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,retry_of_execution_id)
		VALUES ($1,$2,$3,3,$4,$5,'running','attempt-03','attempt-01')`, dbID, d.ID, d.Revision, d.SQL, actor)
	pastikanSQLState(t, err, "23505")
	_, err = pool.Exec(ctx, `UPDATE migration_history SET sql='SELECT 99' WHERE execution_id='attempt-02'`)
	pastikanSQLState(t, err, "23514")
	_, err = pool.Exec(ctx, `DELETE FROM migration_history WHERE execution_id='attempt-02'`)
	pastikanSQLState(t, err, "23514")
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM migration_revision_snapshots WHERE draft_id=$1`, d.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot harus tetap satu: count=%d err=%v", count, err)
	}
}

func TestRiwayatHideRollback(t *testing.T) {
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
	c, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "C", SQL: "SELECT 5", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	simpanEventPosisi(t, pool, actor, dbID, a, "apply-A", "success", a.SQL)
	simpanSnapshotPosisi(t, pool, a, "apply-A")
	simpanEventPosisi(t, pool, actor, dbID, b, "apply-B", "success", b.SQL)
	simpanSnapshotPosisi(t, pool, b, "apply-B")
	simpanEventPosisi(t, pool, actor, dbID, b, "rollback-B", "success", down)
	simpanPairPosisi(t, pool, dbID, "apply-B", "rollback-B")
	_, err = pool.Exec(ctx, `INSERT INTO migration_history
		(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,action_type,target_execution_id)
		VALUES ($1,$2,$3,4,$4,$5,'success','forward-B','forward','rollback-B')`, dbID, b.ID, b.Revision, b.SQL, actor)
	if err != nil {
		t.Fatal(err)
	}
	simpanEventPosisi(t, pool, actor, dbID, c, "failed-C", "failed", c.SQL)
	all, _, _, err := svc.listHistoryPage(ctx, dbID, 0, 10, false)
	if err != nil || len(all) != 5 {
		t.Fatalf("riwayat lengkap: len=%d err=%v", len(all), err)
	}
	var rollbackFound bool
	for _, item := range all {
		if item.ExecutionID != nil && *item.ExecutionID == "rollback-B" && item.ActionType == "rollback" {
			rollbackFound = true
		}
	}
	if !rollbackFound {
		t.Fatal("aksi rollback tidak teridentifikasi")
	}
	filtered, more, _, err := svc.listHistoryPage(ctx, dbID, 0, 10, true)
	if err != nil || more || len(filtered) != 3 {
		t.Fatalf("hide rollback: %+v err=%v", filtered, err)
	}
	for _, item := range filtered {
		if item.ExecutionID == nil || *item.ExecutionID == "apply-B" || *item.ExecutionID == "rollback-B" {
			t.Fatalf("filter salah: %+v", item)
		}
	}
	if filtered[0].Status != "failed" || filtered[1].ActionType != "forward" {
		t.Fatalf("attempt gagal dan forward harus terlihat: %+v", filtered)
	}
	first, more, cursor, err := svc.listHistoryPage(ctx, dbID, 0, 2, true)
	if err != nil || !more || len(first) != 2 || cursor == nil {
		t.Fatalf("halaman pertama: %+v %v", first, err)
	}
	last, more, _, err := svc.listHistoryPage(ctx, dbID, first[len(first)-1].Version, 2, true)
	if err != nil || more || len(last) != 1 || last[0].ExecutionID == nil || *last[0].ExecutionID != "apply-A" {
		t.Fatalf("halaman berikutnya: %+v err=%v", last, err)
	}
	var persisted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE database_id=$1`, dbID).Scan(&persisted); err != nil || persisted != 5 {
		t.Fatalf("filter tidak boleh menghapus data: %d %v", persisted, err)
	}
}

// TestRiwayatAksiMemakaiSnapshot memeriksa Up/Down immutable untuk event rollback/forward.
func TestRiwayatAksiMemakaiSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	down := "SELECT 2"
	d, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "Reversible", SQL: "SELECT 1", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := catatExecutionPertamaTx(ctx, tx, dbID, d.ID, d.Revision, actor, "up-original", d.SQL, d.SQLDown, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE migration_history SET status='success' WHERE execution_id='up-original'`); err != nil {
		t.Fatal(err)
	}

	rollbackID := "up-original"
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := catatExecutionAksiTx(ctx, tx, dbID, d.ID, d.Revision, actor, "rollback-1", "rollback", "SELECT 99", &rollbackID, nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("SQL Down lain tidak ditolak: %v", err)
	}
	if _, err := catatExecutionAksiTx(ctx, tx, dbID, d.ID, d.Revision, actor, "rollback-1", "rollback", down, nil, nil); !errors.Is(err, errMigrationOrder) {
		t.Fatalf("target rollback kosong tidak ditolak: %v", err)
	}
	v2, err := catatExecutionAksiTx(ctx, tx, dbID, d.ID, d.Revision, actor, "rollback-1", "rollback", down, &rollbackID, nil)
	if err != nil || v2 != 2 {
		t.Fatalf("event rollback gagal: version=%d err=%v", v2, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE migration_history SET status='success' WHERE execution_id='rollback-1'`); err != nil {
		t.Fatal(err)
	}
	simpanPairPosisi(t, pool, dbID, "up-original", "rollback-1")

	forwardTarget := "rollback-1"
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	v3, err := catatExecutionAksiTx(ctx, tx, dbID, d.ID, d.Revision, actor, "forward-1", "forward", d.SQL, &forwardTarget, nil)
	if err != nil || v3 != 3 {
		t.Fatalf("event forward gagal: version=%d err=%v", v3, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	item, err := svc.getHistoryByExecution(ctx, dbID, "forward-1")
	if err != nil || item.ActionType != "forward" || item.TargetExecutionID == nil || *item.TargetExecutionID != forwardTarget || item.SQL != d.SQL {
		t.Fatalf("history forward tidak lengkap: %+v err=%v", item, err)
	}
}
