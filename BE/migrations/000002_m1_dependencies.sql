ALTER TABLE sessions ADD COLUMN current_token_encrypted bytea;
ALTER TABLE sessions ADD COLUMN admin_at_login boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN rotation_required boolean NOT NULL DEFAULT false;
-- Existing sessions cannot recover a rotated identifier and must log in again.
UPDATE sessions SET revoked_at = now() WHERE revoked_at IS NULL;

CREATE UNIQUE INDEX database_connections_target_unique
    ON database_connections((CASE WHEN lower(rtrim(host, '.')) = 'localhost'
        THEN '127.0.0.1' ELSE lower(rtrim(host, '.')) END), port, database_name);
