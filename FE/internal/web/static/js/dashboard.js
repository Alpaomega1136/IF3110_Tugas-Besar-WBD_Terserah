import { authenticatedRequest, requireAuthenticatedUser, logout } from "./auth.js";

const byId = (id) => document.getElementById(id);
const status = byId("dashboard-status");
const content = byId("dashboard-content");
const username = byId("welcome-username");
const role = byId("account-role");
const adminPanel = byId("admin-panel");
const logoutButton = byId("logout-button");
const databasesStatus = byId("databases-status");
const databasesError = byId("databases-error");
const databasesEmpty = byId("databases-empty");
const databasesTable = byId("databases-table");
const databasesBody = byId("databases-body");

function validID(id) {
    return typeof id === "string" && /^[1-9]\d*$/.test(id) && BigInt(id) <= 9223372036854775807n;
}

function textCell(row, value) {
    const cell = document.createElement("td");
    cell.textContent = String(value);
    row.appendChild(cell);
    return cell;
}

function actionLink(cell, id, resource, label) {
    const link = document.createElement("a");
    link.href = `/databases/${encodeURIComponent(id)}/${resource}`;
    link.textContent = label;
    cell.appendChild(link);
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
            validID(db.id) && ["view", "edit"].includes(db.access))) {
            throw new Error("Format daftar database atau izin dari backend tidak valid.");
        }
        for (const database of result.databases) {
            const row = document.createElement("tr");
            textCell(row, database.id);
            textCell(row, database.name);
            textCell(row, database.database_name);
            textCell(row, database.access === "edit" ? "View + Edit" : "View Only");
            const links = document.createElement("td");
            links.className = "database-workspace-links";
            actionLink(links, database.id, "viewer", "Lihat Viewer");
            actionLink(links, database.id, "history", "Lihat History");
            if (database.access === "edit") actionLink(links, database.id, "drafts", "Kelola Draft");
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
            window.location.replace("/login");
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
        role.textContent = user.is_platform_admin ? "Platform Admin" : "Pengguna";
        adminPanel.hidden = !user.is_platform_admin;
        content.hidden = false;
        status.hidden = true;
        logoutButton.disabled = false;
        await loadDatabases(user.is_platform_admin);
    } catch (error) {
        status.textContent = error.message || "Gagal memuat dashboard.";
        status.className = "error-message";
    }
}

logoutButton.addEventListener("click", async () => {
    logoutButton.disabled = true;
    status.hidden = false;
    status.className = "";
    status.textContent = "Sedang logout...";
    try {
        await logout();
        window.location.replace("/login");
    } catch (error) {
        status.className = "error-message";
        status.textContent = error.message || "Logout gagal. Silakan coba lagi.";
        logoutButton.disabled = false;
    }
});

loadDashboard();
