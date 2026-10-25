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
-- Transfer legacy M3 objects to the trusted administrator executing this script.
ALTER SCHEMA _terserah OWNER TO CURRENT_USER;
ALTER TABLE _terserah.migration_receipts OWNER TO CURRENT_USER;
ALTER TABLE _terserah.target_identity OWNER TO CURRENT_USER;
REVOKE ALL ON SCHEMA _terserah FROM PUBLIC, :"executor_role";
REVOKE ALL ON _terserah.migration_receipts FROM PUBLIC, :"executor_role";
REVOKE ALL ON _terserah.target_identity FROM PUBLIC, :"executor_role";
GRANT USAGE ON SCHEMA _terserah TO :"executor_role";
GRANT SELECT, INSERT ON _terserah.migration_receipts TO :"executor_role";
GRANT SELECT ON _terserah.target_identity TO :"executor_role";
COMMIT;
