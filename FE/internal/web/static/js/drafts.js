import {
    authenticatedRequest,
    requireAuthenticatedUser,
    isCSRFError
} from "./auth.js";
import { executionFeedback, parseExecutionResult, executeErrorMessage } from "./execution.js";
import { positiveInt64, parseCursorPage } from "./pagination.js";

const page = document.getElementById("draft-page");
const databaseId = page.dataset.databaseId;

const pageStatus = document.getElementById("draft-page-status");
const pageError = document.getElementById("draft-page-error");
const workspace = document.getElementById("draft-workspace");
const databaseName = document.getElementById("draft-database-name");
const targetName = document.getElementById("draft-target-name");
const access = document.getElementById("draft-access");

const listStatus = document.getElementById("draft-list-status");
const listError = document.getElementById("draft-list-error");
const listEmpty = document.getElementById("draft-list-empty");
const listTable = document.getElementById("draft-list-table");
const listBody = document.getElementById("draft-list-body");
const newButton = document.getElementById("draft-new");
const pagination = document.getElementById("draft-pagination");
const prevButton = document.getElementById("draft-prev");
const nextButton = document.getElementById("draft-next");
const pageNumber = document.getElementById("draft-page-number");

const form = document.getElementById("draft-form");
const heading = document.getElementById("draft-editor-heading");
const currentId = document.getElementById("draft-current-id");
const titleInput = document.getElementById("draft-title");
const sqlInput = document.getElementById("draft-sql");
const titleError = document.getElementById("draft-title-error");
const sqlError = document.getElementById("draft-sql-error");
const saveButton = document.getElementById("draft-save");
const saveMessage = document.getElementById("draft-save-message");

const detail = document.getElementById("draft-detail");
const detailAuthor = document.getElementById("draft-detail-author");
const detailCreated = document.getElementById("draft-detail-created");
const detailUpdated = document.getElementById("draft-detail-updated");
const detailSQL = document.getElementById("draft-detail-sql");
const executionPanel = document.getElementById("draft-execution");
const executeButton = document.getElementById("draft-execute");
const reloadButton = document.getElementById("draft-reload");
const executionRevision = document.getElementById("draft-execution-revision");
const executionState = document.getElementById("draft-execution-state");
const executionMessage = document.getElementById("draft-execution-message");
const executionResult = document.getElementById("draft-execution-result");
const resultStatus = document.getElementById("draft-result-status");
const resultVersion = document.getElementById("draft-result-version");
const resultID = document.getElementById("draft-result-id");
const resultError = document.getElementById("draft-result-error");

const apiPath = `/api/v1/databases/${encodeURIComponent(databaseId)}/drafts`;

const encoder = new TextEncoder();
const maxSQLBytes = 48 * 1024;
const maxBodyBytes = 64 * 1024;

let selectedDraft = null;
let busy = false;
let allowNavigation = false;
let executing = false;
let lockedExecution = null; // A sent request may have an unknown outcome. Never retry it automatically.
let cursors = [null];
let currentPage = 0;
let hasMore = false;
let nextCursor = null;
const pageLimit = 50;

function updatePagination() {
    pagination.hidden = workspace.hidden;
    pageNumber.textContent = `Halaman ${currentPage + 1}`;
    prevButton.disabled = busy || currentPage === 0;
    nextButton.disabled = busy || !hasMore;
}

function showMessage(element, message, isError = false) {
    element.textContent = message;
    element.className = isError ? "error-message" : "success-message";
    element.hidden = !message;
}

function setFieldError(input, target, message) {
    showMessage(target, message, true);

    if (message) {
        input.setAttribute("aria-invalid", "true");
    } else {
        input.removeAttribute("aria-invalid");
    }
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
        pageError.textContent = "Hak akses Edit ditolak atau telah dicabut. " + "Kembali ke Dashboard.";
        pageError.hidden = false;
        return;
    }

    const message =
        err.status === 403 && isCSRFError(err)
            ? "Token CSRF tidak valid. Silakan coba lagi."
            : err.status === 409
                ? "Revisi draft berubah atau eksekusi masih terkunci. Muat ulang draft sebelum menyimpan."
                : err.status === 404
                    ? "Draft tidak ditemukan pada database ini."
                    : (err.message || "Operasi draft gagal.");

    showMessage(target, message, true);
}

