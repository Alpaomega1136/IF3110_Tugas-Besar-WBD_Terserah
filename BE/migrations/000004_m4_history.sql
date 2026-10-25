ALTER TABLE migration_history ADD COLUMN receipt_version smallint NOT NULL DEFAULT 0
    CHECK (receipt_version IN (0,1));
CREATE INDEX migration_history_pending_idx ON migration_history(database_id)
    WHERE status IN ('running','unknown');
ALTER TABLE database_connections ADD COLUMN target_identity uuid UNIQUE;
