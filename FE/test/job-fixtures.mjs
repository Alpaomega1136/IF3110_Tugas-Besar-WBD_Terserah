export const dbID = "9007199254740993";
export const timestamp = "2026-11-22T01:00:00Z";
export const jobFixture = (changes = {}) => ({
    id: "9007199254741001", database_id: dbID, review_id: "8", draft_id: "6", draft_revision: "1",
    requested_by: "3", action_type: "up", execution_mode: "batch", status: "running",
    attempt_count: 1, max_attempts: 3, created_at: timestamp, available_at: timestamp,
    started_at: timestamp, finished_at: null, error_code: null, committed_batches: "1", committed_rows: "2000",
    ...changes
});
export const attemptFixture = (changes = {}) => ({
    execution_id: "a".repeat(32), version: "9", attempt_no: 1, status: "running",
    started_at: timestamp, finished_at: null, error_code: null, ...changes
});
