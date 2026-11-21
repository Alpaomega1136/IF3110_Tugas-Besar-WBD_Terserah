import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { executionFeedback, parseExecutionResult, parseJobResponse, executeErrorMessage } from "./execution.js";
import { positiveInt64, parseCursorPage } from "./pagination.js";
import { initializeSchemaDiff } from "./schema-diff.js";
import { byId, addCell, formatDate, setBanner, setFieldError, utf8Bytes } from "./ui.js";

const page = byId("draft-page");
const databaseId = page.dataset.databaseId;
const apiPath = `/api/v1/databases/${encodeURIComponent(databaseId)}/drafts`;

const pageStatus = byId("draft-page-status");
const pageError = byId("draft-page-error");
const workspace = byId("draft-workspace");
const databaseName = byId("draft-database-name");
const targetName = byId("draft-target-name");
const access = byId("draft-access");

const listStatus = byId("draft-list-status");
const listError = byId("draft-list-error");
const listEmpty = byId("draft-list-empty");
const listTable = byId("draft-list-table");
const listBody = byId("draft-list-body");
const newButton = byId("draft-new");
const pagination = byId("draft-pagination");
const prevButton = byId("draft-prev");
const nextButton = byId("draft-next");
const pageNumber = byId("draft-page-number");

const form = byId("draft-form");
const heading = byId("draft-editor-heading");
const currentId = byId("draft-current-id");
const titleInput = byId("draft-title");
const sqlInput = byId("draft-sql");
const titleError = byId("draft-title-error");
const sqlError = byId("draft-sql-error");
const saveButton = byId("draft-save");
const saveMessage = byId("draft-save-message");

const detail = byId("draft-detail");
const detailAuthor = byId("draft-detail-author");
const detailCreated = byId("draft-detail-created");
const detailUpdated = byId("draft-detail-updated");
const detailSQL = byId("draft-detail-sql");

const executionPanel = byId("draft-execution");
const executeButton = byId("draft-execute");
const submitReviewButton = byId("draft-submit-review");
const explainButton = byId("draft-explain");
const explainMessage = byId("draft-explain-message");
const explainResult = byId("draft-explain-result");
const reloadButton = byId("draft-reload");
const executionRevision = byId("draft-execution-revision");
const executionState = byId("draft-execution-state");
const executionMessage = byId("draft-execution-message");
const executionResult = byId("draft-execution-result");
const resultStatus = byId("draft-result-status");
const resultVersion = byId("draft-result-version");
const resultID = byId("draft-result-id");
const resultError = byId("draft-result-error");

const maxSQLBytes = 48 * 1024;
const maxBodyBytes = 64 * 1024;
const pageLimit = 50;

let selectedDraft = null;
let busy = false;
let allowNavigation = false;
let executing = false;
let lockedExecution = null; // A sent request may have an unknown outcome. Never retry it automatically.
let cursors = [null];
let currentPage = 0;
let hasMore = false;
let nextCursor = null;

function updatePagination() {
    pagination.hidden = workspace.hidden;
    pageNumber.textContent = String(currentPage + 1);
    prevButton.disabled = busy || currentPage === 0;
    nextButton.disabled = busy || !hasMore;
}

function redirectToLogin() {
    allowNavigation = true;
    window.location.replace("/login");
}

function handleError(err, target = saveMessage) {
    if (err.status === 401) {
        redirectToLogin();
        return;
    }
    if (err.status === 403 && !isCSRFError(err)) {
        workspace.hidden = true;
        pageError.textContent = "Hak akses Edit ditolak atau telah dicabut. Kembali ke Dashboard.";
        pageError.hidden = false;
        return;
    }
    const message = err.status === 403 && isCSRFError(err)
        ? "Token CSRF tidak valid. Silakan coba lagi."
        : err.status === 409
            ? "Revisi draft berubah atau eksekusi masih terkunci. Muat ulang draft sebelum menyimpan."
            : err.status === 404
                ? "Draft tidak ditemukan pada database ini."
                : (err.message || "Operasi draft gagal.");
    setBanner(target, message, true);
}

