import {
    authenticatedRequest,
    requireAuthenticatedUser
} from "./auth.js";

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

const apiPath = `/api/v1/databases/${encodeURIComponent(databaseId)}/drafts`;

const encoder = new TextEncoder();
const maxBodyBytes = 1024 * 1024;

let selectedDraft = null;
let busy = false;
let allowNavigation = false;

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

    if (err.status === 403 && err.code !== "csrf_invalid") {
        workspace.hidden = true;
        pageError.textContent = "Hak akses Edit ditolak atau telah dicabut. " + "Kembali ke Dashboard.";
        pageError.hidden = false;
        return;
    }

    const message =
        err.status === 403 && err.code === "csrf_invalid"
            ? "Token CSRF tidak valid. Silakan coba lagi."
            : err.status === 409
                ? "Draft sudah dieksekusi/dikunci dan tidak bisa diubah."
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

function setBusy(value) {
    busy = value;

    newButton.disabled = value;
    saveButton.disabled = value;
    titleInput.disabled = value;
    sqlInput.disabled = value;

    for (const button of listBody.querySelectorAll("button")) {
        button.disabled = value;
    }

    saveButton.textContent = value
        ? "Menyimpan..."
        : selectedDraft
            ? "Simpan Perubahan"
            : "Buat Draft";
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

function requireDraftShape(draft) {
    if (!draft || String(draft.database_id) !== databaseId || !/^[1-9][0-9]*$/.test(String(draft.id)) ||
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
}

function startNewDraft() {
    if (busy || !confirmDiscard()) return;

    selectedDraft = null;

    heading.textContent = "Buat Draft Baru";
    currentId.hidden = true;

    form.reset();
    detail.hidden = true;

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

async function loadDrafts() {
    listStatus.hidden = false;
    listStatus.textContent = "Memuat daftar draft...";
    listError.hidden = true;

    try {
        const result = await authenticatedRequest(apiPath);

        if (!Array.isArray(result?.drafts)) {
            throw new Error("Format daftar draft dari backend tidak valid.");
        }

        renderDrafts(result.drafts);
    } finally {
        listStatus.hidden = true;
    }
}

async function openDraft(draftId) {
    if (busy || !confirmDiscard()) return;

    setBusy(true);
    showMessage(saveMessage, "");

    try {
        const result = await authenticatedRequest(
            `${apiPath}/${encodeURIComponent(draftId)}`
        );

        showDraft(result?.draft);

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

    if (sqlBytes < 1 || sqlBytes > maxBodyBytes) {
        setFieldError(sqlInput, sqlError, "SQL harus diisi dan maksimal 1 MiB.");
    }

    if (!titleError.hidden || !sqlError.hidden) {
        (titleError.hidden ? sqlInput : titleInput).focus();
        return;
    }

    const payload = selectedDraft ? {} : { title, sql };

    if (selectedDraft) {
        if (title !== selectedDraft.title) {
            payload.title = title;
        }

        if (sql !== selectedDraft.sql) {
            payload.sql = sql;
        }

        if (Object.keys(payload).length === 0) {
            showMessage(saveMessage, "Tidak ada perubahan yang perlu disimpan.");
            return;
        }
    }

    const serialized = JSON.stringify(payload);

    if (encoder.encode(serialized).length > maxBodyBytes) {
        setFieldError(sqlInput, sqlError, "Ukuran JSON melebihi 1 MiB. " + "Kurangi ukuran SQL sebelum menyimpan.");
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

        showDraft(result?.draft);

        try {
            await loadDrafts();

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

        if (database.effective_access !== "edit") {
            throw new Error("Akses View Only tidak diizinkan membuka migration draft.");
        }

        databaseName.textContent = database.name;
        targetName.textContent = database.database_name;
        access.textContent = "View + Edit";

        workspace.hidden = false;

        await loadDrafts();

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