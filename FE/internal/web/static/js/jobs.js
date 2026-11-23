import { authenticatedRequest, requireAuthenticatedUser, clearAuthState, isCSRFError } from "./auth.js";
import { confirmAction } from "./confirmation.js";
import { executionFeedback } from "./execution.js";
import { positiveInt64 } from "./pagination.js";
import { parseJobPage, parseJobDetail, assertJobAdvance, jobLabels } from "./job-contract.js";
import { byId, addCell, formatDate, navigateTo, setBanner } from "./ui.js";

const page = byId("jobs-page");
const databaseID = page.dataset.databaseId;
const jobID = page.dataset.jobId;
const api = `/api/v1/databases/${encodeURIComponent(databaseID)}/jobs`;
const workspace = byId("jobs-workspace");
const body = byId("jobs-list-body"), attemptsBody = byId("jobs-attempts-body");
const refreshButton = byId("jobs-refresh"), auto = byId("jobs-auto");
const previousButton = byId("jobs-prev"), nextButton = byId("jobs-next");
const resumeButton = byId("jobs-request-resume");
const compensationButton = byId("jobs-request-compensation");
const controller = new AbortController();
const options = { signal: controller.signal };
const listRows = new Map(), attemptRows = new Map();
let disposed = false, authenticated = false, canEdit = false, loading = false, acting = false, failed = false;
let timer = null, current = null, cursors = [null], currentPage = 0, hasMore = false, nextCursor = null;

const alive = () => !disposed && page.isConnected !== false;
const jobPath = (id) => `/databases/${encodeURIComponent(databaseID)}/jobs/${encodeURIComponent(id)}`;
const reviewPath = (id) => `/databases/${encodeURIComponent(databaseID)}/reviews/${encodeURIComponent(id)}`;
const text = (id, value) => { const e = byId(id); if (e.textContent !== String(value)) e.textContent = String(value); };

function controls() {
    const busy = loading || acting;
    refreshButton.disabled = busy;
    byId("jobs-page-retry").disabled = busy;
    previousButton.disabled = busy || currentPage === 0;
    nextButton.disabled = busy || !hasMore;
    auto.disabled = acting;
    resumeButton.disabled = busy || failed;
    compensationButton.disabled = busy || failed;
}

function stopTimer() { if (timer !== null) clearTimeout(timer); timer = null; }
function schedule() {
    stopTimer();
    if (alive() && auto.checked && !failed && !document.hidden && !acting) {
        timer = setTimeout(() => { timer = null; refresh(); }, 3000);
    }
}

function cleanup() {
    if (disposed) return;
    disposed = true;
    stopTimer();
    controller.abort();
    current = null; listRows.clear(); attemptRows.clear();
    const dialog = byId("global-confirm-dialog");
    if (dialog?.open) dialog.close("cancel");
    document.removeEventListener?.("visibilitychange", visibilityChanged);
    window.removeEventListener?.("pagehide", cleanup);
}
function visibilityChanged() {
    stopTimer();
    if (!document.hidden && !failed && alive()) refresh();
}
window.__spaCleanup?.();
window.__spaCleanup = cleanup;
document.addEventListener?.("visibilitychange", visibilityChanged);
window.addEventListener?.("pagehide", cleanup, { once: true });

function failure(error) {
    if (!alive()) return;
    failed = true;
    stopTimer();
    const revoked = [401, 403, 404].includes(error.status);
    if (revoked) {
        body.replaceChildren(); attemptsBody.replaceChildren();
        listRows.clear(); attemptRows.clear(); current = null;
        byId("jobs-detail").hidden = true;
        ["jobs-database-name", "jobs-access", "jobs-feedback", "jobs-detail-id", "jobs-detail-state", "jobs-detail-mode", "jobs-detail-draft", "jobs-detail-attempts", "jobs-detail-batches", "jobs-detail-rows", "jobs-detail-next", "jobs-detail-times", "jobs-detail-error"].forEach((id) => text(id, ""));
        ["jobs-detail-review", "jobs-detail-parent", "jobs-detail-child", "jobs-resume-review-link", "jobs-compensation-review-link"].forEach((id) => byId(id).removeAttribute("href"));
        workspace.hidden = true;
        setBanner(byId("jobs-page-error"), "Job atau database tidak tersedia untuk akun ini. Periksa kembali hak akses.", true);
        byId("jobs-page-retry").hidden = false;
    } else {
        setBanner(byId("jobs-read-error"), `${error.message || "Status job belum dapat dibaca."} Pembaruan dijeda. Gunakan Muat ulang untuk memeriksa kembali.`, true);
        if (workspace.hidden) {
            setBanner(byId("jobs-page-error"), "Data job belum dapat dimuat. Coba baca kembali.", true);
            byId("jobs-page-retry").hidden = false;
        }
    }
    text("jobs-read-status", "Pembaruan otomatis dijeda.");
    if (error.status === 401) { clearAuthState(); navigateTo("/login", { replace: true }); }
}