const executionKey = (draft) => (draft ? `${draft.id}:${draft.revision}` : null);

function isDirty() {
    if (selectedDraft) {
        return titleInput.value !== selectedDraft.title || sqlInput.value !== selectedDraft.sql;
    }
    return Boolean(titleInput.value || sqlInput.value);
}

async function confirmDiscard() {
    return !isDirty() || await confirmAction({
        title: "Abaikan perubahan draft?",
        message: "Ada perubahan draft yang belum disimpan. Abaikan perubahan?",
        confirmLabel: "Abaikan Perubahan",
        tone: "danger"
    });
}

function updateExecutionControls() {
    const saved = selectedDraft !== null;
    const modified = saved && isDirty();
    const locked = saved && lockedExecution === executionKey(selectedDraft);

    executionPanel.hidden = !saved;
    executeButton.disabled = busy || !saved || modified || locked;
    if (submitReviewButton) submitReviewButton.disabled = busy || !saved || modified;
    explainButton.disabled = busy || !saved || modified;
    reloadButton.disabled = busy || !saved;
    executeButton.textContent = executing ? "Menjalankan Migration..." : "Execute Migration";

    if (!saved) return;
    executionRevision.textContent = selectedDraft.revision;
    executionState.textContent = locked
        ? "Eksekusi untuk revisi ini sudah diminta. Periksa History atau buka ulang draft sebelum tindakan lain."
        : modified
            ? "Ada perubahan belum disimpan. Simpan perubahan sebelum Execute."
            : "SQL tersimpan sudah siap ditinjau. Pastikan target dan revisi benar sebelum Execute.";
}

function resetExecutionFeedback() {
    lockedExecution = null;
    executionResult.hidden = true;
    explainResult.hidden = true;
    explainResult.textContent = "";
    setBanner(explainMessage, "");
    setBanner(executionMessage, "");
    updateExecutionControls();
}

function setBusy(value) {
    busy = value;
    newButton.disabled = value;
    updatePagination();
    saveButton.disabled = value;
    titleInput.disabled = value;
    sqlInput.disabled = value;

    for (const button of listBody.querySelectorAll("button")) {
        button.disabled = value;
    }
    saveButton.textContent = value
        ? "Memproses..."
        : selectedDraft ? "Simpan Perubahan" : "Buat Draft";
    updateExecutionControls();
}

function requireDraftShape(draft) {
    if (!draft || draft.database_id !== databaseId ||
        !positiveInt64(draft.id) || !positiveInt64(draft.revision) ||
        typeof draft.title !== "string" || typeof draft.sql !== "string") {
        throw new Error("Format detail draft dari backend tidak valid.");
    }
    return draft;
}

function showDraft(draft) {
    selectedDraft = requireDraftShape(draft);
    heading.textContent = "Edit Draft";
    currentId.textContent = `Draft ID: ${draft.id}`;
    currentId.hidden = false;

    titleInput.value = draft.title;
    sqlInput.value = draft.sql;
    detailAuthor.textContent = draft.created_by_username || "-";
    detailCreated.textContent = formatDate(draft.created_at);
    detailUpdated.textContent = formatDate(draft.updated_at);
    detailSQL.textContent = draft.sql;
    detail.hidden = false;

    setFieldError(titleInput, titleError, "");
    setFieldError(sqlInput, sqlError, "");
    setBanner(saveMessage, "");
    resetExecutionFeedback();
}

async function startNewDraft() {
    if (busy || !(await confirmDiscard()) || busy) return;
    selectedDraft = null;
    heading.textContent = "Buat Draft Baru";
    currentId.hidden = true;
    form.reset();
    detail.hidden = true;
    resetExecutionFeedback();
    setFieldError(titleInput, titleError, "");
    setFieldError(sqlInput, sqlError, "");
    setBanner(saveMessage, "");
    setBusy(false);
    titleInput.focus();
}

