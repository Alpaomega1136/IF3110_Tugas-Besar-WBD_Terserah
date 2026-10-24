import { authenticatedRequest, requireAuthenticatedUser } from "./auth.js";

const byId = (id) => document.getElementById(id);
const page = byId("viewer-page");
const databaseId = page.dataset.databaseId;
const status = byId("viewer-status");
const errorBox = byId("viewer-error");
const workspace = byId("viewer-workspace");
const schemasStatus = byId("viewer-schemas-status");

function validID(id) {
    return typeof id === "string" && /^[1-9]\d*$/.test(id) && BigInt(id) <= 9223372036854775807n;
}

function handleError(error) {
    if (error.status === 401) {
        window.location.replace("/login");
        return;
    }
    workspace.hidden = true;
    errorBox.textContent = error.status === 403
        ? "Hak akses database telah ditolak atau dicabut. Kembali ke Dashboard."
        : error.status === 404
            ? "Database atau schema tidak ditemukan. Kembali ke Dashboard."
            : error.status === 503
                ? "Target PostgreSQL sedang tidak tersedia. Coba lagi setelah koneksi pulih."
                : (error.message || "Gagal membuka database viewer.");
    errorBox.hidden = false;
}

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;
        if (!validID(databaseId)) throw new Error("ID database tidak valid.");
        const data = await authenticatedRequest("/api/v1/databases");
        if (!Array.isArray(data?.databases)) throw new Error("Format database tidak valid.");
        const database = data.databases.find((entry) => entry.id === databaseId);
        if (!database || !["view", "edit"].includes(database.access)) {
            throw new Error("Kamu tidak memiliki grant untuk membuka database ini.");
        }
        byId("viewer-database-name").textContent = database.name;
        byId("viewer-target-name").textContent = database.database_name;
        byId("viewer-access").textContent = database.access === "edit" ? "View + Edit" : "View Only";
        byId("viewer-drafts-link").hidden = database.access !== "edit";
        schemasStatus.hidden = false;
        const schemas = await authenticatedRequest(`/api/v1/databases/${encodeURIComponent(databaseId)}/schemas`);
        if (!Array.isArray(schemas?.schemas)) throw new Error("Format schema tidak valid.");
        const list = byId("viewer-schemas-list");
        list.replaceChildren();
        for (const name of schemas.schemas) {
            const item = document.createElement("li");
            item.textContent = String(name);
            list.appendChild(item);
        }
        byId("viewer-schemas-empty").hidden = schemas.schemas.length !== 0;
        workspace.hidden = false;
    } catch (error) {
        handleError(error);
    } finally {
        status.hidden = true;
        schemasStatus.hidden = true;
    }
}

initialize();