async function loadAccess() {
    const result = await authenticatedRequest("/api/v1/databases", options);
    if (!alive()) return;
    if (!Array.isArray(result?.databases)) throw new Error("Respons hak akses database tidak valid.");
    const database = result.databases.find((d) => d.id === databaseID && ["view", "edit"].includes(d.access));
    if (!database) { const error = new Error("Akses database tidak tersedia."); error.status = 403; throw error; }
    canEdit = database.access === "edit";
    byId("jobs-drafts-link").hidden = !canEdit;
    text("jobs-database-name", database.name);
    text("jobs-access", canEdit ? "View + Edit" : "View Only");
}

function link(cell, label, href) {
    let a = cell.children[0];
    if (!a) { a = document.createElement("a"); a.className = "workspace-button workspace-button--subtle"; cell.appendChild(a); }
    if (a.textContent !== label) a.textContent = label;
    a.href = href;
}

// Keep existing links mounted so background status updates preserve keyboard focus.
function syncRows(container, cache, entries, key, count, render) {
    const keep = new Set(entries.map(key));
    for (const [id, row] of cache) {
        if (!keep.has(id)) { container.removeChild(row); cache.delete(id); }
    }
    entries.forEach((item, index) => {
        const id = key(item);
        let row = cache.get(id);
        if (!row) { row = document.createElement("tr"); for (let i = 0; i < count; i++) addCell(row, ""); cache.set(id, row); }
        render(row.children, item);
        if (container.children[index] !== row) container.insertBefore(row, container.children[index] || null);
    });
}

function renderList(entries) {
    syncRows(body, listRows, entries, (j) => j.id, 8, (cells, j) => {
        const values = [`#${j.id} / #${j.review_id}`, `${j.action_type} / ${j.execution_mode}`, jobLabels[j.status],
            `${j.attempt_count}/${j.max_attempts}`, j.committed_batches, j.committed_rows, formatDate(j.created_at)];
        values.forEach((v, i) => { if (cells[i].textContent !== v) cells[i].textContent = v; });
        link(cells[7], `Detail #${j.id}`, jobPath(j.id));
    });
    byId("jobs-list-empty").hidden = entries.length !== 0;
    byId("jobs-list-table").hidden = entries.length === 0;
    byId("jobs-pagination").hidden = false;
    text("jobs-page-number", currentPage + 1);
}

function reference(id, emptyID, value) {
    const a = byId(id);
    a.hidden = !value; byId(emptyID).hidden = Boolean(value);
    if (value) { a.href = jobPath(value); a.textContent = `Job #${value}`; }
}

const activeReview = (job) => [job.resume_review, job.compensation_review].some((r) => ["pending_review", "approved", "queued"].includes(r?.status));
function canResume(detail) {
    const last = detail?.attempts.at(-1), j = detail?.job;
    return canEdit && j?.status === "partial" && j.execution_mode === "batch" && BigInt(j.committed_batches) > 0n &&
        ["partial", "failed"].includes(last?.status) && positiveInt64(last?.version) && !activeReview(j);
}

function canCompensate(detail) {
    const j = detail?.job, last = detail?.attempts.at(-1);
    return canEdit && j?.can_compensate === true && j.execution_mode === "batch" &&
        ["partial", "success", "partially_reverted"].includes(j.status) && positiveInt64(last?.version) && !activeReview(j);
}

