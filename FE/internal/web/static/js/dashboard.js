import { authenticatedRequest, requireAuthenticatedUser } from "./auth.js";
import { positiveInt64 } from "./pagination.js";
import { byId, addCell, navigateTo } from "./ui.js";

const status = byId("dashboard-status");
const content = byId("dashboard-content");
const username = byId("welcome-username");
const databasesStatus = byId("databases-status");
const databasesError = byId("databases-error");
const databasesEmpty = byId("databases-empty");
const databasesTable = byId("databases-table");
const databasesBody = byId("databases-body");

const statDbCount = byId("stat-db-count");
const statPendingReviews = byId("stat-pending-reviews");
const adminShortcuts = byId("dashboard-admin-shortcuts");
const reviewsPanel = byId("dashboard-reviews-panel");
const reviewsEmpty = byId("dashboard-reviews-empty");
const reviewsList = byId("dashboard-reviews-list");

function actionLink(cell, id, resource, label, variant = "secondary") {
    const link = document.createElement("a");
    link.className = variant === "primary"
        ? "workspace-button"
        : "workspace-button workspace-button--subtle";
    link.href = `/databases/${encodeURIComponent(id)}/${resource}`;
    link.textContent = label;
    cell.appendChild(link);
}

async function loadPendingReviews() {
    if (!reviewsPanel) return;
    try {
        const result = await authenticatedRequest("/api/v1/reviews?limit=5&only_actionable=true");
        if (!Array.isArray(result?.reviews) || !Number.isSafeInteger(result.total) || result.total < 0) {
            throw new Error("Format daftar review dari backend tidak valid.");
        }
        const actionable = result.reviews;
        if (statPendingReviews) statPendingReviews.textContent = String(result.total);

        if (reviewsList && reviewsEmpty) {
            reviewsList.replaceChildren();
            reviewsEmpty.textContent = "Tidak ada pengajuan migration yang menunggu keputusan Anda.";
            for (const review of actionable) {
                const item = document.createElement("li");
                item.className = "pending-review-item";

                const info = document.createElement("div");
                info.className = "pending-review-info";
                const title = document.createElement("strong");
                title.textContent = `${review.database_name} — ${review.draft_title} (rev ${review.draft_revision})`;
                const meta = document.createElement("span");
                meta.textContent = `Aksi: ${review.action_type} · Pengaju: ${review.requested_by_username || "-"}`;
                info.append(title, meta);

                const link = document.createElement("a");
                link.className = "workspace-button";
                link.href = `/databases/${encodeURIComponent(review.database_id)}/reviews/${encodeURIComponent(review.id)}`;
                link.textContent = "Tinjau";

                item.append(info, link);
                reviewsList.appendChild(item);
            }
            reviewsPanel.hidden = false;
            reviewsEmpty.hidden = actionable.length !== 0;
            reviewsList.hidden = actionable.length === 0;
        }
    } catch {
        if (statPendingReviews) statPendingReviews.textContent = "–";
        if (reviewsPanel && reviewsEmpty) {
            reviewsPanel.hidden = false;
            reviewsEmpty.hidden = false;
            reviewsEmpty.textContent = "Review tidak dapat dimuat. Coba buka halaman Review & Approval.";
        }
    }
}

async function loadDatabases(isAdmin) {
    databasesStatus.hidden = false;
    databasesError.hidden = true;
    databasesEmpty.hidden = true;
    databasesTable.hidden = true;
    databasesBody.replaceChildren();
    databasesStatus.textContent = "Memuat database...";
    try {
        const result = await authenticatedRequest("/api/v1/databases");
        if (!Array.isArray(result?.databases) || !result.databases.every((db) =>
            positiveInt64(db.id) && ["view", "edit"].includes(db.access))) {
            throw new Error("Format daftar database atau izin dari backend tidak valid.");
        }
        if (statDbCount) statDbCount.textContent = String(result.databases.length);

        for (const database of result.databases) {
            const row = document.createElement("tr");
            addCell(row, database.id);
            addCell(row, database.name);
            addCell(row, database.database_name);

            const accessCell = document.createElement("td");
            const badge = document.createElement("span");
            badge.className = database.access === "edit"
                ? "access-badge access-badge--edit"
                : "access-badge access-badge--view";
            badge.textContent = database.access === "edit" ? "View + Edit" : "View Only";
            accessCell.appendChild(badge);
            row.appendChild(accessCell);

            const links = document.createElement("td");
            links.className = "database-workspace-links";
            actionLink(links, database.id, "viewer", "Viewer", "secondary");
            actionLink(links, database.id, "history", "History", "secondary");
            actionLink(links, database.id, "jobs", "Jobs", "secondary");
            if (database.access === "edit") actionLink(links, database.id, "drafts", "Kelola Draft", "primary");
            row.appendChild(links);
            databasesBody.appendChild(row);
        }
        databasesEmpty.textContent = isAdmin
            ? "Belum ada database yang bisa diakses. Platform Admin tidak otomatis memiliki grant target; tambahkan diri ke group yang diberi izin jika perlu."
            : "Belum ada database yang bisa diakses. Hubungi Platform Admin untuk mendapatkan grant melalui group.";
        databasesEmpty.hidden = result.databases.length !== 0;
        databasesTable.hidden = result.databases.length === 0;
    } catch (error) {
        databasesBody.replaceChildren();
        if (error.status === 401) {
            navigateTo("/login", { replace: true });
            return;
        }
        databasesError.textContent = error.status === 403
            ? "Hak akses daftar database telah ditolak atau dicabut. Muat ulang setelah login kembali."
            : (error.message || "Gagal memuat daftar database.");
        databasesError.hidden = false;
    } finally {
        databasesStatus.hidden = true;
    }
}

async function loadDashboard() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;
        username.textContent = user.username;
        if (adminShortcuts) adminShortcuts.hidden = !user.is_platform_admin;
        content.hidden = false;
        status.hidden = true;
        await loadDatabases(user.is_platform_admin);
        await loadPendingReviews();
    } catch (error) {
        status.textContent = error.message || "Gagal memuat dashboard.";
        status.className = "error-message";
    }
}

loadDashboard();
