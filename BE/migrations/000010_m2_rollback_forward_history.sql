ALTER TABLE migration_drafts
    ADD COLUMN sql_down text,
    ADD COLUMN irreversible boolean NOT NULL DEFAULT false,
    ADD COLUMN irreversible_reason text;

ALTER TABLE migration_drafts
    ADD CONSTRAINT migration_drafts_sql_down_nonblank
        CHECK (sql_down IS NULL OR length(btrim(sql_down)) > 0),
    ADD CONSTRAINT migration_drafts_irreversible_fields
        CHECK (
            (NOT irreversible AND irreversible_reason IS NULL)
            OR (
                irreversible AND sql_down IS NULL
                AND NULLIF(btrim(irreversible_reason), '') IS NOT NULL
            )
        );

CREATE TABLE migration_revision_snapshots (
    draft_id bigint NOT NULL REFERENCES migration_drafts(id) ON DELETE RESTRICT,
    revision bigint NOT NULL CHECK (revision > 0),
    sql_up text NOT NULL,
    sql_down text,
    irreversible boolean NOT NULL,
    irreversible_reason text,
    first_execution_id text REFERENCES migration_history(execution_id),
    locked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (draft_id, revision),
    CONSTRAINT migration_revision_snapshots_definition_valid CHECK (
        (irreversible AND sql_down IS NULL
            AND NULLIF(btrim(irreversible_reason), '') IS NOT NULL)
        OR (NOT irreversible AND NULLIF(btrim(sql_down), '') IS NOT NULL
            AND irreversible_reason IS NULL)
    )
);

INSERT INTO migration_revision_snapshots (
    draft_id, revision, sql_up, sql_down, irreversible, irreversible_reason,
    first_execution_id, locked_at
)
SELECT DISTINCT ON (h.draft_id, h.draft_revision)
    h.draft_id,
    h.draft_revision,
    h.sql,
    NULL,
    true,
    'Eksekusi lama: SQL Down tidak dicatat pada Milestone 1',
    h.execution_id,
    h.started_at
FROM migration_history h
WHERE h.draft_id IS NOT NULL AND h.draft_revision IS NOT NULL
ORDER BY h.draft_id, h.draft_revision, h.version, h.id;

-- Snapshot bersifat append-only: revisi yang pernah dieksekusi tidak dapat diubah.
CREATE FUNCTION reject_migration_revision_snapshot_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'snapshot revisi migration tidak dapat diubah'
        USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER migration_revision_snapshots_immutable
    BEFORE UPDATE OR DELETE ON migration_revision_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_migration_revision_snapshot_mutation();

-- Satu revisi dapat memiliki banyak attempt: Up, rollback, forward, dan retry.
-- Execution ID dan nomor event per database tetap memiliki constraint unik sendiri.
DROP INDEX IF EXISTS migration_history_draft_revision_idx;
CREATE INDEX migration_history_draft_revision_idx
    ON migration_history(database_id, draft_id, draft_revision);
CREATE UNIQUE INDEX migration_history_database_execution_idx
    ON migration_history(database_id, execution_id);

-- Pasangan rollback menghubungkan execution yang dibatalkan dan execution
-- rollback yang sukses. Riwayat tidak pernah dihapus oleh fitur Hide rollback.
CREATE TABLE migration_rollback_pairs (
    database_id bigint NOT NULL REFERENCES database_connections(id),
    applied_execution_id text NOT NULL,
    rollback_execution_id text NOT NULL PRIMARY KEY,
    paired_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT migration_rollback_pairs_distinct CHECK (
        applied_execution_id <> rollback_execution_id
    ),
    CONSTRAINT migration_rollback_pairs_one_per_applied UNIQUE (applied_execution_id),
    CONSTRAINT migration_rollback_pairs_applied_fk
        FOREIGN KEY (database_id, applied_execution_id)
        REFERENCES migration_history(database_id, execution_id),
    CONSTRAINT migration_rollback_pairs_rollback_fk
        FOREIGN KEY (database_id, rollback_execution_id)
        REFERENCES migration_history(database_id, execution_id)
);
CREATE INDEX migration_rollback_pairs_database_idx
    ON migration_rollback_pairs(database_id);

-- Hanya dua eksekusi sukses dari revisi migration yang sama boleh dipasangkan.
CREATE FUNCTION validate_migration_rollback_pair()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    applied_row migration_history%ROWTYPE;
    rollback_row migration_history%ROWTYPE;
BEGIN
    SELECT * INTO applied_row FROM migration_history
        WHERE database_id = NEW.database_id AND execution_id = NEW.applied_execution_id;
    SELECT * INTO rollback_row FROM migration_history
        WHERE database_id = NEW.database_id AND execution_id = NEW.rollback_execution_id;
    IF applied_row.id IS NULL OR rollback_row.id IS NULL THEN
        RAISE EXCEPTION 'pasangan rollback harus berasal dari database yang sama'
            USING ERRCODE = '23503';
    END IF;
    IF applied_row.status <> 'success' OR rollback_row.status <> 'success'
        OR applied_row.draft_id IS NULL OR applied_row.draft_revision IS NULL
        OR applied_row.draft_id IS DISTINCT FROM rollback_row.draft_id
        OR applied_row.draft_revision IS DISTINCT FROM rollback_row.draft_revision THEN
        RAISE EXCEPTION 'pasangan rollback harus mengacu pada eksekusi sukses dari revisi yang sama'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER migration_rollback_pairs_validate
    BEFORE INSERT ON migration_rollback_pairs
    FOR EACH ROW EXECUTE FUNCTION validate_migration_rollback_pair();

-- Pasangan yang sudah dicatat tidak boleh diubah atau dihapus.
CREATE FUNCTION reject_migration_rollback_pair_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'pasangan rollback yang berhasil tidak dapat diubah'
        USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER migration_rollback_pairs_immutable
    BEFORE UPDATE OR DELETE ON migration_rollback_pairs
    FOR EACH ROW EXECUTE FUNCTION reject_migration_rollback_pair_mutation();