function renderDrafts(drafts) {
    listBody.replaceChildren();
    for (const draft of drafts) {
        const row = document.createElement("tr");
        addCell(row, draft.id);
        addCell(row, draft.title);
        addCell(row, draft.created_by_username || "-");
        addCell(row, formatDate(draft.updated_at));

        const actions = document.createElement("td");
        const openButton = document.createElement("button");
        openButton.type = "button";
        openButton.className = "draft-open-button";
        openButton.textContent = "Buka";
        openButton.disabled = busy;
        openButton.addEventListener("click", () => openDraft(String(draft.id)));

        actions.appendChild(openButton);
        row.appendChild(actions);
        listBody.appendChild(row);
    }
    listEmpty.hidden = drafts.length !== 0;
    listTable.hidden = drafts.length === 0;
}

async function loadDrafts(index = currentPage, reset = false) {
    if (reset) {
        cursors = [null];
        index = 0;
    }
    if (index < 0 || index >= cursors.length) return;
    listStatus.hidden = false;
    listStatus.textContent = "Memuat daftar draft...";
    listError.hidden = true;
    prevButton.disabled = true;
    nextButton.disabled = true;

    try {
        const cursor = cursors[index];
        const query = new URLSearchParams({ limit: String(pageLimit) });
        if (cursor !== null) query.set("after", cursor);

        const pageResult = parseCursorPage(
            await authenticatedRequest(`${apiPath}?${query}`),
            "drafts",
            "next_after"
        );
        for (const draft of pageResult.entries) {
            if (!draft || draft.database_id !== databaseId || !positiveInt64(draft.id)) {
                throw new Error("Identitas draft dari backend tidak valid.");
            }
        }
        if (pageResult.hasMore && (pageResult.nextCursor === cursor || cursors.slice(0, index + 1).includes(pageResult.nextCursor))) {
            throw new Error("Cursor draft berulang; pagination dihentikan demi keamanan.");
        }
        currentPage = index;
        hasMore = pageResult.hasMore;
        nextCursor = pageResult.nextCursor;
        cursors = cursors.slice(0, index + 1);
        renderDrafts(pageResult.entries);
        updatePagination();
    } catch (error) {
        hasMore = false;
        nextCursor = null;
        if (error.status === 401 || error.status === 403) {
            handleError(error, listError);
        } else {
            setBanner(listError, error.message || "Gagal memuat daftar draft.", true);
        }
    } finally {
        listStatus.hidden = true;
        updatePagination();
    }
}

prevButton.addEventListener("click", async () => {
    if (busy || currentPage === 0) return;
    setBusy(true);
    try { await loadDrafts(currentPage - 1); } finally { setBusy(false); }
});
nextButton.addEventListener("click", async () => {
    if (busy || !hasMore || !positiveInt64(nextCursor)) return;
    cursors[currentPage + 1] = nextCursor;
    setBusy(true);
    try { await loadDrafts(currentPage + 1); } finally { setBusy(false); }
});

async function openDraft(draftId) {
    if (busy || !(await confirmDiscard()) || busy) return;
    setBusy(true);
    setBanner(saveMessage, "");
    try {
        const result = await authenticatedRequest(`${apiPath}/${encodeURIComponent(draftId)}`);
        showDraft(result);
        form.scrollIntoView({ behavior: "smooth", block: "start" });
    } catch (err) {
        handleError(err);
    } finally {
        setBusy(false);
    }
}

