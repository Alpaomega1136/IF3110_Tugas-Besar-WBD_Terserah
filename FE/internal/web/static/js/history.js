import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { positiveInt64, parseCursorPage, parseReconcileResult } from "./pagination.js";
import { byId, addCell, formatDate } from "./ui.js";

const page = byId("history-page");
const databaseId = page.dataset.databaseId;
const apiPath = `/api/v1/databases/${encodeURIComponent(databaseId)}/migrations`;

const pageStatus = byId("history-page-status");
const pageError = byId("history-page-error");
const workspace = byId("history-workspace");
const databaseName = byId("history-database-name");
const targetName = byId("history-target-name");
const access = byId("history-access");
const draftsLink = byId("history-drafts-link");

const refreshButton = byId("history-refresh");
const listStatus = byId("history-list-status");
const listError = byId("history-list-error");
const listEmpty = byId("history-list-empty");
const listTable = byId("history-list-table");
const listBody = byId("history-list-body");
const pagination = byId("history-pagination");
const prevButton = byId("history-prev");
const nextButton = byId("history-next");
const pageNumber = byId("history-page-number");
const pendingNotice = byId("history-pending-notice");

const reconcilePanel = byId("history-reconcile-panel");
const reconcileButton = byId("history-reconcile");
const reconcileStatus = byId("history-reconcile-status");
const reconcileError = byId("history-reconcile-error");
const reconcileResult = byId("history-reconcile-result");
const reconcileSummary = byId("history-reconcile-summary");
const reconcileTable = byId("history-reconcile-table");
const reconcileBody = byId("history-reconcile-body");

const detail = byId("history-detail");
const detailStatus = byId("history-detail-status");
const detailError = byId("history-detail-error");
const detailContent = byId("history-detail-content");
const detailSQLSection = byId("history-detail-sql-section");
const detailSQL = byId("history-detail-sql");

const pageLimit = 50;
const STATUS_LABELS = {
    running: "Berjalan",
    success: "Berhasil",
    failed: "Gagal",
    unknown: "Tidak pasti"
};

let busy = false;
let canEdit = false;
let cursors = [null];
let currentPage = 0;
let nextCursor = null;
let hasMore = false;

function showMessage(element, message) {
    element.textContent = message;
    element.hidden = !message;
}

const formatHistoryDate = (value, unfinished = false) =>
    formatDate(value, { nullLabel: unfinished ? "Belum selesai" : "-", invalidLabel: "Tanggal tidak valid" });

const statusLabel = (status) => STATUS_LABELS[status] ?? String(status ?? "-");

function makeStatus(status) {
    const span = document.createElement("span");
    span.className = "migration-status";
    if (["running", "success", "failed", "unknown"].includes(status)) {
        span.className += ` migration-status--${status}`;
    }
    span.textContent = statusLabel(status);
    return span;
}

const versionString = (version) => (positiveInt64(version) ? version : null);

function updatePagination() {
    pagination.hidden = workspace.hidden;
    pageNumber.textContent = `Halaman ${currentPage + 1}`;
    prevButton.disabled = busy || currentPage === 0;
    nextButton.disabled = busy || !hasMore;
}

