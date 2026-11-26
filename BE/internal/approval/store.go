package approval

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"terserah/backend/internal/policy"
	"terserah/backend/internal/telemetry"
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

// reviewSQLSnapshotTx menentukan SQL yang harus disetujui untuk satu aksi.
// Dipakai saat pengajuan dan keputusan agar review tidak mengacu pada definisi lama.
func reviewSQLSnapshotTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, action string, targetID *string) (string, error) {
	if !ValidActionType(action) {
		return "", ErrInvalidInput
	}
	if action == "compensate" {
		if targetID == nil || len(*targetID) != 32 {
			return "", ErrInvalidInput
		}
		var down string
		err := tx.QueryRow(ctx, `SELECT snap.sql_down FROM migration_jobs j
 JOIN migration_job_attempts a ON a.job_id=j.id AND a.attempt_no=j.attempt_count
 JOIN migration_revision_snapshots snap ON snap.draft_id=j.draft_id AND snap.revision=j.draft_revision
 WHERE j.database_id=$1 AND j.draft_id=$2 AND j.draft_revision=$3 AND a.execution_id=$4
 AND j.execution_mode='batch' AND NOT snap.irreversible AND snap.sql_down IS NOT NULL
 AND j.status IN ('partial','success','partially_reverted')
 AND ((j.action_type='compensate' AND j.status='partially_reverted') OR j.action_type<>'compensate') FOR UPDATE OF j`, dbID, draftID, revision, *targetID).Scan(&down)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrInvalidInput
		}
		return down, err
	}
	if action == "resume" {
		if targetID == nil || len(*targetID) != 32 {
			return "", ErrInvalidInput
		}
		var snapshot string
		err := tx.QueryRow(ctx, `SELECT j.sql_snapshot FROM migration_jobs j
 JOIN migration_job_attempts a ON a.job_id=j.id AND a.attempt_no=j.attempt_count
 JOIN migration_history h ON h.execution_id=a.execution_id
 WHERE j.database_id=$1 AND j.draft_id=$2 AND j.draft_revision=$3 AND a.execution_id=$4
 AND j.execution_mode='batch' AND j.status='partial' AND j.committed_batches>0
 AND a.status IN ('partial','failed') AND h.status IN ('partial','failed') FOR UPDATE OF j`, dbID, draftID, revision, *targetID).Scan(&snapshot)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return snapshot, err
	}
	var sqlUp string
	var currentRevision int64
	err := tx.QueryRow(ctx, `SELECT sql,revision FROM migration_drafts
		WHERE database_id=$1 AND id=$2 FOR UPDATE`, dbID, draftID).Scan(&sqlUp, &currentRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if currentRevision != revision {
		return "", ErrRevisionConflict
	}
	if action != "rollback" && action != "forward" {
		if action == "up" && targetID != nil {
			return "", ErrInvalidInput
		}
		return sqlUp, nil
	}
	if targetID == nil || *targetID == "" || len(*targetID) > 128 {
		return "", ErrInvalidInput
	}
	var previousAction, previousStatus, immutableUp string
	var immutableDown *string
	var irreversible bool
	err = tx.QueryRow(ctx, `
		SELECT h.action_type,h.status,snap.sql_up,snap.sql_down,snap.irreversible
		FROM migration_history h
		JOIN migration_revision_snapshots snap ON snap.draft_id=h.draft_id AND snap.revision=h.draft_revision
		WHERE h.database_id=$1 AND h.draft_id=$2 AND h.draft_revision=$3 AND h.execution_id=$4
	`, dbID, draftID, revision, *targetID).Scan(&previousAction, &previousStatus, &immutableUp, &immutableDown, &irreversible)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidInput
	}
	if err != nil {
		return "", err
	}
	if previousStatus != "success" {
		return "", ErrInvalidInput
	}
	if action == "rollback" {
		if (previousAction != "up" && previousAction != "forward") || irreversible || immutableDown == nil {
			return "", ErrInvalidInput
		}
		return *immutableDown, nil
	}
	if previousAction != "rollback" && previousAction != "compensate" {
		return "", ErrInvalidInput
	}
	if previousAction == "compensate" {
		var paired bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_rollback_pairs WHERE database_id=$1 AND rollback_execution_id=$2)`, dbID, *targetID).Scan(&paired); err != nil {
			return "", err
		}
		if !paired {
			return "", ErrInvalidInput
		}
	}
	return immutableUp, nil
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
	defer telemetry.Rollback(ctx, tx)

	var lockedDB int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, dbID).Scan(&lockedDB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewView{}, ErrNotFound
		}
		return ReviewView{}, err
	}

	// Preserve historical resume coordinates and immutable rollback/forward SQL.
	sqlSnapshot, err := reviewSQLSnapshotTx(ctx, tx, dbID, input.DraftID, input.Revision, input.ActionType, input.TargetExecutionID)
	if err != nil {
		return ReviewView{}, err
	}

	groups, err := inspectDatabaseGroups(ctx, tx, dbID)
	if err != nil {
		return ReviewView{}, err
	}
	_, approvalErr := ensureApprovedTx(ctx, tx, dbID, input.DraftID, input.Revision, input.ActionType, 0, input.TargetExecutionID, &sqlSnapshot)
	if errors.Is(approvalErr, ErrNotApproved) {
		_, err = tx.Exec(ctx, `UPDATE migration_reviews SET status = 'cancelled', decided_at = $5 WHERE database_id = $1 AND draft_id = $2 AND draft_revision = $3 AND action_type = $4 AND status = 'approved' AND target_execution_id IS NOT DISTINCT FROM $6::text`, dbID, input.DraftID, input.Revision, input.ActionType, now, input.TargetExecutionID)
		if err != nil {
			return ReviewView{}, err
		}
	} else if approvalErr != nil {
		return ReviewView{}, approvalErr
	}

	var duplicate bool
	// A terminal job may obtain fresh approval without losing its previous decisions.
	_, err = tx.Exec(ctx, `UPDATE migration_reviews r SET status='cancelled',decided_at=$6
 WHERE r.database_id=$1 AND r.draft_id=$2 AND r.draft_revision=$3 AND r.action_type=$4
 AND r.target_execution_id IS NOT DISTINCT FROM $5::text AND r.status='queued'
 AND EXISTS(SELECT 1 FROM migration_jobs j WHERE (j.review_id=r.id OR EXISTS(
 SELECT 1 FROM migration_retry_authorizations a WHERE a.job_id=j.id AND a.review_id=r.id))
 AND j.status='failed' AND j.execution_mode='normal')`,
		dbID, input.DraftID, input.Revision, input.ActionType, input.TargetExecutionID, now)
	if err != nil {
		return ReviewView{}, err
	}
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
	defer telemetry.Rollback(ctx, tx)
	var lockedDB int64
	if err := tx.QueryRow(ctx, `SELECT id FROM database_connections WHERE id = $1 FOR UPDATE`, dbID).Scan(&lockedDB); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewView{}, ErrNotFound
		}
		return ReviewView{}, err
	}

	var status, action, sqlSnapshot string
	var draftID, revision int64
	var targetID *string
	err = tx.QueryRow(ctx, `
		SELECT status,draft_id,draft_revision,action_type,target_execution_id,sql_snapshot
		FROM migration_reviews
		WHERE id = $1 AND database_id = $2
		FOR UPDATE
	`, reviewID, dbID).Scan(&status, &draftID, &revision, &action, &targetID, &sqlSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewView{}, ErrNotFound
	}
	if err != nil {
		return ReviewView{}, err
	}
	if status != StatusPendingReview {
		return ReviewView{}, ErrReviewClosed
	}
	if approve {
		// Review lama tidak boleh disetujui setelah SQL, revisi, atau target berubah.
		currentSQL, err := reviewSQLSnapshotTx(ctx, tx, dbID, draftID, revision, action, targetID)
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrRevisionConflict) {
				return ReviewView{}, ErrRevisionConflict
			}
			return ReviewView{}, err
		}
		if currentSQL != sqlSnapshot {
			return ReviewView{}, ErrRevisionConflict
		}
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
	defer telemetry.Rollback(ctx, tx)

	return loadReviewTx(ctx, tx, actorID, dbID, reviewID, true)
}

// reviewActionableSQL uses migration_reviews r and the actor ID as $1.
const reviewActionableSQL = `r.status = 'pending_review' AND EXISTS (
    SELECT 1 FROM group_database_permissions p
    JOIN groups g ON g.id = p.group_id
    JOIN users u ON u.id = g.leader_user_id AND u.is_active
    JOIN user_groups ug ON ug.group_id = g.id AND ug.user_id = u.id
    LEFT JOIN migration_review_approvals a ON a.review_id = r.id AND a.group_id = g.id
    WHERE p.database_id = r.database_id AND g.leader_user_id = $1
      AND (a.group_id IS NULL OR a.leader_user_id <> g.leader_user_id OR a.decision = 'pending')
)`

func (s *PostgresStore) ListReviews(ctx context.Context, actorID, dbID int64, options ReviewListOptions) (ReviewPage, error) {
	if dbID != 0 {
		access, err := policy.DatabaseAccess(ctx, s.pool, actorID, dbID)
		if err != nil {
			return ReviewPage{}, err
		}
		if !policy.Allows(access, false) {
			return ReviewPage{}, ErrForbidden
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ReviewPage{}, err
	}
	defer telemetry.Rollback(ctx, tx)

	countFirstPage := options.OnlyActionable && options.BeforeID == 0
	totalSQL := "0::bigint"
	if countFirstPage {
		totalSQL = "count(*) OVER ()"
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.database_id, `+totalSQL+`
		FROM migration_reviews r
		WHERE ($2::bigint = 0 OR r.database_id = $2)
		  AND ($3::bigint = 0 OR r.id < $3)
		  AND ($4::text = '' OR r.status = $4)
		  AND (NOT $5 OR (`+reviewActionableSQL+`))
		  AND EXISTS (
			SELECT 1
			FROM group_database_permissions p
			JOIN user_groups ug ON ug.group_id = p.group_id
			JOIN users u ON u.id = ug.user_id AND u.is_active
			WHERE p.database_id = r.database_id AND ug.user_id = $1
		  )
		ORDER BY r.id DESC
		LIMIT $6
	`, actorID, dbID, options.BeforeID, options.Status, options.OnlyActionable, options.Limit+1)
	if err != nil {
		return ReviewPage{}, err
	}
	type entry struct{ ID, DBID, Total int64 }
	entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
	if err != nil {
		return ReviewPage{}, err
	}

	page := ReviewPage{HasMore: len(entries) > options.Limit}
	if countFirstPage {
		var total int64
		if len(entries) > 0 {
			total = entries[0].Total
		}
		page.Total = &total
	}
	if page.HasMore {
		entries = entries[:options.Limit]
		next := strconv.FormatInt(entries[len(entries)-1].ID, 10)
		page.NextBefore = &next
	}
	page.Reviews = make([]ReviewView, 0, len(entries))
	for _, entry := range entries {
		item, err := loadReviewTx(ctx, tx, actorID, entry.DBID, entry.ID, false)
		if err != nil {
			return ReviewPage{}, err
		}
		page.Reviews = append(page.Reviews, item)
	}
	return page, nil
}

