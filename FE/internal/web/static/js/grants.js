import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { positiveInt64 } from "./pagination.js";
import { byId, addCell, addOption, setBanner, navigateTo } from "./ui.js";

const status = byId("grants-status");
const errorBox = byId("grants-error");
const panel = byId("grants-panel");
const noGroups = byId("grants-no-groups");
const noGrants = byId("grants-empty");
const table = byId("grants-table");
const body = byId("grants-body");

const form = byId("grant-form");
const groupSelect = byId("grant-group");
const databaseSelect = byId("grant-database");
const databasesEmpty = byId("grants-no-databases");
const refreshButton = byId("grants-refresh");
const accessSelect = byId("grant-access");
const submitButton = byId("grant-submit");
const message = byId("grant-message");

let groups = [];
let databases = [];
let currentUserId = null;
let saving = false;

const showMessage = (text, isError = false) => setBanner(message, text, isError);
const groupContainsSelf = (group) => (group.user_ids ?? []).some((id) => String(id) === currentUserId);

function setSaving(value) {
    saving = value;
    const unavailable = groups.length === 0 || databases.length === 0;
    databaseSelect.disabled = value || unavailable;
    groupSelect.disabled = value || unavailable;
    accessSelect.disabled = value || unavailable;
    submitButton.disabled = value || unavailable;
    refreshButton.disabled = value;

    for (const button of body.querySelectorAll("button")) {
        button.disabled = value || button.dataset?.missingConnection === "true";
    }
    submitButton.textContent = value ? "Menyimpan..." : "Simpan Grant";
}

function connectionName(id) {
    const connection = databases.find((entry) => entry.id === id);
    return connection ? `${connection.name} (ID ${id})` : `Koneksi tidak tersedia (ID ${id})`;
}

function renderGrants() {
    const previousGroup = groupSelect.value;
    const previousDatabase = databaseSelect.value;
    groupSelect.replaceChildren();
    databaseSelect.replaceChildren();
    body.replaceChildren();

    addOption(groupSelect, "", "Pilih group...");
    addOption(databaseSelect, "", "Pilih database target...");

    for (const database of databases) {
        addOption(databaseSelect, database.id, `${database.name} — ${database.database_name} (ID ${database.id})`);
    }

    let grantCount = 0;
    for (const group of groups) {
        addOption(groupSelect, group.id, group.name);

        for (const grant of group.grants ?? []) {
            grantCount++;
            const id = grant.database_id;
            const exists = databases.some((database) => database.id === id);
            const row = document.createElement("tr");
            addCell(row, group.name);
            addCell(row, connectionName(id));

            const accessCell = document.createElement("td");
            const badge = document.createElement("span");
            badge.className = "access-badge access-badge--view";
            badge.textContent = "View Only";
            if (grant.access_level === "edit") {
                badge.className = "access-badge access-badge--edit";
                badge.textContent = "View + Edit";
            }
            accessCell.appendChild(badge);
            row.appendChild(accessCell);

            const actions = document.createElement("td");
            actions.className = "grant-actions";
            const edit = document.createElement("button");
            edit.type = "button";
            edit.className = "grant-action";
            edit.textContent = "Ubah";
            edit.disabled = !exists;
            edit.dataset.missingConnection = String(!exists);
            if (!exists) edit.title = "Koneksi tidak tersedia; grant ini hanya dapat dicabut.";
            edit.addEventListener("click", () => {
                if (saving || !exists) return;
                groupSelect.value = group.id;
                databaseSelect.value = id;
                accessSelect.value = grant.access_level;
                showMessage("");
                form.scrollIntoView({ behavior: "smooth", block: "start" });
                accessSelect.focus({ preventScroll: true });
            });

            const revoke = document.createElement("button");
            revoke.type = "button";
            revoke.className = "grant-action grant-revoke";
            revoke.textContent = "Cabut";
            revoke.setAttribute("aria-label", `Cabut grant database ${id} untuk ${group.name}`);
            revoke.addEventListener("click", () => revokeGrant(group, grant));
            actions.append(edit, revoke);
            row.appendChild(actions);
            body.appendChild(row);
        }
    }

    if (groups.some((group) => group.id === previousGroup)) groupSelect.value = previousGroup;
    if (databases.some((database) => database.id === previousDatabase)) databaseSelect.value = previousDatabase;
    panel.hidden = groups.length === 0;
    noGroups.hidden = groups.length !== 0;
    databasesEmpty.hidden = databases.length !== 0 || groups.length === 0;
    table.hidden = grantCount === 0;
    noGrants.hidden = grantCount !== 0 || groups.length === 0;
    setSaving(saving);
}

async function reloadData() {
    const [groupData, databaseData] = await Promise.all([
        authenticatedRequest("/api/v1/admin/groups"),
        authenticatedRequest("/api/v1/admin/databases")
    ]);
    if (!Array.isArray(groupData?.groups) || !Array.isArray(databaseData?.databases)) {
        throw new Error("Format daftar group atau koneksi dari backend tidak valid.");
    }
    if (!databaseData.databases.every((entry) =>
        positiveInt64(entry.id) && typeof entry.name === "string" && typeof entry.database_name === "string")) {
        throw new Error("Daftar koneksi memiliki ID atau nama yang tidak valid.");
    }
    groups = groupData.groups;
    databases = databaseData.databases;
    renderGrants();
}

