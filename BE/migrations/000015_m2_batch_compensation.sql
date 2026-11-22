ALTER TABLE migration_jobs ADD COLUMN compensation_root_execution_id text REFERENCES migration_history(execution_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE migration_jobs DROP CONSTRAINT migration_jobs_action_type_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_action_type_check CHECK(action_type IN ('up','resume','rollback','forward','compensate'));
ALTER TABLE migration_jobs DROP CONSTRAINT migration_jobs_status_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_status_check CHECK(status IN ('queued','running','success','failed','unknown','partial','recovering','retry_wait','resumed','partially_reverted'));
ALTER TABLE migration_job_attempts DROP CONSTRAINT migration_job_attempts_status_check;
ALTER TABLE migration_job_attempts ADD CONSTRAINT migration_job_attempts_status_check CHECK(status IN ('running','success','failed','unknown','partial','partially_reverted'));
ALTER TABLE migration_history DROP CONSTRAINT migration_history_status_check;
ALTER TABLE migration_history ADD CONSTRAINT migration_history_status_check CHECK(status IN ('running','success','failed','unknown','partial','partially_reverted'));
ALTER TABLE migration_jobs DROP CONSTRAINT resume_snapshot;
ALTER TABLE migration_jobs ADD CONSTRAINT resume_snapshot CHECK (
 (action_type='up' AND parent_job_id IS NULL AND target_execution_id IS NULL)
 OR (action_type IN ('resume','compensate') AND parent_job_id IS NOT NULL AND parent_job_id<>id AND target_execution_id IS NOT NULL AND execution_mode='batch' AND batch_root_execution_id IS NOT NULL)
 OR (action_type='rollback' AND parent_job_id IS NULL AND target_execution_id IS NOT NULL AND execution_mode='normal')
 OR (action_type='forward' AND parent_job_id IS NULL AND target_execution_id IS NOT NULL)
);
ALTER TABLE migration_jobs ADD CONSTRAINT compensation_root_mode CHECK (compensation_root_execution_id IS NULL OR action_type='compensate');
CREATE FUNCTION protect_compensation_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.compensation_root_execution_id IS NOT NULL AND NEW.compensation_root_execution_id IS DISTINCT FROM OLD.compensation_root_execution_id THEN
  RAISE EXCEPTION 'compensation root is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER compensation_root_immutable BEFORE UPDATE ON migration_jobs FOR EACH ROW EXECUTE FUNCTION protect_compensation_root();
DROP INDEX migration_jobs_active_database_idx;
CREATE UNIQUE INDEX migration_jobs_active_database_idx ON migration_jobs(database_id) WHERE status IN ('running','unknown','partial','recovering','retry_wait','partially_reverted');
CREATE OR REPLACE FUNCTION pair_successful_queued_rollback() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original_execution text;
BEGIN
 IF NEW.status='success' AND NEW.action_type='rollback' THEN
  original_execution:=NEW.target_execution_id;
 ELSIF NEW.status='success' AND NEW.action_type='compensate' THEN
  SELECT root.target_execution_id INTO original_execution FROM migration_jobs j
   JOIN migration_history root ON root.execution_id=j.compensation_root_execution_id WHERE j.id=NEW.job_id;
  IF NOT EXISTS(SELECT 1 FROM migration_history h WHERE h.execution_id=original_execution AND h.status='success') THEN
   RETURN NEW;
  END IF;
 END IF;
 IF original_execution IS NOT NULL THEN
  INSERT INTO migration_rollback_pairs(database_id,applied_execution_id,rollback_execution_id)
   VALUES(NEW.database_id,original_execution,NEW.execution_id) ON CONFLICT(rollback_execution_id) DO NOTHING;
 END IF;
 RETURN NEW;
END;
$$;
