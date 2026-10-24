import {
    authenticatedRequest,
    requireAuthenticatedUser
} from "./auth.js";

const page = document.getElementById("history-page");
const databaseId = page.dataset.databaseId;
const apiPath = `/api/v1/databases/${encodeURIComponent(databaseId)}/migrations`;

const pageStatus = document.getElementById("history-page-status");
const pageError = document.getElementById("history-page-error");
const workspace = document.getElementById("history-workspace");
const databaseName = document.getElementById("history-database-name");
const targetName = document.getElementById("history-target-name");
const access = document.getElementById("history-access");
const draftsLink = document.getElementById("history-drafts-link");

const refreshButton = document.getElementById("history-refresh");
const listStatus = document.getElementById("history-list-status");
const listError = document.getElementById("history-list-error");
const listEmpty = document.getElementById("history-list-empty");
const listTable = document.getElementById("history-list-table");
const listBody = document.getElementById("history-list-body");

const detail = document.getElementById("history-detail");
const detailStatus = document.getElementById("history-detail-status");
const detailError = document.getElementById("history-detail-error");
const detailContent = document.getElementById("history-detail-content");
const detailSQLSection = document.getElementById("history-detail-sql-section");
const detailSQL = document.getElementById("history-detail-sql");

let busy = false;

function showMessage(element, message) {
    element.textContent = message;
    element.hidden = !message;
}

function formatDate(value, unfinished = false) {
    if (value == null) return unfinished ? "Belum selesai" : "-";

    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "Tanggal tidak valid";

    return new Intl.DateTimeFormat("id-ID", {
        dateStyle: "medium",
        timeStyle: "short"
    }).format(date);
}

function statusLabel(status) {
    const labels = {
        running: "Berjalan",
        success: "Berhasil",
        failed: "Gagal",
        unknown: "Tidak pasti"
    };
    return labels[status] ?? String(status ?? "-");
}

function addCell(row, value) {
    const cell = document.createElement("td");
    cell.textContent = String(value);
    row.appendChild(cell);
    return cell;
}

function makeStatus(status) {
    const span = document.createElement("span");
    span.className = "migration-status";
    if (["running", "success", "failed", "unknown"].includes(status)) {
        span.classList.add(`migration-status--${status}`);
    }
    span.textContent = statusLabel(status);
    return span;
}

function versionString(version) {
    if (typeof version === "string" && /^[1-9][0-9]*$/.test(version)) {
        return BigInt(version) <= 9223372036854775807n ? version : null;
    }

    return null;
}

function setBusy(value) {
    busy = value;
    refreshButton.disabled = value;
    for (const button of listBody.querySelectorAll("button")) {
        button.disabled = value || button.dataset.invalidVersion === "true";
    }
}

function clearDetail() {
    detail.hidden = true;
    detailContent.hidden = true;
    detailSQLSection.hidden = true;
    detailSQL.textContent = "";
    showMessage(detailStatus, "");
    showMessage(detailError, "");
}

function handleRequestError(error, target) {
    if (error.status === 401) {
        window.location.replace("/login");
        return;
    }

    if (error.status === 403) {
        clearDetail();
        listBody.replaceChildren();
        workspace.hidden = true;
        showMessage(
            pageError,
            "Hak akses ke database telah ditolak atau dicabut. Kembali ke Dashboard."
        );
        return;
    }

    showMessage(
        target,
        error.status === 404
            ? "Riwayat atau versi migration tidak ditemukan."
            : (error.message || "Gagal memuat migration history.")
    );
}

