import {
    authenticatedRequest,
    requireAuthenticatedUser,
    logout
} from "./auth.js";

const status = document.getElementById("dashboard-status");
const content = document.getElementById("dashboard-content");
const username = document.getElementById("welcome-username");
const role = document.getElementById("account-role");
const adminPanel = document.getElementById("admin-panel");
const logoutButton = document.getElementById("logout-button");

const databasesStatus = document.getElementById("databases-status");
const databasesError = document.getElementById("databases-error");
const databasesEmpty = document.getElementById("databases-empty");
const databasesTable = document.getElementById("databases-table");
const databasesBody = document.getElementById("databases-body");


function addDatabaseCell(row, value) {
    const cell = document.createElement("td");
    cell.textContent = String(value);
    row.appendChild(cell);
}

async function loadDatabases() {
    databasesStatus.hidden = false;
    databasesError.hidden = true;
    databasesEmpty.hidden = true;
    databasesTable.hidden = true;
    databasesStatus.textContent = "Memuat database...";

    try {
        const result = await authenticatedRequest("/api/v1/databases");

        if (!Array.isArray(result?.databases)) {
            throw new Error("Format daftar database dari backend tidak valid.");
        }

        databasesBody.replaceChildren();

        for (const database of result.databases) {
            const row = document.createElement("tr");

            addDatabaseCell(row, database.id);
            addDatabaseCell(row, database.name);
            addDatabaseCell(row, database.database_name);

            addDatabaseCell(
                row,
                database.effective_access === "edit"
                    ? "View + Edit"
                    : database.effective_access === "view"
                        ? "View Only"
                        : "Tidak diketahui"
            );
 
            const actionCell = document.createElement("td");

            if (database.effective_access === "edit") {
                const link = document.createElement("a");
                link.href = `/databases/${encodeURIComponent(String(database.id))}/drafts`;
                link.textContent = "Kelola Draft";
                actionCell.appendChild(link);
            } else {
                actionCell.textContent = "Memerlukan akses Edit";
            }

            row.appendChild(actionCell);

            databasesBody.appendChild(row);
        }

        databasesStatus.hidden = true;

        databasesEmpty.hidden = result.databases.length !== 0;

        databasesTable.hidden =  result.databases.length === 0;

    } catch (error) {
        databasesStatus.hidden = true;

        if (error.status === 401) {
            window.location.replace("/login");
            return;
        }

        databasesError.textContent = error.status === 403
            ? "Akses daftar database ditolak."
            : (error.message || "Gagal memuat daftar database.");

        databasesError.hidden = false;
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
        await loadDatabases();
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