// EnsureApproved verifies that an approved review exists for (dbID, draftID, revision, actionType)
// and that every group currently holding view or edit access to dbID has approved it.
func (s *PostgresStore) EnsureApproved(ctx context.Context, dbID, draftID, revision int64, actionType string) (ReviewView, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewView{}, err
	}
	defer telemetry.Rollback(ctx, tx)
	review, err := EnsureApprovedTx(ctx, tx, dbID, draftID, revision, actionType)
	if err != nil {
		return ReviewView{}, err
	}
	return review, tx.Commit(ctx)
}

// EnsureApprovedTx hanya untuk aksi tanpa target. Rollback/forward wajib memakai
// EnsureApprovedExecutionTx agar target execution dan SQL immutable ikut diperiksa.
func EnsureApprovedTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType string) (ReviewView, error) {
	if actionType == "rollback" || actionType == "forward" {
		return ReviewView{}, ErrNotApproved
	}
	return ensureApprovedTx(ctx, tx, dbID, draftID, revision, actionType, 0, nil, nil)
}

// EnsureApprovedReviewTx validates the precise approved payload, including its execution target.
func EnsureApprovedReviewTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType string, reviewID int64, target *string) (ReviewView, error) {
	return ensureApprovedTx(ctx, tx, dbID, draftID, revision, actionType, reviewID, target, nil)
}

