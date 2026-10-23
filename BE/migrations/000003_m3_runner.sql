ALTER TABLE migration_drafts ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0);
ALTER TABLE migration_history ADD COLUMN execution_id text UNIQUE;
ALTER TABLE migration_history ADD COLUMN draft_revision bigint CHECK (draft_revision > 0);
CREATE UNIQUE INDEX migration_history_draft_revision_idx
    ON migration_history(database_id, draft_id, draft_revision);
