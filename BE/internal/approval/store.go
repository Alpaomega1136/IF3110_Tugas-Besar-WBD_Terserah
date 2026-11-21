package approval

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/policy"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

type requiredGroup struct {
	groupID        int64
	groupName      string
	accessLevel    policy.Access
	leaderUserID   *int64
	leaderUsername *string
	leaderActive   *bool
	leaderMember   bool
}

func inspectDatabaseGroups(ctx context.Context, tx pgx.Tx, dbID int64) ([]requiredGroup, error) {
	rows, err := tx.Query(ctx, `
		SELECT
			g.id,
			g.name,
			p.access_level,
			g.leader_user_id,
			u.username,
			u.is_active,
			EXISTS(
				SELECT 1 FROM user_groups ug
				WHERE ug.group_id = g.id AND ug.user_id = g.leader_user_id
			) AS is_member
		FROM group_database_permissions p
		JOIN groups g ON g.id = p.group_id
		LEFT JOIN users u ON u.id = g.leader_user_id
		WHERE p.database_id = $1
		ORDER BY g.id ASC
		FOR UPDATE OF g
	`, dbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := []requiredGroup{}
	for rows.Next() {
		var rg requiredGroup
		var lvl string
		if err := rows.Scan(
			&rg.groupID,
			&rg.groupName,
			&lvl,
			&rg.leaderUserID,
			&rg.leaderUsername,
			&rg.leaderActive,
			&rg.leaderMember,
		); err != nil {
			return nil, err
		}
		rg.accessLevel = policy.Access(lvl)
		if rg.leaderUserID == nil || rg.leaderActive == nil || !*rg.leaderActive || !rg.leaderMember {
			return nil, ErrLeaderMissing
		}
		groups = append(groups, rg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, ErrLeaderMissing
	}
	return groups, nil
}

func (s *PostgresStore) SubmitReview(ctx context.Context, actorID, dbID int64, input SubmitInput, now time.Time) (ReviewView, error) {
	access, err := policy.DatabaseAccess(ctx, s.pool, actorID, dbID)
	if err != nil {
		return ReviewView{}, err
	}
	if !policy.Allows(access, true) {
		return ReviewView{}, ErrForbidden
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewView{}, err
	}
	defer tx.Rollback(context.Background())

	var lockedDB int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, dbID).Scan(&lockedDB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewView{}, ErrNotFound
		}
		return ReviewView{}, err
	}

	var sqlSnapshot string
	var actualRevision int64
	if input.ActionType == "resume" {
		if input.TargetExecutionID == nil || len(*input.TargetExecutionID) != 32 {
			return ReviewView{}, ErrInvalidInput
		}
		err = tx.QueryRow(ctx, `SELECT j.sql_snapshot,j.draft_revision FROM migration_jobs j
 JOIN migration_job_attempts a ON a.job_id=j.id AND a.attempt_no=j.attempt_count
 JOIN migration_history h ON h.execution_id=a.execution_id
 WHERE j.database_id=$1 AND j.draft_id=$2 AND j.draft_revision=$3 AND a.execution_id=$4
 AND j.execution_mode='batch' AND j.status='partial' AND j.committed_batches>0
 AND a.status IN ('partial','failed') AND h.status IN ('partial','failed')
 FOR UPDATE OF j`, dbID, input.DraftID, input.Revision, *input.TargetExecutionID).Scan(&sqlSnapshot, &actualRevision)
	} else {
		err = tx.QueryRow(ctx, `
		SELECT sql, revision
		FROM migration_drafts
		WHERE database_id = $1 AND id = $2
		FOR UPDATE
	`, dbID, input.DraftID).Scan(&sqlSnapshot, &actualRevision)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewView{}, ErrNotFound
	}
	if err != nil {
		return ReviewView{}, err
	}
	if actualRevision != input.Revision {
		return ReviewView{}, ErrRevisionConflict
	}

	groups, err := inspectDatabaseGroups(ctx, tx, dbID)
	if err != nil {
		return ReviewView{}, err
	}
	_, approvalErr := ensureApprovedTx(ctx, tx, dbID, input.DraftID, input.Revision, input.ActionType, 0, input.TargetExecutionID)
	if errors.Is(approvalErr, ErrNotApproved) {
		_, err = tx.Exec(ctx, `UPDATE migration_reviews SET status = 'cancelled', decided_at = $5 WHERE database_id = $1 AND draft_id = $2 AND draft_revision = $3 AND action_type = $4 AND status = 'approved' AND target_execution_id IS NOT DISTINCT FROM $6::text`, dbID, input.DraftID, input.Revision, input.ActionType, now, input.TargetExecutionID)
		if err != nil {
			return ReviewView{}, err
		}
	} else if approvalErr != nil {
		return ReviewView{}, approvalErr
	}

	var duplicate bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM migration_reviews
			WHERE database_id = $1
			  AND draft_id = $2
			  AND draft_revision = $3
			  AND action_type = $4
			  AND target_execution_id IS NOT DISTINCT FROM $5::text
			  AND status IN ('pending_review', 'approved', 'queued')
		)
	`, dbID, input.DraftID, input.Revision, input.ActionType, input.TargetExecutionID).Scan(&duplicate)
	if err != nil {
		return ReviewView{}, err
	}
	if duplicate {
		return ReviewView{}, ErrRevisionConflict
	}

	var reviewID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO migration_reviews (
			database_id, draft_id, draft_revision, action_type,
			target_execution_id, sql_snapshot, status, requested_by, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending_review', $7, $8)
		RETURNING id
	`, dbID, input.DraftID, input.Revision, input.ActionType, input.TargetExecutionID, sqlSnapshot, actorID, now).Scan(&reviewID)
	if err != nil {
		return ReviewView{}, err
	}

	for _, rg := range groups {
		_, err = tx.Exec(ctx, `
			INSERT INTO migration_review_approvals (
				review_id, group_id, access_level, leader_user_id, decision
			)
			VALUES ($1, $2, $3, $4, 'pending')
		`, reviewID, rg.groupID, string(rg.accessLevel), *rg.leaderUserID)
		if err != nil {
			return ReviewView{}, err
		}
	}

	review, err := loadReviewTx(ctx, tx, actorID, dbID, reviewID, true)
	if err != nil {
		return ReviewView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewView{}, err
	}
	return review, nil
}

