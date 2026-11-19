-- Run with psql as a trusted target administrator, NEVER through the runner.
-- Supply an EXISTING restricted login: psql ... -v executor_role=app_executor -f this_file
-- The login must not own the database, this schema/table, or inherit their owners.
-- Use a dedicated executor per target; no ownership/grants in other databases.
-- Do not grant pg_read_all_stats or pg_signal_backend to this executor.
-- No login/password/role memberships are created or changed by this script.
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL search_path=pg_catalog,pg_temp;
CREATE SCHEMA IF NOT EXISTS _terserah;
CREATE TABLE IF NOT EXISTS _terserah.migration_receipts (
    execution_id text PRIMARY KEY,
    version bigint NOT NULL,
    sql text NOT NULL,
    executed_by bigint NOT NULL,
    committed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE _terserah.migration_receipts ADD COLUMN IF NOT EXISTS signature text;
CREATE TABLE IF NOT EXISTS _terserah.target_identity (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid()
);
INSERT INTO _terserah.target_identity(singleton) VALUES(true) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS _terserah.batch_runs (
    execution_id text PRIMARY KEY,
    database_id bigint NOT NULL,
    version bigint NOT NULL,
    executed_by bigint NOT NULL,
    sql text NOT NULL,
    table_oid oid NOT NULL,
    scope_count bigint NOT NULL CHECK (scope_count >= 0),
    scope_hash text NOT NULL,
    signature text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS _terserah.batch_scope (
    execution_id text NOT NULL REFERENCES _terserah.batch_runs(execution_id),
    row_key bigint NOT NULL,
    PRIMARY KEY(execution_id,row_key)
);
CREATE TABLE IF NOT EXISTS _terserah.batch_rows (
    execution_id text NOT NULL REFERENCES _terserah.batch_runs(execution_id),
    batch_no bigint NOT NULL CHECK (batch_no > 0),
    row_key bigint NOT NULL,
    before_data jsonb NOT NULL CHECK (jsonb_typeof(before_data) = 'object'),
    after_data jsonb NOT NULL CHECK (jsonb_typeof(after_data) = 'object'),
    PRIMARY KEY(execution_id,batch_no,row_key)
);
CREATE TABLE IF NOT EXISTS _terserah.batch_checkpoints (
    execution_id text NOT NULL REFERENCES _terserah.batch_runs(execution_id),
    batch_no bigint NOT NULL CHECK (batch_no > 0),
    last_key bigint NOT NULL,
    row_count bigint NOT NULL CHECK (row_count > 0),
    committed_rows bigint NOT NULL CHECK (committed_rows >= row_count),
    row_hash text NOT NULL,
    previous_signature text NOT NULL,
    signature text NOT NULL,
    committed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(execution_id,batch_no)
);
-- Transfer legacy M3 objects to the trusted administrator executing this script.
ALTER SCHEMA _terserah OWNER TO CURRENT_USER;
ALTER TABLE _terserah.migration_receipts OWNER TO CURRENT_USER;
ALTER TABLE _terserah.target_identity OWNER TO CURRENT_USER;
ALTER TABLE _terserah.batch_runs OWNER TO CURRENT_USER;
ALTER TABLE _terserah.batch_scope OWNER TO CURRENT_USER;
ALTER TABLE _terserah.batch_rows OWNER TO CURRENT_USER;
ALTER TABLE _terserah.batch_checkpoints OWNER TO CURRENT_USER;
REVOKE ALL ON SCHEMA _terserah FROM PUBLIC, :"executor_role";
REVOKE ALL ON _terserah.migration_receipts FROM PUBLIC, :"executor_role";
REVOKE ALL ON _terserah.target_identity FROM PUBLIC, :"executor_role";
GRANT USAGE ON SCHEMA _terserah TO :"executor_role";
GRANT SELECT, INSERT ON _terserah.migration_receipts TO :"executor_role";
GRANT SELECT ON _terserah.target_identity TO :"executor_role";
REVOKE ALL ON _terserah.batch_runs, _terserah.batch_scope, _terserah.batch_rows, _terserah.batch_checkpoints FROM PUBLIC, :"executor_role";
GRANT SELECT, INSERT ON _terserah.batch_runs, _terserah.batch_scope, _terserah.batch_rows, _terserah.batch_checkpoints TO :"executor_role";
COMMIT;