function displayExecutionResult(result) {
    resultStatus.textContent = result.status;
    resultStatus.className = `migration-status migration-status--${result.status}`;
    resultVersion.textContent = result.version;
    resultID.textContent = result.execution_id;
    resultError.textContent = result.error_code || "-";
    executionResult.hidden = false;

    const feedback = executionFeedback(result.status);
    const progress = result.committed_batches != null && result.committed_rows != null
        ? ` Batch sudah di-commit: ${result.committed_batches}; row sudah di-commit: ${result.committed_rows}.` : "";
    const attempts = result.attempt_count != null && result.max_attempts != null
        ? ` Attempt: ${result.attempt_count}/${result.max_attempts}.` : "";
    const next = ["retry_wait", "recovering"].includes(result.status) && result.available_at
        ? ` Pemeriksaan/percobaan berikutnya paling cepat: ${formatDate(result.available_at)}.` : "";
    setBanner(executionMessage, feedback.message + progress + attempts + next, feedback.isError);
}

async function followJob(response) {
    const pending = ["queued", "running", "unknown", "recovering", "retry_wait"];
    let job = parseJobResponse(response, databaseId);
    displayExecutionResult({ ...job, version: "-", execution_id: `Job ${job.id}` });
    for (let poll = 0; poll < 30; poll++) {
        if (executionResult.isConnected === false) return;
        response = await authenticatedRequest(`/api/v1/databases/${encodeURIComponent(databaseId)}/jobs/${encodeURIComponent(job.id)}`);
        if (executionResult.isConnected === false) return;
        const current = parseJobResponse(response, databaseId);
        if (current.id !== job.id) throw new Error("Identitas job berubah. Periksa History.");
        job = current;
        const attempt = response.attempts?.at(-1);
        if (attempt?.version && !pending.includes(job.status)) {
            displayExecutionResult(parseExecutionResult({
                execution_id: attempt.execution_id, version: attempt.version,
                status: job.status, error_code: job.error_code,
                committed_batches: job.committed_batches, committed_rows: job.committed_rows
            }));
            return;
        }
        displayExecutionResult({ ...job, version: "-", execution_id: `Job ${job.id}` });
        if (!pending.includes(job.status)) return;
        if (poll < 29) await new Promise((resolve) => setTimeout(resolve, 1000));
    }
    setBanner(executionMessage, `Job ${job.id} masih ${job.status}. Pekerjaan berlanjut di worker; periksa History untuk hasil akhir.`);
}

async function executeDraft() {
    if (busy || !selectedDraft || isDirty() || lockedExecution === executionKey(selectedDraft)) return;

    const draft = selectedDraft;
    const revisionKey = executionKey(draft);
    setBusy(true);
    setBanner(executionMessage, "Memeriksa revisi terbaru dan hak akses...");
    executionResult.hidden = true;

    try {
        // A fresh GET catches stale editor data before asking the user to confirm.
        const latest = requireDraftShape(await authenticatedRequest(`${apiPath}/${encodeURIComponent(draft.id)}`));
        if (latest.revision !== draft.revision || latest.title !== draft.title || latest.sql !== draft.sql) {
            lockedExecution = revisionKey;
            setBanner(
                executionMessage,
                "Draft di server sudah berubah. Buka ulang draft untuk melihat SQL dan revisi terbaru; tidak ada migration dijalankan.",
                true
            );
            return;
        }

        const confirmed = await confirmAction({
            title: "Execute SQL Migration?",
            message: `Execute migration di ${databaseName.textContent} (${targetName.textContent})?\n\n` +
                `Draft: ${draft.title} (ID ${draft.id}, revisi ${draft.revision})\n\n` +
                "Periksa SQL tersimpan di bagian Detail Draft. Eksekusi mengubah database target. " +
                "SQL gagal akan di-rollback oleh backend. Lanjutkan?",
            confirmLabel: "Execute Migration",
            tone: "warning"
        });
        if (!confirmed) {
            setBanner(executionMessage, "Eksekusi dibatalkan. Tidak ada SQL dijalankan.");
            return;
        }

        lockedExecution = revisionKey;
        executing = true;
        updateExecutionControls();
        setBanner(executionMessage, "Mengirim migration ke antrean...");

        const response = await authenticatedRequest(
            `${apiPath}/${encodeURIComponent(draft.id)}/execute`,
            {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ revision: draft.revision })
            }
        );
        if (response?.job) {
            await followJob(response);
        } else {
            displayExecutionResult(parseExecutionResult(response));
        }
    } catch (err) {
        if (err.status === 401) {
            redirectToLogin();
            return;
        }
        if (err.status === 403 && !isCSRFError(err)) {
            workspace.hidden = true;
            pageError.textContent = "Izin Edit dicabut. Akses migration tidak tersedia.";
            pageError.hidden = false;
            return;
        }
        setBanner(executionMessage, executeErrorMessage(err), true);
    } finally {
        executing = false;
        setBusy(false);
    }
}

