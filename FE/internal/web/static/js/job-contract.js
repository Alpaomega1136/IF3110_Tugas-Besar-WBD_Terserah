import { parseJobResponse } from "./execution.js";
import { positiveInt64, parseCursorPage } from "./pagination.js";

export const jobLabels = {
    queued: "Dalam antrean", running: "Berjalan", success: "Berhasil", failed: "Gagal",
    unknown: "Hasil belum pasti", partial: "Sebagian committed", recovering: "Memeriksa bukti",
    retry_wait: "Menunggu retry / resume", resumed: "Dialihkan ke job resume"
};

const date = (v) => typeof v === "string" && Number.isFinite(Date.parse(v));
const executionStates = ["running", "success", "failed", "unknown", "partial"];

export function parseJob(job, databaseID) {
    parseJobResponse({ job }, databaseID);
    if (!["review_id", "draft_id", "draft_revision", "requested_by"].every((key) => positiveInt64(job[key])) ||
        !["up", "resume", "rollback", "forward"].includes(job.action_type) || !["normal", "batch"].includes(job.execution_mode) ||
        !Number.isInteger(job.max_attempts) || job.max_attempts < 1 || job.max_attempts > 10 ||
        !Number.isInteger(job.attempt_count) || job.attempt_count < 0 || job.attempt_count > job.max_attempts ||
        typeof job.committed_batches !== "string" || typeof job.committed_rows !== "string" ||
        BigInt(job.committed_rows) < BigInt(job.committed_batches) ||
        !date(job.created_at) || !date(job.available_at) ||
        ["started_at", "finished_at"].some((key) => job[key] != null && !date(job[key])) ||
        (job.authorization_review_id != null && !positiveInt64(job.authorization_review_id)) ||
        (job.action_type === "resume" && (!positiveInt64(job.parent_job_id) || !job.target_execution_id)) ||
        (["rollback", "forward"].includes(job.action_type) && (job.execution_mode !== "normal" || !job.target_execution_id || job.parent_job_id != null))) {
        throw new Error("Detail job dari backend tidak valid. Muat ulang sebelum mengambil tindakan.");
    }
    return job;
}

export function parseJobPage(payload, databaseID) {
    const page = parseCursorPage(payload, "jobs", "next_before");
    if (page.entries.length > 50) throw new Error("Halaman job terlalu besar.");
    let previous = null;
    for (const job of page.entries) {
        parseJob(job, databaseID);
        if (previous !== null && BigInt(job.id) >= BigInt(previous)) throw new Error("Urutan job tidak valid.");
        previous = job.id;
    }
    if (page.hasMore && page.nextCursor !== previous) throw new Error("Cursor job tidak sesuai halaman.");
    return page;
}

export function parseJobDetail(payload, databaseID, jobID) {
    const job = parseJob(payload?.job, databaseID);
    if (job.id !== jobID || !Array.isArray(payload.attempts) || payload.attempts.length !== job.attempt_count) {
        throw new Error("Identitas atau daftar attempt job tidak valid.");
    }
    const executions = new Set();
    for (const [index, a] of payload.attempts.entries()) {
        if (!a || a.attempt_no !== index + 1 || typeof a.execution_id !== "string" || !a.execution_id || executions.has(a.execution_id) ||
            !executionStates.includes(a.status) || (a.version != null && !positiveInt64(a.version)) ||
            !date(a.started_at) || (a.finished_at != null && !date(a.finished_at)) ||
            (a.error_code != null && typeof a.error_code !== "string")) {
            throw new Error("Attempt job dari backend tidak valid.");
        }
        executions.add(a.execution_id);
    }
    const r = job.resume_review;
    if (r != null && (!positiveInt64(r.id) ||
        !["pending_review", "approved", "rejected", "queued", "cancelled"].includes(r.status) ||
        r.target_execution_id !== payload.attempts.at(-1)?.execution_id || job.status !== "partial")) {
        throw new Error("Review resume tidak sesuai execution job.");
    }
    return { job, attempts: payload.attempts };
}

export function assertJobAdvance(previous, current) {
    if (!previous) return;
    const immutable = ["id", "database_id", "review_id", "draft_id", "draft_revision", "requested_by", "action_type", "execution_mode", "max_attempts", "parent_job_id", "target_execution_id"];
    if (immutable.some((key) => previous[key] !== current[key]) ||
        current.attempt_count < previous.attempt_count ||
        BigInt(current.committed_batches) < BigInt(previous.committed_batches) ||
        BigInt(current.committed_rows) < BigInt(previous.committed_rows) ||
        (previous.resumed_by_job_id != null && current.resumed_by_job_id !== previous.resumed_by_job_id)) {
        throw new Error("Identitas atau progres job berubah tidak konsisten. Muat ulang untuk memeriksa hasil.");
    }
}
