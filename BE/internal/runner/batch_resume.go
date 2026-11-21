package runner

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"terserah/backend/internal/batch"
)

// Every retry has its own history row but keeps the first attempt's frozen scope.
func (s *Service) batchRoot(ctx context.Context, dbID int64, current pendingExecution, root string) (pendingExecution, error) {
	if current.execution == nil || root == "" {
		return pendingExecution{}, errBatchEvidence
	}
	var item pendingExecution
	err := s.db.QueryRow(ctx, `SELECT h.execution_id,h.version,h.sql,h.executed_by,h.receipt_version
 FROM migration_history h JOIN migration_history child ON child.batch_root_execution_id=h.execution_id
 WHERE h.execution_id=$1 AND h.database_id=$2 AND child.execution_id=$3
 AND child.database_id=h.database_id AND child.draft_id=h.draft_id AND child.draft_revision=h.draft_revision
 AND child.sql=h.sql AND child.executed_by=$4`, root, dbID, *current.execution, current.actor).Scan(&item.execution, &item.version, &item.sql, &item.actor, &item.format)
	if err != nil {
		return item, err
	}
	if item.sql != current.sql {
		return item, errBatchEvidence
	}
	item.isBatch = true
	return item, nil
}

func (s *Service) verifyBatchAttempt(ctx context.Context, tx pgx.Tx, dbID int64, current pendingExecution, root string) (Result, *time.Time, batch.Checkpoint, error) {
	if current.execution == nil {
		return Result{}, nil, batch.Checkpoint{}, errBatchEvidence
	}
	if root == "" || root == *current.execution {
		return s.verifyBatchPending(ctx, tx, dbID, current)
	}
	first, err := s.batchRoot(ctx, dbID, current, root)
	if err != nil {
		return Result{}, nil, batch.Checkpoint{}, err
	}
	out, finished, progress, err := s.verifyBatchPending(ctx, tx, dbID, first)
	if err != nil {
		return out, finished, progress, err
	}
	child, childFinished, err := s.verifyPendingAttempt(ctx, tx, dbID, current)
	if err != nil {
		return out, finished, progress, err
	}
	if child.Status == "unknown" || (child.Status == "success") != (out.Status == "success") {
		return Result{}, nil, progress, errBatchEvidence
	}
	if child.Status == "success" {
		return child, childFinished, progress, nil
	}
	out.ExecutionID, out.Version = *current.execution, current.version
	return out, finished, progress, nil
}

func loadBatchManifest(ctx context.Context, tx pgx.Tx, execution string) (batch.Manifest, bool, error) {
	m := batch.Manifest{ExecutionID: execution}
	err := tx.QueryRow(ctx, `SELECT database_id,version,executed_by,sql,table_oid,scope_count,scope_hash FROM _terserah.batch_runs WHERE execution_id=$1`, execution).Scan(&m.DatabaseID, &m.Version, &m.ActorID, &m.SQL, &m.TableOID, &m.ScopeCount, &m.ScopeHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}
