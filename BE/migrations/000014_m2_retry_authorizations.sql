CREATE TABLE migration_retry_authorizations (
    job_id bigint NOT NULL REFERENCES migration_jobs(id),
    first_attempt integer NOT NULL CHECK (first_attempt > 1),
    review_id bigint NOT NULL UNIQUE REFERENCES migration_reviews(id),
    requested_by bigint NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(job_id,first_attempt)
);
CREATE TRIGGER retry_authorizations_immutable
    BEFORE UPDATE OR DELETE ON migration_retry_authorizations
    FOR EACH ROW EXECUTE FUNCTION reject_migration_revision_snapshot_mutation();