executeButton.addEventListener("click", executeDraft);
submitReviewButton?.addEventListener("click", async () => {
    if (busy || !selectedDraft || isDirty()) return;
    const draft = selectedDraft;
    const confirmed = await confirmAction({
        title: "Ajukan Review Migration?",
        message: `Ajukan Draft "${draft.title}" (ID ${draft.id}, revisi ${draft.revision}) untuk ditinjau dan disetujui oleh seluruh Leader Group pada database ini?`,
        confirmLabel: "Ajukan Review"
    });
    if (!confirmed || busy) return;

    setBusy(true);
    setBanner(executionMessage, "Mengirim pengajuan review ke leader group...");
    try {
        const result = await authenticatedRequest(`/api/v1/databases/${encodeURIComponent(databaseId)}/reviews`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ draft_id: draft.id, revision: draft.revision, action_type: "up" })
        });
        const count = Array.isArray(result?.review?.approvals) ? result.review.approvals.length : 0;
        setBanner(
            executionMessage,
            `Pengajuan review #${result?.review?.id ?? "-"} berhasil dibuat (Status: Pending Review, membutuhkan persetujuan ${count} leader group).`
        );
    } catch (err) {
        if (err.status === 409 && err.code === "group_leader_required") {
            setBanner(executionMessage, "Pengajuan ditolak: setiap group yang memiliki akses view atau edit ke database ini wajib memiliki Leader Group aktif.", true);
        } else if (err.status === 409) {
            setBanner(executionMessage, "Revisi draft ini sudah memiliki pengajuan review aktif atau revisi draft telah berubah.", true);
        } else {
            handleError(err, executionMessage);
        }
    } finally {
        setBusy(false);
    }
});
explainButton.addEventListener("click", async () => {
    if (busy || !selectedDraft || isDirty()) return;
    const draft = selectedDraft;
    setBusy(true);
    explainResult.hidden = true;
    setBanner(explainMessage, "Menganalisis query...");
    try {
        const result = await authenticatedRequest(`${apiPath}/${encodeURIComponent(draft.id)}/explain`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ revision: draft.revision })
        });
        if (result.revision !== draft.revision || !Array.isArray(result.statements)) {
            throw new Error("Format hasil analisis tidak valid.");
        }
        explainResult.textContent = result.statements.map((item, index) =>
            `Statement ${index + 1}:\n${item.sql.trim()}\n\n${item.error || JSON.stringify(item.plan, null, 2)}`
        ).join("\n\n");
        explainResult.hidden = false;
        setBanner(explainMessage, "Analisis selesai. Hasil adalah perkiraan PostgreSQL.");
    } catch (err) {
        handleError(err, explainMessage);
    } finally {
        setBusy(false);
    }
});

reloadButton.addEventListener("click", () => {
    if (selectedDraft) openDraft(selectedDraft.id);
});
titleInput.addEventListener("input", updateExecutionControls);
sqlInput.addEventListener("input", () => {
    explainResult.hidden = true;
    setBanner(explainMessage, "");
    updateExecutionControls();
});
newButton.addEventListener("click", startNewDraft);

