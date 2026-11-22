//go:build integration

package runner

import (
	"context"
	"testing"
	"time"
)

// TestFinalisasiRollbackAtomik memastikan status sukses dan pasangan tersimpan bersama.
func TestFinalisasiRollbackAtomik(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	down := "SELECT 2"
	svc := New(pool, nil, nil)
	draft, err := svc.createDraft(ctx, dbID, actor, draftInput{Title: "Up Down", SQL: "SELECT 1", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := catatExecutionPertamaTx(ctx, tx, dbID, draft.ID, draft.Revision, actor, "stage6-up", draft.SQL, &down, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE migration_history SET status='success' WHERE execution_id='stage6-up'`); err != nil {
		t.Fatal(err)
	}
	target := "stage6-up"
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := catatExecutionAksiTx(ctx, tx, dbID, draft.ID, draft.Revision, actor, "stage6-rollback", "rollback", down, &target, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := svc.finalisasiAttempt(ctx, Result{ExecutionID: "stage6-rollback", Status: "success"}, &now); err != nil {
		t.Fatal(err)
	}
	var status string
	var count int
	if err := pool.QueryRow(ctx, `SELECT status FROM migration_history WHERE execution_id='stage6-rollback'`).Scan(&status); err != nil || status != "success" {
		t.Fatalf("status rollback: %s %v", status, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM migration_rollback_pairs WHERE applied_execution_id='stage6-up' AND rollback_execution_id='stage6-rollback'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pasangan rollback tidak ditemukan: %d %v", count, err)
	}

	// Duplikasi pasangan harus menggagalkan transaction sehingga status tetap running.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := catatExecutionAksiTx(ctx, tx, dbID, draft.ID, draft.Revision, actor, "stage6-duplikat", "rollback", down, &target, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.finalisasiAttempt(ctx, Result{ExecutionID: "stage6-duplikat", Status: "success"}, &now); err == nil {
		t.Fatal("pasangan rollback duplikat seharusnya ditolak")
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM migration_history WHERE execution_id='stage6-duplikat'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("status harus tetap running setelah rollback metadata: %s %v", status, err)
	}
}
