CREATE TABLE migration_jobs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id bigint NOT NULL REFERENCES database_connections(id),
    review_id bigint NOT NULL UNIQUE REFERENCES migration_reviews(id),
    draft_id bigint NOT NULL REFERENCES migration_drafts(id),
    draft_revision bigint NOT NULL CHECK (draft_revision > 0),
    action_type text NOT NULL CHECK (action_type = 'up'),
    execution_mode text NOT NULL DEFAULT 'normal' CHECK (execution_mode = 'normal'),
    sql_snapshot text NOT NULL CHECK (octet_length(sql_snapshot) BETWEEN 1 AND 49152),
    snapshot_hash text NOT NULL CHECK (snapshot_hash ~ '^[0-9a-f]{64}$'),
    requested_by bigint NOT NULL REFERENCES users(id),
    trace_parent text NOT NULL DEFAULT '' CHECK (trace_parent = '' OR trace_parent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','success','failed','unknown')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    available_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    worker_id text,
    lease_token_hash bytea,
    lease_expires_at timestamptz,
    error_code text CHECK (length(error_code) BETWEEN 1 AND 64),
    committed_batches bigint NOT NULL DEFAULT 0 CHECK (committed_batches >= 0),
    committed_rows bigint NOT NULL DEFAULT 0 CHECK (committed_rows >= 0),
    CHECK (attempt_count <= max_attempts),
    CHECK ((status = 'running' AND worker_id IS NOT NULL AND lease_token_hash IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'running' AND worker_id IS NULL AND lease_token_hash IS NULL AND lease_expires_at IS NULL)),
    CHECK ((status IN ('success','failed')) = (finished_at IS NOT NULL))
);
CREATE INDEX migration_jobs_queue_idx ON migration_jobs(available_at,id) WHERE status = 'queued';
CREATE INDEX migration_jobs_database_idx ON migration_jobs(database_id,id DESC);
-- An expired lease is not proof that the old target transaction stopped.
CREATE UNIQUE INDEX migration_jobs_active_database_idx ON migration_jobs(database_id) WHERE status IN ('running','unknown');

CREATE TABLE migration_job_attempts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id bigint NOT NULL REFERENCES migration_jobs(id),
    execution_id text NOT NULL UNIQUE CHECK (execution_id ~ '^[0-9a-f]{32}$'),
    attempt_no integer NOT NULL CHECK (attempt_no > 0),
    worker_id text NOT NULL,
    sql_snapshot text NOT NULL,
    status text NOT NULL DEFAULT 'running' CHECK (status IN ('running','success','failed','unknown')),
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    error_code text CHECK (length(error_code) BETWEEN 1 AND 64),
    UNIQUE(job_id,attempt_no),
    CHECK ((status <> 'running') = (finished_at IS NOT NULL))
);

CREATE FUNCTION protect_migration_job_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.database_id,NEW.review_id,NEW.draft_id,NEW.draft_revision,NEW.action_type,
           NEW.execution_mode,NEW.sql_snapshot,NEW.snapshot_hash,NEW.requested_by,NEW.trace_parent,NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id,OLD.database_id,OLD.review_id,OLD.draft_id,OLD.draft_revision,OLD.action_type,
           OLD.execution_mode,OLD.sql_snapshot,OLD.snapshot_hash,OLD.requested_by,OLD.trace_parent,OLD.created_at) THEN
        RAISE EXCEPTION 'migration job snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER migration_job_snapshot_immutable BEFORE UPDATE ON migration_jobs
    FOR EACH ROW EXECUTE FUNCTION protect_migration_job_snapshot();

CREATE FUNCTION protect_migration_attempt_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.job_id,NEW.execution_id,NEW.attempt_no,NEW.worker_id,NEW.sql_snapshot,NEW.started_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.job_id,OLD.execution_id,OLD.attempt_no,OLD.worker_id,OLD.sql_snapshot,OLD.started_at) THEN
        RAISE EXCEPTION 'migration attempt snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER migration_attempt_snapshot_immutable BEFORE UPDATE ON migration_job_attempts
    FOR EACH ROW EXECUTE FUNCTION protect_migration_attempt_snapshot();