function renderDetail(detail) {
    const { job: j, attempts } = detail;
    byId("jobs-detail").hidden = false;
    text("jobs-detail-id", `#${j.id}`);
    text("jobs-detail-state", jobLabels[j.status]);
    text("jobs-detail-mode", `${j.action_type} / ${j.execution_mode}`);
    text("jobs-detail-draft", `#${j.draft_id} / ${j.draft_revision}`);
    text("jobs-detail-attempts", `${j.attempt_count}/${j.max_attempts}`);
    text("jobs-batch-label", j.action_type === "compensate" ? "Batch dipulihkan" : "Batch committed");
    text("jobs-row-label", j.action_type === "compensate" ? "Row dipulihkan" : "Row committed");
    text("jobs-detail-batches", j.committed_batches); text("jobs-detail-rows", j.committed_rows);
    text("jobs-detail-next", ["queued", "retry_wait", "recovering"].includes(j.status) ? formatDate(j.available_at) : "-");
    text("jobs-detail-times", `${formatDate(j.started_at)} / ${formatDate(j.finished_at)}`);
    text("jobs-detail-error", j.error_code || "-");
    text("jobs-feedback", `${executionFeedback(j.status).message} ${j.action_type === "compensate" ? "Batch dipulihkan" : "Batch committed"}: ${j.committed_batches}; row: ${j.committed_rows}.`);
    byId("jobs-detail-review").href = reviewPath(j.authorization_review_id ?? j.review_id);
    reference("jobs-detail-parent", "jobs-detail-no-parent", j.parent_job_id);
    reference("jobs-detail-child", "jobs-detail-no-child", j.resumed_by_job_id);
    syncRows(attemptsBody, attemptRows, attempts, (a) => a.execution_id, 6, (cells, a) => {
        [String(a.attempt_no), a.execution_id, jobLabels[a.status], a.error_code || "-", `${formatDate(a.started_at)} / ${formatDate(a.finished_at)}`]
            .forEach((v, i) => { if (cells[i].textContent !== v) cells[i].textContent = v; });
        if (a.version) link(cells[5], `Versi ${a.version}`, `/databases/${encodeURIComponent(databaseID)}/history?version=${encodeURIComponent(a.version)}`);
        else cells[5].textContent = "Belum tersedia";
    });
    byId("jobs-attempts-empty").hidden = attempts.length !== 0;
    byId("jobs-attempts-table").hidden = attempts.length === 0;
    const partial = j.status === "partial" && j.execution_mode === "batch" && BigInt(j.committed_batches) > 0n;
    byId("jobs-resume-panel").hidden = !partial;
    resumeButton.hidden = !canResume(detail);
    byId("jobs-resume-permission").hidden = canEdit;
    byId("jobs-compensation-panel").hidden = !j.can_compensate;
    compensationButton.hidden = !canCompensate(detail);
    byId("jobs-compensation-permission").hidden = canEdit;
    const cr = j.compensation_review;
    byId("jobs-compensation-review").hidden = !cr;
    if (cr) {
        byId("jobs-compensation-review-link").href = reviewPath(cr.id);
        byId("jobs-compensation-review-link").textContent = `Review pembatalan #${cr.id} ? ${cr.status}`;
    }
    const r = j.resume_review;
    byId("jobs-resume-review").hidden = !r;
    if (r) {
        byId("jobs-resume-review-link").href = reviewPath(r.id);
        byId("jobs-resume-review-link").textContent = `Review resume #${r.id} — ${r.status}${r.status === "approved" ? ": buka untuk memasukkan resume ke antrean" : ""}`;
    }
}

async function refresh({ index = currentPage } = {}) {
    if (!alive() || loading || acting) return;
    loading = true; stopTimer(); controls();
    try {
        if (!authenticated) {
            const user = await requireAuthenticatedUser();
            if (!user) { cleanup(); return; }
            if (!alive()) return;
            authenticated = true;
        }
        await loadAccess();
        if (!alive()) return;
        const query = new URLSearchParams({ limit: "50" });
        if (cursors[index]) query.set("before", cursors[index]);
        const [list, result] = await Promise.all([
            authenticatedRequest(`${api}?${query}`, options),
            jobID ? authenticatedRequest(`${api}/${encodeURIComponent(jobID)}`, options) : null
        ]);
        if (!alive()) return;
        const data = parseJobPage(list, databaseID);
        const detail = result ? parseJobDetail(result, databaseID, jobID) : null;
        if (detail) assertJobAdvance(current?.job, detail.job);
        currentPage = index; hasMore = data.hasMore; nextCursor = data.nextCursor;
        cursors = cursors.slice(0, index + 1);
        renderList(data.entries);
        if (detail) { current = detail; renderDetail(detail); }
        failed = false; workspace.hidden = false;
        byId("jobs-read-error").hidden = true; byId("jobs-page-error").hidden = true; byId("jobs-page-retry").hidden = true;
        text("jobs-read-status", auto.checked ? "Pembaruan otomatis aktif." : "Pembaruan otomatis dijeda.");
        text("jobs-last-checked", `Terakhir diperiksa: ${formatDate(new Date().toISOString())}.`);
    } catch (error) { failure(error); }
    finally { loading = false; if (alive()) { byId("jobs-page-status").hidden = true; controls(); schedule(); } }
}