func (s *PostgresStore) DecideReview(ctx context.Context, actorID, dbID, reviewID int64, approve bool, note string, now time.Time) (ReviewView, error) {
	access, err := policy.DatabaseAccess(ctx, s.pool, actorID, dbID)
	if err != nil {
		return ReviewView{}, err
	}
	if !policy.Allows(access, false) {
		return ReviewView{}, ErrForbidden
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewView{}, err
	}
	defer tx.Rollback(context.Background())
	var lockedDB int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, dbID).Scan(&lockedDB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewView{}, ErrNotFound
		}
		return ReviewView{}, err
	}

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status
		FROM migration_reviews
		WHERE id = $1 AND database_id = $2
		FOR UPDATE
	`, reviewID, dbID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewView{}, ErrNotFound
	}
	if err != nil {
		return ReviewView{}, err
	}
	if status != StatusPendingReview {
		return ReviewView{}, ErrReviewClosed
	}

	groups, err := inspectDatabaseGroups(ctx, tx, dbID)
	if err != nil {
		return ReviewView{}, err
	}

	// Ensure any group granted access after submission is also tracked on this review,
	// and sync the current group leader if the leader was reassigned while pending.
	for _, rg := range groups {
		_, err = tx.Exec(ctx, `
			INSERT INTO migration_review_approvals (
				review_id, group_id, access_level, leader_user_id, decision
			)
			VALUES ($1, $2, $3, $4, 'pending')
			ON CONFLICT (review_id, group_id)
			DO UPDATE SET
				access_level = EXCLUDED.access_level,
				decision = CASE WHEN migration_review_approvals.leader_user_id <> EXCLUDED.leader_user_id THEN 'pending' ELSE migration_review_approvals.decision END,
				note = CASE WHEN migration_review_approvals.leader_user_id <> EXCLUDED.leader_user_id THEN NULL ELSE migration_review_approvals.note END,
				decided_by = CASE WHEN migration_review_approvals.leader_user_id <> EXCLUDED.leader_user_id THEN NULL ELSE migration_review_approvals.decided_by END,
				decided_at = CASE WHEN migration_review_approvals.leader_user_id <> EXCLUDED.leader_user_id THEN NULL ELSE migration_review_approvals.decided_at END,
				leader_user_id = EXCLUDED.leader_user_id
		`, reviewID, rg.groupID, string(rg.accessLevel), *rg.leaderUserID)
		if err != nil {
			return ReviewView{}, err
		}
	}

	decision := DecisionApproved
	if !approve {
		decision = DecisionRejected
	}
	var notePtr *string
	if note != "" {
		notePtr = &note
	}

	tag, err := tx.Exec(ctx, `
		UPDATE migration_review_approvals
		SET decision = $3, note = $4, decided_by = $2, decided_at = $5
		WHERE review_id = $1
		  AND leader_user_id = $2
		  AND decision = 'pending'
		  AND EXISTS (SELECT 1 FROM group_database_permissions p WHERE p.group_id = migration_review_approvals.group_id AND p.database_id = $6)
	`, reviewID, actorID, decision, notePtr, now, dbID)
	if err != nil {
		return ReviewView{}, err
	}
	if tag.RowsAffected() == 0 {
		return ReviewView{}, ErrNotApprover
	}

	approvals, err := loadApprovalsTx(ctx, tx, reviewID)
	if err != nil {
		return ReviewView{}, err
	}
	requiredIDs := make([]int64, 0, len(groups))
	for _, rg := range groups {
		requiredIDs = append(requiredIDs, rg.groupID)
	}

	allApproved, anyRejected := EvaluateCompleteness(requiredIDs, approvals)
	if anyRejected {
		_, err = tx.Exec(ctx, `
			UPDATE migration_reviews
			SET status = 'rejected', rejection_reason = $2, decided_at = $3
			WHERE id = $1
		`, reviewID, notePtr, now)
		if err != nil {
			return ReviewView{}, err
		}
	} else if allApproved {
		_, err = tx.Exec(ctx, `
			UPDATE migration_reviews
			SET status = 'approved', decided_at = $2
			WHERE id = $1
		`, reviewID, now)
		if err != nil {
			return ReviewView{}, err
		}
	}

	review, err := loadReviewTx(ctx, tx, actorID, dbID, reviewID, true)
	if err != nil {
		return ReviewView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewView{}, err
	}
	return review, nil
}

func (s *PostgresStore) GetReview(ctx context.Context, actorID, dbID, reviewID int64) (ReviewView, error) {
	access, err := policy.DatabaseAccess(ctx, s.pool, actorID, dbID)
	if err != nil {
		return ReviewView{}, err
	}
	if !policy.Allows(access, false) {
		return ReviewView{}, ErrForbidden
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReviewView{}, err
	}
	defer tx.Rollback(context.Background())

	return loadReviewTx(ctx, tx, actorID, dbID, reviewID, true)
}

func (s *PostgresStore) ListDatabaseReviews(ctx context.Context, actorID, dbID, beforeID int64, limit int) ([]ReviewView, bool, *string, error) {
	access, err := policy.DatabaseAccess(ctx, s.pool, actorID, dbID)
	if err != nil {
		return nil, false, nil, err
	}
	if !policy.Allows(access, false) {
		return nil, false, nil, ErrForbidden
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, nil, err
	}
	defer tx.Rollback(context.Background())

	rows, err := tx.Query(ctx, `
		SELECT id
		FROM migration_reviews
		WHERE database_id = $1 AND ($2::bigint = 0 OR id < $2)
		ORDER BY id DESC
		LIMIT $3
	`, dbID, beforeID, limit+1)
	if err != nil {
		return nil, false, nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, nil, err
	}

	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}
	items := make([]ReviewView, 0, len(ids))
	for _, id := range ids {
		item, err := loadReviewTx(ctx, tx, actorID, dbID, id, false)
		if err != nil {
			return nil, false, nil, err
		}
		items = append(items, item)
	}
	var next *string
	if more {
		val := strconv.FormatInt(items[len(items)-1].ID, 10)
		next = &val
	}
	return items, more, next, nil
}

func (s *PostgresStore) ListAssignedReviews(ctx context.Context, actorID int64) ([]ReviewView, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT r.id, r.database_id
		FROM migration_reviews r
		JOIN group_database_permissions p ON p.database_id = r.database_id
		JOIN user_groups ug ON ug.group_id = p.group_id
		JOIN users u ON u.id = ug.user_id
		WHERE ug.user_id = $1 AND u.is_active = true
		ORDER BY r.id DESC
		LIMIT 100
	`, actorID)
	if err != nil {
		return nil, err
	}
	type pair struct{ id, dbID int64 }
	pairs := []pair{}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.dbID); err != nil {
			rows.Close()
			return nil, err
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	items := make([]ReviewView, 0, len(pairs))
	for _, p := range pairs {
		item, err := loadReviewTx(ctx, tx, actorID, p.dbID, p.id, false)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// EnsureApproved verifies that an approved review exists for (dbID, draftID, revision, actionType)
// and that every group currently holding view or edit access to dbID has approved it.
func (s *PostgresStore) EnsureApproved(ctx context.Context, dbID, draftID, revision int64, actionType string) (ReviewView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewView{}, err
	}
	defer tx.Rollback(context.Background())
	review, err := EnsureApprovedTx(ctx, tx, dbID, draftID, revision, actionType)
	if err != nil {
		return ReviewView{}, err
	}
	return review, tx.Commit(ctx)
}

// EnsureApprovedTx keeps approval and group locks until the caller commits.
func EnsureApprovedTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType string) (ReviewView, error) {
	return ensureApprovedTx(ctx, tx, dbID, draftID, revision, actionType, 0, nil)
}

