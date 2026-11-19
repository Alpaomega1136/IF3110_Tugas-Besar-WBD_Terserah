-- Milestone 2: Group leader assignment and multi-group migration approval workflow.
ALTER TABLE groups
    ADD COLUMN leader_user_id bigint REFERENCES users(id);

CREATE INDEX groups_leader_user_idx ON groups(leader_user_id)
    WHERE leader_user_id IS NOT NULL;

CREATE TABLE migration_reviews (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id bigint NOT NULL REFERENCES database_connections(id) ON DELETE CASCADE,
    draft_id bigint NOT NULL REFERENCES migration_drafts(id) ON DELETE CASCADE,
    draft_revision bigint NOT NULL CHECK (draft_revision > 0),
    action_type text NOT NULL DEFAULT 'up'
        CHECK (action_type IN ('up', 'rollback', 'forward', 'resume', 'compensate')),
    target_execution_id text,
    sql_snapshot text NOT NULL,
    status text NOT NULL DEFAULT 'pending_review'
        CHECK (status IN ('pending_review', 'approved', 'rejected', 'queued', 'cancelled')),
    requested_by bigint NOT NULL REFERENCES users(id),
    rejection_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz
);

CREATE INDEX migration_reviews_database_idx
    ON migration_reviews(database_id, id DESC);

CREATE INDEX migration_reviews_draft_rev_idx
    ON migration_reviews(database_id, draft_id, draft_revision);

CREATE TABLE migration_review_approvals (
    review_id bigint NOT NULL REFERENCES migration_reviews(id) ON DELETE CASCADE,
    group_id bigint NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    access_level text NOT NULL CHECK (access_level IN ('view', 'edit')),
    leader_user_id bigint NOT NULL REFERENCES users(id),
    decision text NOT NULL DEFAULT 'pending'
        CHECK (decision IN ('pending', 'approved', 'rejected')),
    note text,
    decided_by bigint REFERENCES users(id),
    decided_at timestamptz,
    PRIMARY KEY (review_id, group_id)
);

CREATE INDEX migration_review_approvals_leader_idx
    ON migration_review_approvals(leader_user_id)
    WHERE decision = 'pending';