function formatDate(value) {
    if (!value) return "-";

    const date = new Date(value);

    return Number.isNaN(date.getTime())
        ? "-"
        : new Intl.DateTimeFormat("id-ID", {
            dateStyle: "medium",
            timeStyle: "short"
        }).format(date);
}

function executionKey(draft) {
    return draft ? `${draft.id}:${draft.revision}` : null;
}

function updateExecutionControls() {
    const saved = selectedDraft !== null;
    const modified = saved && isDirty();
    const locked = saved && lockedExecution === executionKey(selectedDraft);

    executionPanel.hidden = !saved;
    executeButton.disabled = busy || !saved || modified || locked;
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
    showMessage(executionMessage, "");
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
        : selectedDraft
            ? "Simpan Perubahan"
            : "Buat Draft";
    updateExecutionControls();
}

function isDirty() {
    if (selectedDraft) {
        return titleInput.value !== selectedDraft.title || sqlInput.value !== selectedDraft.sql;
    }

    return Boolean(titleInput.value || sqlInput.value);
}

function confirmDiscard() {
    return !isDirty() || window.confirm("Ada perubahan draft yang belum disimpan. " + "Abaikan perubahan?");
}

function validInt64String(value) {
    return typeof value === "string" && /^[1-9][0-9]*$/.test(value) &&
        BigInt(value) <= 9223372036854775807n;
}

function requireDraftShape(draft) {
    if (!draft || draft.database_id !== databaseId ||
        !validInt64String(draft.id) || !validInt64String(draft.revision) ||
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
    showMessage(saveMessage, "");
    resetExecutionFeedback();
}

function startNewDraft() {
    if (busy || !confirmDiscard()) return;

    selectedDraft = null;

    heading.textContent = "Buat Draft Baru";
    currentId.hidden = true;

    form.reset();
    detail.hidden = true;
    resetExecutionFeedback();

    setFieldError(titleInput, titleError, "");
    setFieldError(sqlInput, sqlError, "");
    showMessage(saveMessage, "");

    setBusy(false);
    titleInput.focus();
}

function appendCell(row, value) {
    const cell = document.createElement("td");
    cell.textContent = String(value ?? "-");
    row.appendChild(cell);
    return cell;
}

function renderDrafts(drafts) {
    listBody.replaceChildren();

    for (const draft of drafts) {
        const row = document.createElement("tr");

        appendCell(row, draft.id);
        appendCell(row, draft.title);
        appendCell(row, draft.created_by_username || "-");
        appendCell(row, formatDate(draft.updated_at));

        const actions = document.createElement("td");
        const openButton = document.createElement("button");

        openButton.type = "button";
        openButton.className = "draft-open-button";
        openButton.textContent = "Buka";
        openButton.disabled = busy;

        openButton.addEventListener("click", () => {
            openDraft(String(draft.id));
        });

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
            await authenticatedRequest(`${apiPath}?${query}`), "drafts", "next_after"
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
            showMessage(listError, error.message || "Gagal memuat daftar draft.", true);
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
    if (busy || !confirmDiscard()) return;

    setBusy(true);
    showMessage(saveMessage, "");

    try {
        const result = await authenticatedRequest(
            `${apiPath}/${encodeURIComponent(draftId)}`
        );

        showDraft(result);

        form.scrollIntoView({
            behavior: "smooth",
            block: "start"
        });
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
    showMessage(executionMessage, feedback.message, feedback.isError);
}

async function executeDraft() {
    if (busy || !selectedDraft || isDirty() || lockedExecution === executionKey(selectedDraft)) return;

    const draft = selectedDraft;
    const revisionKey = executionKey(draft);
    setBusy(true);
    showMessage(executionMessage, "Memeriksa revisi terbaru dan hak akses...");
    executionResult.hidden = true;

    try {
        // A fresh GET catches stale editor data before asking the user to confirm.
        const latest = requireDraftShape(await authenticatedRequest(
            `${apiPath}/${encodeURIComponent(draft.id)}`
        ));
        if (latest.revision !== draft.revision || latest.title !== draft.title || latest.sql !== draft.sql) {
            lockedExecution = revisionKey;
            showMessage(executionMessage,
                "Draft di server sudah berubah. Buka ulang draft untuk melihat SQL dan revisi terbaru; tidak ada migration dijalankan.", true);
            return;
        }

        const confirmed = window.confirm(
            `Execute migration di ${databaseName.textContent} (${targetName.textContent})?\n\n` +
            `Draft: ${draft.title} (ID ${draft.id}, revisi ${draft.revision})\n\n` +
            "Periksa SQL tersimpan di bagian Detail Draft. Eksekusi mengubah database target. " +
            "SQL gagal akan di-rollback oleh backend. Lanjutkan?"
        );
        if (!confirmed) {
            showMessage(executionMessage, "Eksekusi dibatalkan. Tidak ada SQL dijalankan.");
            return;
        }

        lockedExecution = revisionKey;
        executing = true;
        updateExecutionControls();
        showMessage(executionMessage, "Permintaan eksekusi dikirim. Menunggu hasil backend...");
        const result = parseExecutionResult(await authenticatedRequest(
            `${apiPath}/${encodeURIComponent(draft.id)}/execute`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ revision: draft.revision })
            }
        ));
        displayExecutionResult(result);
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
        showMessage(executionMessage, executeErrorMessage(err), true);
    } finally {
        executing = false;
        setBusy(false);
    }
}

