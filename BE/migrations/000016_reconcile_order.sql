ALTER TABLE migration_history ADD COLUMN last_reconciled_at timestamptz;

DROP INDEX migration_history_pending_idx;
CREATE INDEX migration_history_pending_idx
    ON migration_history (database_id, (COALESCE(last_reconciled_at, started_at)), version)
    WHERE status IN ('running', 'unknown', 'partial', 'partially_reverted');
