//go:build integration

package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func pastikanSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("SQLSTATE diharapkan %s, diperoleh %v", code, err)
	}
}

func TestImmutableDraftSetelahAttemptGagal(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	down := "SELECT 2"
	d, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "Versi awal", SQL: "SELECT 1", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	d, err = svc.updateDraft(ctx, dbID, d.ID, draftInput{Title: "Versi kedua", SQL: "SELECT 3", SQLDown: &down, Revision: d.Revision})
	if err != nil || d.Revision != 2 {
		t.Fatalf("draft yang belum pernah dieksekusi harus bisa diubah: %+v %v", d, err)
	}

	// Gagal dieksekusi tetap mengunci definisi, meskipun belum ada receipt sukses.
	const executionID = "abababababababababababababababab"
	if _, err := pool.Exec(ctx, `INSERT INTO migration_history
        (database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
        VALUES ($1,$2,$3,1,$4,$5,'failed',$6)`, dbID, d.ID, d.Revision, d.SQL, actorID, executionID); err != nil {
		t.Fatal(err)
	}
	_, err = svc.updateDraft(ctx, dbID, d.ID, draftInput{Title: "Diganti", SQL: "SELECT 4", SQLDown: &down, Revision: d.Revision})
	if !errors.Is(err, errDraftLocked) {
		t.Fatalf("service harus menolak revisi yang pernah dieksekusi: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE migration_drafts SET sql='SELECT 99' WHERE id=$1`, d.ID)
	pastikanSQLState(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE migration_drafts SET sql_down='SELECT 99' WHERE id=$1`, d.ID)
	pastikanSQLState(t, err, "23514")
	_, err = pool.Exec(ctx, `UPDATE migration_drafts SET revision=revision+1 WHERE id=$1`, d.ID)
	pastikanSQLState(t, err, "23514")

	var title, up string
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT title,sql,revision FROM migration_drafts WHERE id=$1`, d.ID).Scan(&title, &up, &revision); err != nil {
		t.Fatal(err)
	}
	if title != "Versi kedua" || up != "SELECT 3" || revision != 2 {
		t.Fatalf("definisi berubah setelah attempt gagal: %s %s %d", title, up, revision)
	}
}

func TestImmutableSnapshotDicatatAtomik(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	down := "SELECT 5"
	d, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "Dapat dibatalkan", SQL: "SELECT 4", SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	const executionID = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	version, err := catatExecutionPertamaTx(ctx, tx, dbID, d.ID, d.Revision, actorID, executionID, d.SQL, d.SQLDown, d.Irreversible, d.IrreversibleReason)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("versi attempt pertama = %d", version)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var up, storedDown, firstExecution string
	var irreversible bool
	if err := pool.QueryRow(ctx, `SELECT sql_up,sql_down,irreversible,first_execution_id
        FROM migration_revision_snapshots WHERE draft_id=$1 AND revision=$2`, d.ID, d.Revision).Scan(&up, &storedDown, &irreversible, &firstExecution); err != nil {
		t.Fatal(err)
	}
	if up != d.SQL || storedDown != down || irreversible || firstExecution != executionID {
		t.Fatalf("snapshot tidak sesuai: up=%q down=%q irreversible=%v execution=%q", up, storedDown, irreversible, firstExecution)
	}
	var sqlHistory, status string
	if err := pool.QueryRow(ctx, `SELECT sql,status FROM migration_history WHERE execution_id=$1`, executionID).Scan(&sqlHistory, &status); err != nil {
		t.Fatal(err)
	}
	if sqlHistory != up || status != "running" {
		t.Fatalf("history tidak sesuai: %q %q", sqlHistory, status)
	}
	// Status gagal tidak boleh menghapus snapshot attempt yang sudah tercatat.
	if _, err := pool.Exec(ctx, `UPDATE migration_history SET status='failed' WHERE execution_id=$1`, executionID); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE migration_revision_snapshots SET sql_down='SELECT 99' WHERE draft_id=$1`, d.ID)
	pastikanSQLState(t, err, "23514")
	_, err = pool.Exec(ctx, `DELETE FROM migration_revision_snapshots WHERE draft_id=$1`, d.ID)
	pastikanSQLState(t, err, "23514")
	_, err = svc.updateDraft(ctx, dbID, d.ID, draftInput{Title: "Revisi setelah attempt", SQL: "SELECT 88", SQLDown: &down, Revision: d.Revision})
	if !errors.Is(err, errDraftLocked) {
		t.Fatalf("definisi setelah snapshot masih bisa diubah: %v", err)
	}
}

func TestImmutableSnapshotGagalTidakMeninggalkanHistory(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	reason := "Data tidak bisa dipulihkan"
	d, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "Irreversible", SQL: "DELETE FROM users", Irreversible: true, IrreversibleReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Bentuk yang tidak valid sengaja dikirim untuk memastikan transaksi atomik.
	_, err = catatExecutionPertamaTx(ctx, tx, dbID, d.ID, d.Revision, actorID,
		"efefefefefefefefefefefefefefefef", d.SQL, nil, false, nil)
	pastikanSQLState(t, err, "23514")
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM migration_history WHERE draft_id=$1`, d.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("history tanpa snapshot tertinggal: count=%d err=%v", count, err)
	}
}

func TestImmutableSnapshotIrreversible(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	alasan := " Tidak ada backup data "
	d, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "Hapus data", SQL: "DELETE FROM users", Irreversible: true, IrreversibleReason: &alasan})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = catatExecutionPertamaTx(ctx, tx, dbID, d.ID, d.Revision, actorID,
		"dededededededededededededededede", d.SQL, d.SQLDown, d.Irreversible, d.IrreversibleReason)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var sqlDown *string
	var irreversible bool
	var reason string
	if err := pool.QueryRow(ctx, `SELECT sql_down,irreversible,irreversible_reason
        FROM migration_revision_snapshots WHERE draft_id=$1`, d.ID).Scan(&sqlDown, &irreversible, &reason); err != nil {
		t.Fatal(err)
	}
	if sqlDown != nil || !irreversible || reason != "Tidak ada backup data" {
		t.Fatalf("snapshot irreversible tidak sesuai: down=%v irreversible=%v reason=%q", sqlDown, irreversible, reason)
	}
}

func TestImmutableFlagPadaDraftAPI(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actorID, dbID := draftTestFixture(t, pool)
	svc := New(pool, nil, nil)
	down := "SELECT 2"
	d, err := svc.createDraft(ctx, dbID, actorID, draftInput{Title: "Preview UI", SQL: "SELECT 1", SQLDown: &down})
	if err != nil || d.Immutable {
		t.Fatalf("draft baru seharusnya belum immutable: %+v, %v", d, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO migration_history
		(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id)
		VALUES ($1,$2,$3,1,$4,$5,'failed','stage9immutable')`, dbID, d.ID, d.Revision, d.SQL, actorID); err != nil {
		t.Fatal(err)
	}
	read, err := svc.getDraft(ctx, dbID, d.ID)
	if err != nil || !read.Immutable {
		t.Fatalf("draft yang pernah dieksekusi harus immutable: %+v, %v", read, err)
	}
	items, _, _, err := svc.listDraftsPage(ctx, dbID, 0, 10)
	if err != nil || len(items) != 1 || !items[0].Immutable {
		t.Fatalf("daftar draft tidak mengembalikan immutable: %+v, %v", items, err)
	}
}