// EnsureApprovedExecutionTx binds both the immutable SQL and the exact target.
func EnsureApprovedExecutionTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType, targetExecutionID, sqlText string) (ReviewView, error) {
	if (actionType != "rollback" && actionType != "forward") || targetExecutionID == "" || sqlText == "" {
		return ReviewView{}, ErrNotApproved
	}
	return ensureApprovedTx(ctx, tx, dbID, draftID, revision, actionType, 0, &targetExecutionID, &sqlText)
}

func ensureApprovedTx(ctx context.Context, tx pgx.Tx, dbID, draftID, revision int64, actionType string, requestedID int64, target, sqlText *string) (ReviewView, error) {
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
          AND ($7::text IS NULL OR sql_snapshot=$7)
          AND ($4<>'up' OR sql_snapshot=(SELECT sql FROM migration_drafts WHERE database_id=$1 AND id=$2 AND revision=$3))
		  AND status IN ('approved', 'queued')
		ORDER BY id DESC
		LIMIT 1
		FOR UPDATE
	`, dbID, draftID, revision, actionType, requestedID, target, sqlText).Scan(&reviewID)
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
	return loadReviewTx(ctx, tx, 0, dbID, reviewID, true)
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
			CASE WHEN $4 THEN r.sql_snapshot ELSE '' END,
			r.status,
			r.requested_by,
			u.username,
			r.rejection_reason,
			r.created_at,
			r.decided_at,
			`+reviewActionableSQL+`
		FROM migration_reviews r
		JOIN database_connections dc ON dc.id = r.database_id
		JOIN migration_drafts d ON d.id = r.draft_id
		JOIN users u ON u.id = r.requested_by
		WHERE r.id = $2 AND r.database_id = $3
	`, actorID, reviewID, dbID, includeSQL).Scan(
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
		&rv.CanCurrentUserAct,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewView{}, ErrNotFound
	}
	if err != nil {
		return ReviewView{}, err
	}

	rv.Approvals, err = loadApprovalsTx(ctx, tx, reviewID)
	return rv, err
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