executeButton.addEventListener("click", executeDraft);
reloadButton.addEventListener("click", () => {
    if (selectedDraft) openDraft(selectedDraft.id);
});
titleInput.addEventListener("input", updateExecutionControls);
sqlInput.addEventListener("input", updateExecutionControls);

newButton.addEventListener("click", startNewDraft);

form.addEventListener("submit", async (event) => {
    event.preventDefault();

    if (busy) return;

    setFieldError(titleInput, titleError, "");
    setFieldError(sqlInput, sqlError, "");
    showMessage(saveMessage, "");

    const title = titleInput.value.trim();
    const sql = sqlInput.value.trim();

    const titleBytes = encoder.encode(title).length;
    const sqlBytes = encoder.encode(sql).length;

    if (titleBytes < 1 || titleBytes > 200) {
        setFieldError(titleInput, titleError, "Judul harus 1-200 byte.");
    }

    if (sqlBytes < 1 || sqlBytes > maxSQLBytes) {
        setFieldError(sqlInput, sqlError, "SQL harus diisi dan maksimal 48 KiB.");
    }

    if (!titleError.hidden || !sqlError.hidden) {
        (titleError.hidden ? sqlInput : titleInput).focus();
        return;
    }

    if (selectedDraft && title === selectedDraft.title && sql === selectedDraft.sql) {
        showMessage(saveMessage, "Tidak ada perubahan yang perlu disimpan.");
        return;
    }
    const payload = selectedDraft
        ? { title, sql, revision: selectedDraft.revision }
        : { title, sql };

    const serialized = JSON.stringify(payload);

    if (encoder.encode(serialized).length > maxBodyBytes) {
        setFieldError(sqlInput, sqlError, "Ukuran JSON melebihi 64 KiB. " + "Kurangi ukuran SQL sebelum menyimpan.");
        return;
    }

    const updating = selectedDraft !== null;

    const url = updating ? `${apiPath}/${encodeURIComponent(String(selectedDraft.id))}` : apiPath;

    setBusy(true);

    try {
        const result = await authenticatedRequest(url, {
            method: updating ? "PATCH" : "POST",
            headers: {
                "Content-Type": "application/json"
            },
            body: serialized
        });

        showDraft(result);

        try {
            await loadDrafts(0, true);

            showMessage(
                saveMessage,
                updating
                    ? "Perubahan draft berhasil disimpan."
                    : "Draft baru berhasil dibuat."
            );
        } catch (refreshError) {
            if (refreshError.status === 401 || refreshError.status === 403) {
                handleError(refreshError);
            } else {
                showMessage(
                    saveMessage,
                    "Draft tersimpan, tetapi daftar gagal diperbarui.",
                    true
                );
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
