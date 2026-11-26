DROP INDEX migration_history_draft_revision_idx;
CREATE INDEX migration_history_draft_revision_idx ON migration_history(database_id,draft_id,draft_revision);
ALTER TABLE migration_job_attempts ADD COLUMN retry_scheduled boolean NOT NULL DEFAULT false;
ALTER TABLE migration_history ADD COLUMN batch_root_execution_id text REFERENCES migration_history(execution_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE migration_jobs ADD COLUMN batch_root_execution_id text REFERENCES migration_history(execution_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE migration_jobs ADD CONSTRAINT batch_root_mode CHECK (batch_root_execution_id IS NULL OR execution_mode='batch');
ALTER TABLE migration_jobs DROP CONSTRAINT migration_jobs_status_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_status_check CHECK (status IN ('queued','running','success','failed','unknown','partial','recovering','retry_wait'));
DROP INDEX migration_jobs_active_database_idx;
CREATE UNIQUE INDEX migration_jobs_active_database_idx ON migration_jobs(database_id) WHERE status IN ('running','unknown','partial','recovering','retry_wait');
CREATE INDEX migration_jobs_recovery_idx ON migration_jobs(available_at,id) WHERE status IN ('running','unknown','recovering');
UPDATE migration_jobs j SET batch_root_execution_id=(SELECT a.execution_id FROM migration_job_attempts a JOIN migration_history h ON h.execution_id=a.execution_id WHERE a.job_id=j.id ORDER BY a.attempt_no LIMIT 1) WHERE j.execution_mode='batch';
UPDATE migration_history h SET batch_root_execution_id=j.batch_root_execution_id FROM migration_jobs j JOIN migration_job_attempts a ON a.job_id=j.id WHERE a.execution_id=h.execution_id AND j.execution_mode='batch';
CREATE FUNCTION protect_batch_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.batch_root_execution_id IS NOT NULL AND NEW.batch_root_execution_id IS DISTINCT FROM OLD.batch_root_execution_id THEN
        RAISE EXCEPTION 'batch root is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER batch_root_immutable BEFORE UPDATE ON migration_jobs FOR EACH ROW EXECUTE FUNCTION protect_batch_root();