// EnsureApprovedReviewTx validates the precise approved payload, including its execution target.
func EnsureApprovedReviewTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType string, reviewID int64, target *string) (ReviewView, error) {
	return ensureApprovedTx(ctx, tx, dbID, draftID, revision, actionType, reviewID, target)
}

func ensureApprovedTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType string, requestedID int64, target *string) (ReviewView, error) {
	groups, err := inspectDatabaseGroups(ctx, tx, dbID)
	if err != nil {
		return ReviewView{}, err
	}

	var reviewID int64
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM migration_reviews
		WHERE database_id = $1
		  AND draft_id = $2
		  AND draft_revision = $3
		  AND action_type = $4
		  AND ($5::bigint=0 OR id=$5)
		  AND target_execution_id IS NOT DISTINCT FROM $6::text
		  AND status IN ('approved', 'queued')
		ORDER BY id DESC
		LIMIT 1
		FOR UPDATE
	`, dbID, draftID, revision, actionType, requestedID, target).Scan(&reviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewView{}, ErrNotApproved
	}
	if err != nil {
		return ReviewView{}, err
	}

	approvals, err := loadApprovalsTx(ctx, tx, reviewID)
	if err != nil {
		return ReviewView{}, err
	}
	requiredIDs := make([]int64, 0, len(groups))
	for _, rg := range groups {
		requiredIDs = append(requiredIDs, rg.groupID)
		for _, a := range approvals {
			if a.GroupID == rg.groupID && a.LeaderUserID != *rg.leaderUserID {
				return ReviewView{}, ErrNotApproved
			}
		}
	}
	allApproved, anyRejected := EvaluateCompleteness(requiredIDs, approvals)
	if anyRejected || !allApproved {
		return ReviewView{}, ErrNotApproved
	}

	review, err := loadReviewTx(ctx, tx, 0, dbID, reviewID, true)
	if err != nil {
		return ReviewView{}, err
	}
	return review, nil
}

func loadReviewTx(ctx context.Context, tx pgx.Tx, actorID, dbID, reviewID int64, includeSQL bool) (ReviewView, error) {
	var rv ReviewView
	err := tx.QueryRow(ctx, `
		SELECT
			r.id,
			r.database_id,
			dc.name,
			r.draft_id,
			d.title,
			r.draft_revision,
			r.action_type,
			r.target_execution_id,
			CASE WHEN $3 THEN r.sql_snapshot ELSE '' END,
			r.status,
			r.requested_by,
			u.username,
			r.rejection_reason,
			r.created_at,
			r.decided_at
		FROM migration_reviews r
		JOIN database_connections dc ON dc.id = r.database_id
		JOIN migration_drafts d ON d.id = r.draft_id
		JOIN users u ON u.id = r.requested_by
		WHERE r.id = $1 AND r.database_id = $2
	`, reviewID, dbID, includeSQL).Scan(
		&rv.ID,
		&rv.DatabaseID,
		&rv.DatabaseName,
		&rv.DraftID,
		&rv.DraftTitle,
		&rv.DraftRevision,
		&rv.ActionType,
		&rv.TargetExecutionID,
		&rv.SQLSnapshot,
		&rv.Status,
		&rv.RequestedBy,
		&rv.RequestedByUsername,
		&rv.RejectionReason,
		&rv.CreatedAt,
		&rv.DecidedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewView{}, ErrNotFound
	}
	if err != nil {
		return ReviewView{}, err
	}

	rv.Approvals, err = loadApprovalsTx(ctx, tx, reviewID)
	if err != nil {
		return ReviewView{}, err
	}
	if rv.Status == StatusPendingReview && actorID > 0 {
		err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM group_database_permissions p
			JOIN groups g ON g.id = p.group_id
			JOIN users u ON u.id = g.leader_user_id AND u.is_active
			JOIN user_groups ug ON ug.group_id = g.id AND ug.user_id = u.id
			LEFT JOIN migration_review_approvals a ON a.review_id = $1 AND a.group_id = g.id
			WHERE p.database_id = $2 AND g.leader_user_id = $3
			  AND (a.group_id IS NULL OR a.leader_user_id <> g.leader_user_id OR a.decision = 'pending')
		)`, reviewID, dbID, actorID).Scan(&rv.CanCurrentUserAct)
		if err != nil {
			return ReviewView{}, err
		}
	}
	return rv, nil
}

func loadApprovalsTx(ctx context.Context, tx pgx.Tx, reviewID int64) ([]GroupApprovalView, error) {
	rows, err := tx.Query(ctx, `
		SELECT
			a.group_id,
			g.name,
			a.access_level,
			a.leader_user_id,
			u.username,
			a.decision,
			a.note,
			a.decided_by,
			a.decided_at
		FROM migration_review_approvals a
		JOIN groups g ON g.id = a.group_id
		JOIN users u ON u.id = a.leader_user_id
		WHERE a.review_id = $1
		ORDER BY a.group_id ASC
	`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []GroupApprovalView{}
	for rows.Next() {
		var item GroupApprovalView
		var lvl string
		if err := rows.Scan(
			&item.GroupID,
			&item.GroupName,
			&lvl,
			&item.LeaderUserID,
			&item.LeaderUsername,
			&item.Decision,
			&item.Note,
			&item.DecidedBy,
			&item.DecidedAt,
		); err != nil {
			return nil, err
		}
		item.AccessLevel = policy.Access(lvl)
		items = append(items, item)
	}
	return items, rows.Err()
}
