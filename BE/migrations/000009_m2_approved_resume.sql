ALTER TABLE migration_jobs ADD COLUMN parent_job_id bigint UNIQUE REFERENCES migration_jobs(id);
ALTER TABLE migration_jobs ADD COLUMN target_execution_id text REFERENCES migration_history(execution_id);
ALTER TABLE migration_jobs ADD COLUMN resumed_by_job_id bigint UNIQUE REFERENCES migration_jobs(id);
ALTER TABLE migration_jobs DROP CONSTRAINT migration_jobs_action_type_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_action_type_check CHECK (action_type IN ('up','resume'));
ALTER TABLE migration_jobs DROP CONSTRAINT migration_jobs_status_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_status_check CHECK (status IN ('queued','running','success','failed','unknown','partial','recovering','retry_wait','resumed'));
ALTER TABLE migration_jobs ADD CONSTRAINT resume_snapshot CHECK (
    (action_type='up' AND parent_job_id IS NULL AND target_execution_id IS NULL)
    OR (action_type='resume' AND parent_job_id IS NOT NULL AND parent_job_id<>id AND target_execution_id IS NOT NULL AND execution_mode='batch' AND batch_root_execution_id IS NOT NULL)
);
ALTER TABLE migration_jobs ADD CONSTRAINT resume_handoff CHECK ((status='resumed')=(resumed_by_job_id IS NOT NULL));

CREATE FUNCTION protect_resume_coordinates() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.parent_job_id,NEW.target_execution_id) IS DISTINCT FROM ROW(OLD.parent_job_id,OLD.target_execution_id)
       OR (OLD.resumed_by_job_id IS NOT NULL AND NEW.resumed_by_job_id IS DISTINCT FROM OLD.resumed_by_job_id) THEN
        RAISE EXCEPTION 'resume coordinates are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER resume_coordinates_immutable BEFORE UPDATE ON migration_jobs FOR EACH ROW EXECUTE FUNCTION protect_resume_coordinates();

ALTER TABLE migration_history ADD COLUMN action_type text NOT NULL DEFAULT 'up' CHECK (action_type IN ('up','resume'));
ALTER TABLE migration_history ADD COLUMN target_execution_id text REFERENCES migration_history(execution_id);
ALTER TABLE migration_history ADD COLUMN job_id bigint REFERENCES migration_jobs(id);
UPDATE migration_history h SET job_id=a.job_id FROM migration_job_attempts a WHERE a.execution_id=h.execution_id;

CREATE FUNCTION protect_review_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.database_id,NEW.draft_id,NEW.draft_revision,NEW.action_type,NEW.target_execution_id,NEW.sql_snapshot,NEW.requested_by,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.database_id,OLD.draft_id,OLD.draft_revision,OLD.action_type,OLD.target_execution_id,OLD.sql_snapshot,OLD.requested_by,OLD.created_at) THEN
        RAISE EXCEPTION 'review snapshot is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER review_snapshot_immutable BEFORE UPDATE ON migration_reviews FOR EACH ROW EXECUTE FUNCTION protect_review_snapshot();
