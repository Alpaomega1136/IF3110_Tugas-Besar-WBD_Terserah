//go:build integration

package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/connections"
)

// siapkanApprovalAsync membuat fixture minimal dengan leader dan grant editor.
func siapkanApprovalAsync(t *testing.T, pool *pgxpool.Pool, actor, dbID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var groupID int64
	if err := pool.QueryRow(ctx, `INSERT INTO groups(name,leader_user_id) VALUES ('tim-async', $1) RETURNING id`, actor).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_groups(user_id,group_id) VALUES ($1,$2)`, actor, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES ($1,$2,'edit')`, groupID, dbID); err != nil {
		t.Fatal(err)
	}
	return groupID
}

func setujuiAsync(t *testing.T, pool *pgxpool.Pool, actor, groupID, dbID int64, draft Draft, action, target, sql string) int64 {
	t.Helper()
	ctx := context.Background()
	var reviewID int64
	var targetPtr *string
	if target != "" {
		targetPtr = &target
	}
	if err := pool.QueryRow(ctx, `INSERT INTO migration_reviews
		(database_id,draft_id,draft_revision,action_type,target_execution_id,sql_snapshot,status,requested_by)
		VALUES ($1,$2,$3,$4,$5,$6,'approved',$7) RETURNING id`,
		dbID, draft.ID, draft.Revision, action, targetPtr, sql, actor).Scan(&reviewID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO migration_review_approvals
		(review_id,group_id,access_level,leader_user_id,decision,decided_by,decided_at)
		VALUES ($1,$2,'edit',$3,'approved',$3,now())`, reviewID, groupID, actor); err != nil {
		t.Fatal(err)
	}
	return reviewID
}

func rencanaAsync(t *testing.T, pool *pgxpool.Pool, in AsyncOperationRequest) (AsyncOperationPlan, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	return PrepareAsyncOperationTx(ctx, tx, in)
}

func TestKontrakAsyncUpDanRevalidasi(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	groupID := siapkanApprovalAsync(t, pool, actor, dbID)
	up, down := "SELECT 1", "SELECT 2"
	draft, err := New(pool, nil, nil).createDraft(ctx, dbID, actor, draftInput{Title: "Up async", SQL: up, SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	reviewID := setujuiAsync(t, pool, actor, groupID, dbID, draft, "up", "", up)
	in := AsyncOperationRequest{DatabaseID: dbID, DraftID: draft.ID, Revision: draft.Revision, ActorID: actor, ReviewID: reviewID, ActionType: "up"}
	plan, err := rencanaAsync(t, pool, in)
	if err != nil || plan.SQL != up {
		t.Fatalf("snapshot Up salah: %+v err=%v", plan, err)
	}
	// Worker mencatat attempt running sebelum melakukan revalidasi setelah lock.
	simpanEventPosisi(t, pool, actor, dbID, draft, "execution-async", "running", up)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevalidateAsyncOperationTx(ctx, tx, in, "execution-async", up); err != nil {
		t.Fatalf("attempt sendiri tidak boleh memblokir worker: %v", err)
	}
	if err := RevalidateAsyncOperationTx(ctx, tx, in, "execution-async", "SELECT 999"); !errors.Is(err, errConflict) {
		t.Fatalf("SQL dalam job yang berubah harus ditolak: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Attempt pending lain tetap harus memblokir eksekusi.
	simpanEventPosisi(t, pool, actor, dbID, draft, "execution-lain", "unknown", up)
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevalidateAsyncOperationTx(ctx, tx, in, "execution-async", up); !errors.Is(err, errMigrationUnresolved) {
		t.Fatalf("pending lain harus memblokir: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Perubahan grant ketika job menunggu tidak boleh lolos revalidasi.
	if _, err := pool.Exec(ctx, `DELETE FROM group_database_permissions WHERE database_id=$1`, dbID); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := RevalidateAsyncOperationTx(ctx, tx, in, "execution-async", up); !errors.Is(err, connections.ErrForbidden) {
		t.Fatalf("akses dicabut harus ditolak: %v", err)
	}
}

func TestKontrakAsyncRollbackForward(t *testing.T) {
	ctx := context.Background()
	pool := draftTestDB(t)
	actor, dbID := draftTestFixture(t, pool)
	groupID := siapkanApprovalAsync(t, pool, actor, dbID)
	up, down := "SELECT 1", "SELECT 2"
	draft, err := New(pool, nil, nil).createDraft(ctx, dbID, actor, draftInput{Title: "Rollback async", SQL: up, SQLDown: &down})
	if err != nil {
		t.Fatal(err)
	}
	simpanEventPosisi(t, pool, actor, dbID, draft, "apply-async", "success", up)
	simpanSnapshotPosisi(t, pool, draft, "apply-async")
	reviewRollback := setujuiAsync(t, pool, actor, groupID, dbID, draft, "rollback", "apply-async", down)
	rollbackReq := AsyncOperationRequest{DatabaseID: dbID, DraftID: draft.ID, Revision: draft.Revision, ActorID: actor,
		ReviewID: reviewRollback, ActionType: "rollback", TargetExecutionID: "apply-async"}
	plan, err := rencanaAsync(t, pool, rollbackReq)
	if err != nil || plan.SQL != down {
		t.Fatalf("SQL Down immutable tidak digunakan: %+v err=%v", plan, err)
	}
	wrongReview := rollbackReq
	wrongReview.ReviewID = reviewRollback + 42
	if _, err := rencanaAsync(t, pool, wrongReview); !errors.Is(err, errConflict) {
		t.Fatalf("review lain harus ditolak: %v", err)
	}
	// Pasangan rollback sukses membuka forward memakai SQL Up asli.
	if _, err := pool.Exec(ctx, `INSERT INTO migration_history
		(database_id,draft_id,draft_revision,version,sql,executed_by,status,execution_id,action_type,target_execution_id)
		VALUES($1,$2,$3,2,$4,$5,'success','revert-async','rollback','apply-async')`,
		dbID, draft.ID, draft.Revision, down, actor); err != nil {
		t.Fatal(err)
	}
	simpanPairPosisi(t, pool, dbID, "apply-async", "revert-async")
	reviewForward := setujuiAsync(t, pool, actor, groupID, dbID, draft, "forward", "revert-async", up)
	forwardReq := AsyncOperationRequest{DatabaseID: dbID, DraftID: draft.ID, Revision: draft.Revision, ActorID: actor,
		ReviewID: reviewForward, ActionType: "forward", TargetExecutionID: "revert-async"}
	plan, err = rencanaAsync(t, pool, forwardReq)
	if err != nil || plan.SQL != up {
		t.Fatalf("SQL Up immutable tidak digunakan: %+v err=%v", plan, err)
	}
}