async function reloadGroups() {
    const data = await authenticatedRequest("/api/v1/admin/groups");
    if (!Array.isArray(data?.groups)) throw new Error("Format daftar group dari backend tidak valid.");
    groups = data.groups;
    renderGrants();
}

function handleError(err) {
    if (err.status === 401) {
        navigateTo("/login", { replace: true });
        return;
    }
    if (err.status === 403 && isCSRFError(err)) {
        showMessage("Token CSRF tidak valid. Silakan coba lagi.", true);
    } else if (err.status === 403) {
        panel.hidden = true;
        showMessage("Akses ditolak atau izin admin telah dicabut. Muat ulang halaman.", true);
    } else if (err.status === 404) {
        showMessage("Group, koneksi, atau grant tidak ditemukan. Muat ulang daftar sebelum mencoba lagi.", true);
    } else {
        showMessage(err.message || "Operasi grant gagal.", true);
    }
}

form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (saving) return;

    const group = groups.find((g) => String(g.id) === groupSelect.value);
    const databaseId = databaseSelect.value;
    const level = accessSelect.value;

    if (!group || !positiveInt64(databaseId) ||
        !databases.some((database) => database.id === databaseId) ||
        !["view", "edit"].includes(level)) {
        showMessage("Pilih group, koneksi database yang terdaftar, dan tingkat akses.", true);
        return;
    }

    const existing = (group.grants ?? []).find((g) => String(g.database_id) === databaseId);
    if (existing?.access_level === level) {
        showMessage("Grant tersebut sudah memiliki tingkat akses yang sama.", true);
        return;
    }

    const downgrade = existing?.access_level === "edit" && level === "view";
    if (downgrade && !(await confirmAction({
        title: "Turunkan akses group?",
        message: "Penurunan izin akan mencabut session anggota group. Lanjutkan?",
        confirmLabel: "Turunkan Izin",
        tone: "danger"
    }))) {
        return;
    }

    if (saving) return;
    showMessage("");
    setSaving(true);

    try {
        await authenticatedRequest(
            `/api/v1/admin/groups/${encodeURIComponent(String(group.id))}/databases/${encodeURIComponent(databaseId)}`,
            {
                method: "PUT",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ access_level: level })
            }
        );
        if (downgrade && groupContainsSelf(group)) {
            navigateTo("/login", { replace: true });
            return;
        }
        try {
            await reloadGroups();
            showMessage("Grant berhasil disimpan.");
        } catch (refreshError) {
            if (refreshError.status === 401) {
                navigateTo("/login", { replace: true });
                return;
            }
            showMessage("Grant tersimpan, tetapi daftar terbaru gagal dimuat.", true);
        }
    } catch (err) {
        handleError(err);
    } finally {
        setSaving(false);
    }
});

async function revokeGrant(group, grant) {
    if (saving) return;
    const confirmed = await confirmAction({
        title: "Cabut akses database?",
        message: `Cabut akses ${connectionName(grant.database_id)} dari group ${group.name}? Session anggota group akan dicabut.`,
        confirmLabel: "Cabut Grant",
        tone: "danger"
    });
    if (!confirmed || saving) return;

    showMessage("");
    setSaving(true);

    try {
        await authenticatedRequest(
            `/api/v1/admin/groups/${encodeURIComponent(String(group.id))}/databases/${encodeURIComponent(String(grant.database_id))}`,
            { method: "DELETE" }
        );
        if (groupContainsSelf(group)) {
            navigateTo("/login", { replace: true });
            return;
        }
        try {
            await reloadGroups();
            showMessage("Grant berhasil dicabut.");
        } catch (refreshError) {
            if (refreshError.status === 401) {
                navigateTo("/login", { replace: true });
                return;
            }
            showMessage("Grant dicabut, tetapi daftar terbaru gagal dimuat.", true);
        }
    } catch (err) {
        handleError(err);
    } finally {
        setSaving(false);
    }
}

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;
        if (!user.is_platform_admin) {
            status.hidden = true;
            errorBox.textContent = "Akses ditolak. Halaman ini hanya untuk Platform Admin.";
            errorBox.hidden = false;
            return;
        }
        currentUserId = String(user.id);
        status.textContent = "Memuat group dan grant...";
        await reloadData();
        status.hidden = true;
    } catch (err) {
        status.hidden = true;
        if (err.status === 401) {
            navigateTo("/login", { replace: true });
            return;
        }
        errorBox.textContent = err.status === 403
            ? "Akses admin ditolak. Hanya Platform Admin yang boleh mengelola grant."
            : (err.message || "Gagal memuat group dan koneksi database.");
        errorBox.hidden = false;
    }
}

refreshButton.addEventListener("click", async () => {
    if (saving) return;
    refreshButton.disabled = true;
    showMessage("");
    status.hidden = false;
    status.textContent = "Memuat ulang group dan koneksi...";
    try {
        await reloadData();
        errorBox.hidden = true;
    } catch (error) {
        if (error.status === 401) navigateTo("/login", { replace: true });
        else {
            if (error.status === 403) panel.hidden = true;
            errorBox.textContent = error.status === 403
                ? "Akses admin sudah tidak tersedia."
                : (error.message || "Gagal memuat ulang data.");
            errorBox.hidden = false;
        }
    } finally {
        status.hidden = true;
        refreshButton.disabled = false;
    }
});

initialize();
