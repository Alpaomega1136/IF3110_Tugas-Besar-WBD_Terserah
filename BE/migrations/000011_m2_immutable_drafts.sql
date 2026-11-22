-- Draft yang pernah memiliki attempt atau snapshot tidak boleh diubah lagi.
-- Pemeriksaan pada database melindungi definisi dari perubahan di luar API.
CREATE FUNCTION reject_executed_migration_draft_update()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM migration_revision_snapshots WHERE draft_id = OLD.id)
       OR EXISTS (SELECT 1 FROM migration_history WHERE draft_id = OLD.id) THEN
        RAISE EXCEPTION 'definisi migration yang pernah dieksekusi tidak dapat diubah'
            USING ERRCODE = '23514', CONSTRAINT = 'migration_drafts_executed_immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER migration_drafts_executed_immutable
    BEFORE UPDATE ON migration_drafts
    FOR EACH ROW EXECUTE FUNCTION reject_executed_migration_draft_update();