function setBusy(value) {
    busy = value;
    refreshButton.disabled = value;
    reconcileButton.disabled = value || !canEdit;
    updatePagination();
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

function clearPrivateData() {
    listBody.replaceChildren();
    listTable.hidden = true;
    listEmpty.hidden = true;
    pagination.hidden = true;
    pendingNotice.hidden = true;
    reconcilePanel.hidden = true;
    reconcileResult.hidden = true;
    clearDetail();
    canEdit = false;
}

function handleRequestError(error, target) {
    if (error.status === 401) {
        clearPrivateData();
        workspace.hidden = true;
        window.location.replace("/login");
        return;
    }
    if (error.status === 403 && !isCSRFError(error)) {
        clearPrivateData();
        workspace.hidden = true;
        showMessage(pageError, "Hak akses ke database telah ditolak atau dicabut. Kembali ke Dashboard.");
        return;
    }

    const messages = {
        migration_busy: "Migration lain masih berjalan. Periksa History dan coba reconcile secara manual nanti.",
        execution_conflict: "Status eksekusi berkonflik. Muat ulang History sebelum melanjutkan.",
        target_history_unprotected: "History target atau isolasi executor belum aman. Minta administrator PostgreSQL menjalankan BE/sql/provision_target_history.sql dan menyiapkan executor khusus database ini.",
        target_identity_conflict: "Identitas target berubah atau bentrok. Periksa konfigurasi koneksi.",
        target_unavailable: "Database target tidak dapat dijangkau. Pulihkan koneksi, lalu coba kembali secara manual.",
        migration_unavailable: "Layanan migration belum tersedia. Periksa backend/target dan coba kembali secara manual."
    };
    const msg = error.status === 404
        ? "Riwayat atau versi migration tidak ditemukan."
        : isCSRFError(error)
            ? "Token CSRF ditolak. Muat ulang halaman dan login ulang jika diperlukan."
            : messages[error.code] || (error.status === 409
                ? "Terjadi konflik migration. Muat ulang History dan periksa backend."
                : error.status === 503
                    ? "Target migration tidak tersedia. Coba kembali secara manual setelah pulih."
                    : (error.message || "Operasi migration history gagal."));
    showMessage(target, msg);
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

        addCell(row, `${entry.draft_id ?? "-"} / ${entry.draft_revision ?? "-"}`);
        addCell(row, entry.execution_id ?? "-");
        addCell(row, entry.error_code ?? "-");
        addCell(row, entry.username || "-");
        addCell(row, formatHistoryDate(entry.started_at));
        addCell(row, formatHistoryDate(entry.finished_at, true));

        const actionCell = document.createElement("td");
        const button = document.createElement("button");
        button.type = "button";
        button.className = "history-open-button";
        button.textContent = "Detail";
        button.dataset.invalidVersion = String(version === null);
        button.disabled = busy || version === null;
        button.setAttribute(
            "aria-label",
            version === null ? "Versi migration tidak aman untuk dibuka" : `Lihat detail migration versi ${version}`
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

    const pendingCount = entries.filter((item) => ["running", "unknown"].includes(item.status)).length;
    showMessage(
        pendingNotice,
        pendingCount
            ? `${pendingCount} eksekusi pada halaman ini belum final. ${canEdit ? "Gunakan Reconcile jika diperlukan." : "Mintalah pengguna Edit memeriksa statusnya."}`
            : ""
    );

    if (unsafeVersion) {
        showMessage(listError, "Ada versi migration yang bukan string int64 valid. Muat ulang atau periksa data backend.");
    }
}

function showHistory(entry, version) {
    if (!entry || entry.database_id !== databaseId || versionString(entry.version) !== version || typeof entry.sql !== "string") {
        throw new Error("Format detail migration dari backend tidak valid.");
    }
    const fields = {
        "history-detail-version": version,
        "history-detail-execution-id": entry.execution_id ?? "-",
        "history-detail-draft-revision": entry.draft_revision ?? "-",
        "history-detail-draft-id": entry.draft_id ?? "-",
        "history-detail-result": statusLabel(entry.status),
        "history-detail-executor": entry.username || "-",
        "history-detail-started": formatHistoryDate(entry.started_at),
        "history-detail-finished": formatHistoryDate(entry.finished_at, true),
        "history-detail-error-code": entry.error_code ?? "-"
    };
    for (const [id, value] of Object.entries(fields)) {
        byId(id).textContent = String(value);
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
        const result = await authenticatedRequest(`${apiPath}/${encodeURIComponent(version)}`);
        showHistory(result, version);
    } catch (error) {
        handleRequestError(error, detailError);
    } finally {
        showMessage(detailStatus, "");
        setBusy(false);
    }
}

async function loadHistory(index = currentPage, reset = false) {
    if (busy) return;
    if (reset) {
        cursors = [null];
        index = 0;
    }
    if (index < 0 || index >= cursors.length) return;

    setBusy(true);
    clearDetail();
    showMessage(listError, "");
    showMessage(listStatus, "Memuat daftar migration...");
    try {
        const cursor = cursors[index];
        const query = new URLSearchParams({ limit: String(pageLimit) });
        if (cursor !== null) query.set("before", cursor);

        const result = parseCursorPage(
            await authenticatedRequest(`${apiPath}?${query}`),
            "migrations",
            "next_before"
        );
        for (const entry of result.entries) {
            if (!entry || entry.database_id !== databaseId || !positiveInt64(entry.version)) {
                throw new Error("Identitas versi migration dari backend tidak valid.");
            }
        }
        if (result.hasMore && (result.nextCursor === cursor || cursors.slice(0, index + 1).includes(result.nextCursor))) {
            throw new Error("Cursor migration berulang; pagination dihentikan demi keamanan.");
        }
        currentPage = index;
        hasMore = result.hasMore;
        nextCursor = result.nextCursor;
        cursors = cursors.slice(0, index + 1);
        renderHistory(result.entries);
    } catch (error) {
        hasMore = false;
        nextCursor = null;
        handleRequestError(error, listError);
    } finally {
        showMessage(listStatus, "");
        setBusy(false);
    }
}

function renderReconciled(items) {
    reconcileBody.replaceChildren();
    const counts = { success: 0, failed: 0, running: 0, unknown: 0 };
    for (const item of items) {
        counts[item.status]++;
        const row = document.createElement("tr");
        addCell(row, item.version);
        addCell(row, item.execution_id || "-");
        const statusCell = document.createElement("td");
        statusCell.appendChild(makeStatus(item.status));
        row.appendChild(statusCell);
        addCell(row, item.error_code || "-");
        reconcileBody.appendChild(row);
    }
    reconcileResult.hidden = false;
    reconcileTable.hidden = items.length === 0;
    reconcileSummary.textContent = items.length === 0
        ? "Tidak ada eksekusi running/unknown yang perlu direconcile. Tidak ada SQL yang dijalankan."
        : `${items.length} eksekusi diperiksa: ${counts.success} success, ${counts.failed} failed, ${counts.unknown} unknown, ${counts.running} running. Tidak ada SQL yang dijalankan ulang.`;
}

async function reconcileHistory() {
    if (busy || !canEdit) return;
    if (!(await confirmAction({
        title: "Reconcile migration?",
        message: "Periksa dan perbarui status migration yang belum pasti? SQL tidak dijalankan ulang.",
        confirmLabel: "Reconcile"
    })) || busy || !canEdit) return;

    setBusy(true);
    showMessage(reconcileError, "");
    showMessage(reconcileStatus, "Memeriksa izin Edit dan receipt migration...");
    reconcileResult.hidden = true;
    let completed = false;
    try {
        // Role and grants may have changed while this page was open.
        const accessResult = await authenticatedRequest("/api/v1/databases");
        if (!Array.isArray(accessResult?.databases) ||
            !accessResult.databases.some((database) => database.id === databaseId && database.access === "edit")) {
            const error = new Error("Izin Edit sudah tidak tersedia. Kembali ke Dashboard.");
            error.status = 403;
            throw error;
        }
        const items = parseReconcileResult(await authenticatedRequest(`${apiPath}/reconcile`, { method: "POST" }));
        renderReconciled(items);
        completed = true;
    } catch (error) {
        handleRequestError(error, reconcileError);
    } finally {
        setBusy(false);
        showMessage(reconcileStatus, "");
    }
    if (completed) await loadHistory(0, true);
}

refreshButton.addEventListener("click", () => loadHistory(0, true));
prevButton.addEventListener("click", () => loadHistory(currentPage - 1));
nextButton.addEventListener("click", () => {
    if (busy || !hasMore || !positiveInt64(nextCursor)) return;
    cursors[currentPage + 1] = nextCursor;
    return loadHistory(currentPage + 1);
});
reconcileButton.addEventListener("click", reconcileHistory);

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

        canEdit = database.access === "edit";
        draftsLink.hidden = !canEdit;
        reconcilePanel.hidden = !canEdit;
        workspace.hidden = false;

        await loadHistory(0, true);
    } catch (error) {
        handleRequestError(error, pageError);
    } finally {
        pageStatus.hidden = true;
    }
}

initialize();