function renderHistory(entries) {
    listBody.replaceChildren();
    let unsafeVersion = false;

    for (const entry of entries) {
        const row = document.createElement("tr");
        const version = versionString(entry.version);
        if (version === null) unsafeVersion = true;

        addCell(row, version ?? "Versi tidak aman");

        const statusCell = document.createElement("td");
        statusCell.appendChild(makeStatus(entry.status));
        row.appendChild(statusCell);

        addCell(row, entry.username || "-");
        addCell(row, formatDate(entry.started_at));
        addCell(row, formatDate(entry.finished_at, true));

        const actionCell = document.createElement("td");
        const button = document.createElement("button");
        button.type = "button";
        button.className = "history-open-button";
        button.textContent = "Detail";
        button.dataset.invalidVersion = String(version === null);
        button.disabled = busy || version === null;

        button.setAttribute(
            "aria-label",
            version === null
                ? "Versi migration tidak aman untuk dibuka"
                : `Lihat detail migration versi ${version}`
        );

        if (version !== null) {
            button.addEventListener("click", () => openHistory(version));
        }

        actionCell.appendChild(button);
        row.appendChild(actionCell);
        listBody.appendChild(row);
    }

    listEmpty.hidden = entries.length !== 0;
    listTable.hidden = entries.length === 0;

    if (unsafeVersion) {
        showMessage(
            listError,
            "Ada versi migration yang bukan string int64 valid. Muat ulang atau periksa data backend."
        );
    }
}

function showHistory(entry, version) {
    if (
        !entry ||
        entry.database_id !== databaseId ||
        versionString(entry.version) !== version ||
        typeof entry.sql !== "string"
    ) {
        throw new Error("Format detail migration dari backend tidak valid.");
    }

    const fields = {
        "history-detail-version": version,
        "history-detail-execution-id": entry.execution_id ?? "-",
        "history-detail-draft-revision": entry.draft_revision ?? "-",
        "history-detail-draft-id": entry.draft_id ?? "-",
        "history-detail-result": statusLabel(entry.status),
        "history-detail-executor": entry.username || "-",
        "history-detail-started": formatDate(entry.started_at),
        "history-detail-finished": formatDate(entry.finished_at, true),
        "history-detail-error-code": entry.error_code ?? "-"
    };

    for (const [id, value] of Object.entries(fields)) {
        document.getElementById(id).textContent = String(value);
    }

    detailSQL.textContent = entry.sql;
    detailContent.hidden = false;
    detailSQLSection.hidden = false;
}

async function openHistory(version) {
    if (busy) return;

    setBusy(true);
    detail.hidden = false;
    detailContent.hidden = true;
    detailSQLSection.hidden = true;
    detailSQL.textContent = "";
    showMessage(detailError, "");
    showMessage(detailStatus, `Memuat detail versi ${version}...`);

    try {
        const result = await authenticatedRequest(
            `${apiPath}/${encodeURIComponent(version)}`
        );
        showHistory(result, version);
    } catch (error) {
        handleRequestError(error, detailError);
    } finally {
        showMessage(detailStatus, "");
        setBusy(false);
    }
}

async function loadHistory() {
    if (busy) return;

    setBusy(true);
    clearDetail();
    listEmpty.hidden = true;
    listTable.hidden = true;
    showMessage(listError, "");
    showMessage(listStatus, "Memuat daftar migration...");

    try {
        const result = await authenticatedRequest(apiPath);

        if (!Array.isArray(result?.migrations)) {
            throw new Error("Format daftar migration dari backend tidak valid.");
        }

        renderHistory(result.migrations);
    } catch (error) {
        handleRequestError(error, listError);
    } finally {
        showMessage(listStatus, "");
        setBusy(false);
    }
}

refreshButton.addEventListener("click", loadHistory);

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;

        const result = await authenticatedRequest("/api/v1/databases");

        if (!Array.isArray(result?.databases)) {
            throw new Error("Format daftar database dari backend tidak valid.");
        }

        const database = result.databases.find((item) => String(item.id) === databaseId);

        if (!database || !["view", "edit"].includes(database.access)) {
            throw new Error("Database tidak ditemukan atau tidak dapat diakses.");
        }

        databaseName.textContent = database.name;
        targetName.textContent = database.database_name;

        access.textContent = database.access === "edit" ? "View + Edit" : "View Only";

        draftsLink.hidden = database.access !== "edit";
        workspace.hidden = false;

        await loadHistory();
    } catch (error) {
        handleRequestError(error, pageError);
    } finally {
        pageStatus.hidden = true;
    }
}

initialize();