form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (busy) return;

    setFieldError(titleInput, titleError, "");
    setFieldError(sqlInput, sqlError, "");
    setBanner(saveMessage, "");

    const title = titleInput.value.trim();
    const sql = sqlInput.value.trim();
    const titleBytes = utf8Bytes(title);
    const sqlBytes = utf8Bytes(sql);

    if (titleBytes < 1 || titleBytes > 200) {
        setFieldError(titleInput, titleError, "Isi judul dengan maksimal 200 karakter biasa; karakter tertentu dapat dihitung lebih dari satu.");
    }
    if (sqlBytes < 1 || sqlBytes > maxSQLBytes) {
        setFieldError(sqlInput, sqlError, "SQL harus diisi dan maksimal 48 KiB.");
    }
    if (!titleError.hidden || !sqlError.hidden) {
        (titleError.hidden ? sqlInput : titleInput).focus();
        return;
    }

    if (selectedDraft && title === selectedDraft.title && sql === selectedDraft.sql) {
        setBanner(saveMessage, "Tidak ada perubahan yang perlu disimpan.");
        return;
    }
    const payload = selectedDraft
        ? { title, sql, revision: selectedDraft.revision }
        : { title, sql };
    const serialized = JSON.stringify(payload);

    if (utf8Bytes(serialized) > maxBodyBytes) {
        setFieldError(sqlInput, sqlError, "Ukuran JSON melebihi 64 KiB. Kurangi ukuran SQL sebelum menyimpan.");
        return;
    }

    const updating = selectedDraft !== null;
    const url = updating ? `${apiPath}/${encodeURIComponent(String(selectedDraft.id))}` : apiPath;
    setBusy(true);

    try {
        const result = await authenticatedRequest(url, {
            method: updating ? "PATCH" : "POST",
            headers: { "Content-Type": "application/json" },
            body: serialized
        });
        showDraft(result);
        try {
            await loadDrafts(0, true);
            setBanner(saveMessage, updating ? "Perubahan draft berhasil disimpan." : "Draft baru berhasil dibuat.");
        } catch (refreshError) {
            if (refreshError.status === 401 || refreshError.status === 403) {
                handleError(refreshError);
            } else {
                setBanner(saveMessage, "Draft tersimpan, tetapi daftar gagal diperbarui.", true);
            }
        }
    } catch (err) {
        handleError(err);
    } finally {
        setBusy(false);
    }
});

window.addEventListener("beforeunload", (event) => {
    if (!allowNavigation && isDirty()) {
        event.preventDefault();
        event.returnValue = "";
    }
});

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;

        const result = await authenticatedRequest("/api/v1/databases");
        if (!Array.isArray(result?.databases)) {
            throw new Error("Format daftar database tidak valid.");
        }
        const database = result.databases.find((item) => String(item.id) === databaseId);
        if (!database) {
            throw new Error("Database tidak ditemukan atau tidak dapat diakses.");
        }
        if (database.access !== "edit") {
            throw new Error("Akses View Only tidak diizinkan membuka migration draft.");
        }

        databaseName.textContent = database.name;
        targetName.textContent = database.database_name;
        access.textContent = "View + Edit";
        workspace.hidden = false;

        initializeSchemaDiff(databaseId, (sql) => {
            if (busy || !confirmDiscard()) return false;
            selectedDraft = null;
            heading.textContent = "Buat Draft Baru";
            currentId.hidden = true;
            detail.hidden = true;
            form.reset();
            titleInput.value = "Schema diff";
            sqlInput.value = sql;
            resetExecutionFeedback();
            setFieldError(titleInput, titleError, "");
            setFieldError(sqlInput, sqlError, "");
            setBanner(saveMessage, "");
            form.scrollIntoView({ behavior: "smooth", block: "start" });
            return true;
        });

        await loadDrafts(0, true);
    } catch (err) {
        if (err.status === 401) {
            redirectToLogin();
            return;
        }
        if (err.status === 403) {
            workspace.hidden = true;
        }
        pageError.textContent = err.message || "Gagal memuat halaman draft.";
        pageError.hidden = false;
    } finally {
        pageStatus.hidden = true;
    }
}

initialize();
