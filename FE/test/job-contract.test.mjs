import test from "node:test";
import assert from "node:assert/strict";
import { parseJob, parseJobPage, parseJobDetail, assertJobAdvance } from "../internal/web/static/js/job-contract.js";
import { dbID, jobFixture, attemptFixture } from "./job-fixtures.mjs";

test("job detail preserves string IDs and committed counts beyond Number precision", () => {
    const job = jobFixture({ committed_rows: "9007199254740999" });
    const detail = parseJobDetail({ job, attempts: [attemptFixture()] }, dbID, job.id);
    assert.equal(detail.job.committed_rows, "9007199254740999");
    for (const changed of [{ database_id: "1" }, { committed_rows: "0" }, { max_attempts: 11 }, { attempt_count: 4 },
        { action_type: "compensate" }, { created_at: "invalid" }, { review_id: 8 }, { parent_job_id: "0" }]) {
        assert.throws(() => parseJob({ ...job, ...changed }, dbID));
    }
});

test("job pagination checks order, cursor and scope", () => {
    const first = jobFixture(), second = jobFixture({ id: "9007199254741000" });
    const response = { jobs: [first, second], has_more: true, next_before: second.id };
    assert.equal(parseJobPage(response, dbID).nextCursor, second.id);
    for (const changed of [{ jobs: [second, first] }, { next_before: first.id }, { jobs: [first, first] }, { jobs: [] }]) {
        assert.throws(() => parseJobPage({ ...response, ...changed }, dbID));
    }
});

test("job attempts and resume review refer to the exact current execution", () => {
    const job = jobFixture({ status: "partial", resume_review: { id: "21", status: "approved", target_execution_id: "a".repeat(32) } });
    const attempt = attemptFixture({ status: "partial" });
    assert.equal(parseJobDetail({ job, attempts: [attempt] }, dbID, job.id).job.resume_review.id, "21");
    assert.throws(() => parseJobDetail({ job, attempts: [] }, dbID, job.id));
    assert.throws(() => parseJobDetail({ job, attempts: [attempt] }, dbID, "1"));
    assert.throws(() => parseJobDetail({ job, attempts: [{ ...attempt, attempt_no: 2 }] }, dbID, job.id));
    assert.throws(() => parseJobDetail({ job: { ...job, resume_review: { ...job.resume_review, target_execution_id: "other" } }, attempts: [attempt] }, dbID, job.id));
});

test("polling rejects regressing counts and replaced job coordinates", () => {
    const job = jobFixture();
    assertJobAdvance(job, { ...job, status: "success", committed_rows: "4000", committed_batches: "2" });
    for (const changed of [{ committed_rows: "1999" }, { committed_batches: "0" }, { attempt_count: 0 }, { review_id: "99" }]) {
        assert.throws(() => assertJobAdvance(job, { ...job, ...changed }));
    }
    const handedOff = { ...job, status: "resumed", resumed_by_job_id: "88" };
    assertJobAdvance(job, handedOff);
    assert.throws(() => assertJobAdvance(handedOff, { ...handedOff, resumed_by_job_id: "89" }));
});
