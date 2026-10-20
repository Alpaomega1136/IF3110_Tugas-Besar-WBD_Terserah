CREATE TABLE users (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username text NOT NULL UNIQUE CHECK (length(username) BETWEEN 1 AND 100),
    password_hash text NOT NULL,
    is_platform_admin boolean NOT NULL DEFAULT false,
    is_active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE groups (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE
);
CREATE TABLE user_groups (
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id bigint NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, group_id)
);
CREATE INDEX user_groups_group_idx ON user_groups(group_id);
CREATE TABLE database_connections (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    host text NOT NULL,
    port integer NOT NULL DEFAULT 5432 CHECK (port BETWEEN 1 AND 65535),
    database_name text NOT NULL,
    username text NOT NULL,
    password_encrypted bytea NOT NULL,
    sslmode text NOT NULL DEFAULT 'verify-full'
        CHECK (sslmode IN ('disable', 'require', 'verify-ca', 'verify-full')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE group_database_permissions (
    group_id bigint NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    database_id bigint NOT NULL REFERENCES database_connections(id) ON DELETE CASCADE,
    access_level text NOT NULL CHECK (access_level IN ('view', 'edit')),
    PRIMARY KEY (group_id, database_id)
);
CREATE INDEX group_permissions_database_idx ON group_database_permissions(database_id);
CREATE TABLE sessions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token_hash bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    absolute_expires_at timestamptz NOT NULL,
    rotated_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CHECK (absolute_expires_at > created_at)
);
CREATE INDEX sessions_user_idx ON sessions(user_id);
-- Token aliases share one session so revocation also invalidates grace tokens.
CREATE TABLE session_tokens (
    token_hash bytea PRIMARY KEY,
    session_id bigint NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    valid_until timestamptz NOT NULL
);
CREATE INDEX session_tokens_session_idx ON session_tokens(session_id);
CREATE TABLE migration_drafts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id bigint NOT NULL REFERENCES database_connections(id),
    title text NOT NULL,
    sql text NOT NULL,
    created_by bigint NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX migration_drafts_database_idx ON migration_drafts(database_id);
-- Platform execution ledger. Atomic success history must also live on target.
CREATE TABLE migration_history (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id bigint NOT NULL REFERENCES database_connections(id),
    draft_id bigint REFERENCES migration_drafts(id),
    version bigint NOT NULL CHECK (version > 0),
    sql text NOT NULL,
    executed_by bigint NOT NULL REFERENCES users(id),
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status text NOT NULL CHECK (status IN ('running', 'success', 'failed', 'unknown')),
    error_code text,
    UNIQUE (database_id, version)
);