async function requestResume(kind = "resume") {
    const eligible = kind === "compensate" ? canCompensate : canResume;
    const trigger = kind === "compensate" ? compensationButton : resumeButton;
    if (!alive() || loading || acting || failed || !eligible(current)) return;
    let cancelled = false;
    acting = true; stopTimer(); controls();
    try {
        await loadAccess();
        if (!alive()) return;
        const fresh = parseJobDetail(await authenticatedRequest(`${api}/${encodeURIComponent(jobID)}`, options), databaseID, jobID);
        if (!alive()) return;
        assertJobAdvance(current.job, fresh.job);
        current = fresh; renderDetail(fresh);
        if (!eligible(fresh)) { setBanner(byId("jobs-action-message"), "Job, akses, atau review sudah berubah. Gunakan detail terbaru sebelum mengambil tindakan.", true); return; }
        const j = fresh.job, execution = fresh.attempts.at(-1).execution_id;
        const confirmed = await confirmAction({ title: kind === "compensate" ? "Ajukan pembatalan batch?" : "Ajukan review resume?", confirmLabel: "Ajukan review",
            message: kind === "compensate" ? `Ajukan pembatalan Job #${j.id}, draft #${j.draft_id} revisi ${j.draft_revision}, execution ${execution}?\n\nSQL Down asli akan memulihkan batch committed dalam urutan terbalik. Batch yang sudah dipulihkan akan dilewati saat dilanjutkan. Perubahan dari luar migration dapat menyebabkan pembatalan ditolak.` : `Ajukan resume Job #${j.id}, draft #${j.draft_id} revisi ${j.draft_revision}, execution ${execution}?\n\n${j.committed_batches} batch / ${j.committed_rows} row sudah committed. SQL historis dan scope awal dipertahankan. Worker menunggu persetujuan seluruh leader sebelum melanjutkan.` });
        if (!confirmed || !alive()) { cancelled = !confirmed; return; }
        const result = await authenticatedRequest(`/api/v1/databases/${encodeURIComponent(databaseID)}/reviews`, {
            ...options, method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ draft_id: j.draft_id, revision: j.draft_revision, action_type: kind, target_execution_id: execution })
        });
        if (!alive()) return;
        const r = result?.review;
        if (!r || !positiveInt64(r.id) || r.database_id !== databaseID || r.action_type !== kind || r.target_execution_id !== execution || r.draft_id !== j.draft_id || r.draft_revision !== j.draft_revision) {
            throw new Error("Hasil pengajuan review belum dapat dipastikan.");
        }
        navigateTo(reviewPath(r.id));
    } catch (error) {
        if (!alive()) return;
        const message = isCSRFError(error) ? "CSRF ditolak. Muat ulang atau login kembali sebelum mencoba lagi." : error.message;
        setBanner(byId("jobs-action-message"), `${message || "Pengajuan belum dapat dipastikan."} Periksa status dengan Muat ulang sebelum mengirim lagi.`, true);
        failure(error);
    } finally {
        acting = false;
        if (alive()) {
            controls(); schedule();
            if (cancelled && !trigger.hidden && !trigger.disabled) trigger.focus();
        }
    }
}

refreshButton.addEventListener("click", () => refresh());
byId("jobs-page-retry").addEventListener("click", () => refresh());
previousButton.addEventListener("click", () => currentPage > 0 && refresh({ index: currentPage - 1 }));
nextButton.addEventListener("click", () => {
    if (!loading && !acting && hasMore && positiveInt64(nextCursor)) { cursors[currentPage + 1] = nextCursor; return refresh({ index: currentPage + 1 }); }
});
auto.addEventListener("change", () => { stopTimer(); text("jobs-read-status", auto.checked && !failed ? "Pembaruan otomatis aktif." : "Pembaruan otomatis dijeda."); schedule(); });
resumeButton.addEventListener("click", () => requestResume("resume"));
compensationButton.addEventListener("click", () => requestResume("compensate"));

if (!positiveInt64(databaseID) || (jobID && !positiveInt64(jobID))) {
    failure(new Error("Identitas database atau job tidak valid."));
    byId("jobs-page-status").hidden = true;
} else refresh();
