-- Old queued definitions have no Down SQL; preserve them explicitly as irreversible.
UPDATE migration_drafts d SET irreversible=true,
    irreversible_reason='Legacy draft: SQL Down was not defined before the Milestone 2 upgrade'
WHERE d.sql_down IS NULL AND NOT d.irreversible
    AND NOT EXISTS(SELECT 1 FROM migration_history h WHERE h.draft_id=d.id)
    AND NOT EXISTS(SELECT 1 FROM migration_revision_snapshots snap WHERE snap.draft_id=d.id);

ALTER TABLE migration_jobs DROP CONSTRAINT migration_jobs_action_type_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_action_type_check
    CHECK (action_type IN ('up','resume','rollback','forward'));
ALTER TABLE migration_jobs DROP CONSTRAINT resume_snapshot;
ALTER TABLE migration_jobs ADD CONSTRAINT resume_snapshot CHECK (
    (action_type='up' AND parent_job_id IS NULL AND target_execution_id IS NULL)
    OR (action_type='resume' AND parent_job_id IS NOT NULL AND parent_job_id<>id
        AND target_execution_id IS NOT NULL AND execution_mode='batch' AND batch_root_execution_id IS NOT NULL)
    OR (action_type IN ('rollback','forward') AND parent_job_id IS NULL
        AND target_execution_id IS NOT NULL AND execution_mode='normal')
);

-- Recovery and normal finalization must expose rollback success and its pair atomically.
CREATE FUNCTION pair_successful_queued_rollback() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status='success' AND NEW.action_type='rollback' AND NEW.target_execution_id IS NOT NULL THEN
        INSERT INTO migration_rollback_pairs(database_id,applied_execution_id,rollback_execution_id)
        VALUES(NEW.database_id,NEW.target_execution_id,NEW.execution_id)
        ON CONFLICT(rollback_execution_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER migration_history_pair_rollback
    AFTER UPDATE OF status ON migration_history
    FOR EACH ROW EXECUTE FUNCTION pair_successful_queued_rollback();
