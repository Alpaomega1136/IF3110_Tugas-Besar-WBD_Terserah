-- Metadata attempt yang dapat dipakai bersama oleh runner lama dan worker async.
-- Kolom action_type/target_execution_id mungkin sudah tersedia dari branch worker.
ALTER TABLE migration_history ADD COLUMN IF NOT EXISTS action_type text NOT NULL DEFAULT 'up';
ALTER TABLE migration_history ADD COLUMN IF NOT EXISTS target_execution_id text;
ALTER TABLE migration_history ADD COLUMN IF NOT EXISTS retry_of_execution_id text;

-- Perluasan constraint dilakukan setelah migration worker agar tidak duplikat.
ALTER TABLE migration_history DROP CONSTRAINT IF EXISTS migration_history_action_type_check;
ALTER TABLE migration_history ADD CONSTRAINT migration_history_action_type_check
    CHECK (action_type IN ('up','rollback','forward','resume','compensate'));
ALTER TABLE migration_history ADD CONSTRAINT migration_history_target_execution_fk
    FOREIGN KEY (database_id,target_execution_id)
    REFERENCES migration_history(database_id,execution_id);
ALTER TABLE migration_history ADD CONSTRAINT migration_history_retry_execution_fk
    FOREIGN KEY (database_id,retry_of_execution_id)
    REFERENCES migration_history(database_id,execution_id);
ALTER TABLE migration_history ADD CONSTRAINT migration_history_retry_distinct
    CHECK (retry_of_execution_id IS NULL OR retry_of_execution_id <> execution_id);
CREATE UNIQUE INDEX migration_history_retry_parent_unique_idx
    ON migration_history(retry_of_execution_id) WHERE retry_of_execution_id IS NOT NULL;
CREATE INDEX migration_history_target_execution_idx
    ON migration_history(database_id,target_execution_id) WHERE target_execution_id IS NOT NULL;

-- Identitas attempt tidak boleh diubah. Status dan waktu selesai tetap bisa
-- diperbarui selama finalisasi/recovery pada attempt yang sama.
CREATE FUNCTION protect_migration_attempt_identity()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.database_id,NEW.draft_id,NEW.draft_revision,NEW.version,
           NEW.sql,NEW.executed_by,NEW.execution_id,NEW.started_at,
           NEW.action_type,NEW.target_execution_id,NEW.retry_of_execution_id)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.database_id,OLD.draft_id,OLD.draft_revision,OLD.version,
           OLD.sql,OLD.executed_by,OLD.execution_id,OLD.started_at,
           OLD.action_type,OLD.target_execution_id,OLD.retry_of_execution_id) THEN
        RAISE EXCEPTION 'identitas dan SQL attempt tidak dapat diubah'
            USING ERRCODE='23514', CONSTRAINT='migration_history_attempt_immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER migration_history_attempt_identity_immutable
    BEFORE UPDATE ON migration_history FOR EACH ROW
    EXECUTE FUNCTION protect_migration_attempt_identity();

CREATE FUNCTION reject_migration_attempt_delete()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'attempt migration tidak boleh dihapus'
        USING ERRCODE='23514', CONSTRAINT='migration_history_attempt_immutable';
END;
$$;
CREATE TRIGGER migration_history_attempt_no_delete
    BEFORE DELETE ON migration_history FOR EACH ROW
    EXECUTE FUNCTION reject_migration_attempt_delete();
