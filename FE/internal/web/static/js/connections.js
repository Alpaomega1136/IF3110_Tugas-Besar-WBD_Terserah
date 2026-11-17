import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { byId, addCell, formatDate, setBanner, utf8Bytes } from "./ui.js";

const status = byId("connections-status");
const pageError = byId("connections-error");
const forbidden = byId("connections-forbidden");
const content = byId("connections-content");
const empty = byId("connections-empty");
const table = byId("connections-table");
const body = byId("connections-body");
const refreshButton = byId("connections-refresh");
const actionMessage = byId("connection-action-message");
const createForm = byId("connection-create-form");
const createError = byId("connection-create-error");
const editSection = byId("connection-edit-section");
const editForm = byId("connection-edit-form");
const editError = byId("connection-edit-error");
const editCancel = byId("connection-edit-cancel");

const modes = new Set(["disable", "require", "verify-ca", "verify-full"]);

let entries = [];
let selectedID = null;
let busy = false;

function validID(id) {
    return typeof id === "string" && /^[1-9]\d*$/.test(id);
}

function pathFor(id) {
    if (!validID(id)) throw new Error("ID koneksi tidak valid.");
    return `/api/v1/admin/databases/${encodeURIComponent(id)}`;
}

function setMessage(target, message, error = false) {
    setBanner(target, message, error, "connection-message");
    if (target === actionMessage) target.setAttribute("role", error ? "alert" : "status");
}

function setBusy(value) {
    busy = value;
    for (const control of [...createForm.elements, ...editForm.elements, refreshButton, editCancel, ...body.querySelectorAll("button")]) {
        control.disabled = value;
    }
}

function errorText(error, operation = "Permintaan") {
    if (isCSRFError(error)) return "Validasi CSRF ditolak. Muat ulang halaman dan pastikan APP_ORIGIN sesuai alamat frontend.";
    if (error.status === 403) return "Akses ditolak. Hanya Platform Admin yang dapat mengelola koneksi.";
    if (error.code === "metadata_target_forbidden") return "Database metadata platform tidak boleh digunakan sebagai target. Hapus koneksi ini dan pilih database target lain.";
    if (error.status === 409) return operation === "Hapus"
        ? "Koneksi belum bisa dihapus karena masih memiliki grant atau riwayat yang bergantung padanya."
        : "Konflik koneksi: target sudah terdaftar atau identitas target tidak boleh diubah.";
    if (error.code === "target_unavailable") return "Target PostgreSQL tidak dapat dihubungi. Periksa host, port, kredensial, SSL, dan keadaan server lalu tes ulang.";
    if (error.status === 503) return "Layanan database sedang tidak tersedia. Periksa backend lalu coba lagi.";
    if (error.status === 404) return "Koneksi tidak ditemukan. Muat ulang daftar koneksi.";
    if (error.status === 400) return "Konfigurasi koneksi ditolak oleh backend. Periksa field yang diisi.";
    return error.message || `${operation} gagal. Silakan coba lagi.`;
}

function handleError(error, target, operation) {
    if (error.status === 401) {
        window.location.replace("/login");
        return;
    }
    if (error.status === 403 && !isCSRFError(error)) {
        content.hidden = true;
        forbidden.hidden = false;
    }
    setMessage(target, errorText(error, operation), true);
}

function renderList() {
    body.replaceChildren();
    for (const item of entries) {
        const row = document.createElement("tr");
        addCell(row, item.id);
        addCell(row, item.name);
        addCell(row, `${item.host}:${item.port}`);
        addCell(row, item.database_name);
        addCell(row, item.username);
        addCell(row, item.sslmode);
        addCell(row, formatDate(item.created_at));

        const actions = addCell(row, "");
        actions.className = "connection-actions";

        const editButton = document.createElement("button");
        editButton.type = "button";
        editButton.textContent = "Edit";
        editButton.className = "connection-action";
        editButton.disabled = busy;
        editButton.addEventListener("click", () => openEdit(item.id));

        const testButton = document.createElement("button");
        testButton.type = "button";
        testButton.textContent = "Tes";
        testButton.className = "connection-action button-secondary";
        testButton.disabled = busy;
        testButton.addEventListener("click", () => testConnection(item.id));

        const deleteButton = document.createElement("button");
        deleteButton.type = "button";
        deleteButton.textContent = "Hapus";
        deleteButton.className = "connection-action connection-danger";
        deleteButton.disabled = busy;
        deleteButton.addEventListener("click", () => deleteConnection(item.id));

        actions.replaceChildren(editButton, testButton, deleteButton);
        body.appendChild(row);
    }
    empty.hidden = entries.length > 0;
    table.hidden = entries.length === 0;
}

async function refreshConnections() {
    if (busy) return false;
    status.hidden = false;
    status.textContent = "Memuat daftar koneksi...";
    setMessage(pageError, "");
    try {
        const result = await authenticatedRequest("/api/v1/admin/databases");
        if (!Array.isArray(result?.databases) || result.databases.some((item) => !validID(item?.id))) {
            throw new Error("Format daftar koneksi dari backend tidak valid.");
        }
        entries = result.databases;
        renderList();
        if (selectedID !== null) {
            const selected = entries.find((item) => item.id === selectedID);
            if (selected) openEdit(selectedID);
            else closeEdit();
        }
        return true;
    } catch (error) {
        empty.hidden = true;
        table.hidden = true;
        handleError(error, pageError, "Memuat daftar koneksi");
        return false;
    } finally {
        status.hidden = true;
    }
}

function readForm(form, editing) {
    const value = (name) => form.elements.namedItem(name).value;
    const name = value("name").trim();
    const username = value("username");
    const password = value("password");
    const sslmode = value("sslmode");
    const payload = { name, username, sslmode };

    if (!name || utf8Bytes(name) > 100) {
        throw new Error("Isi nama koneksi dengan maksimal 100 karakter biasa; karakter tertentu dapat dihitung lebih dari satu.");
    }
    if (!username.trim() || utf8Bytes(username) > 63 || username.includes("\0")) {
        throw new Error("Isi username PostgreSQL dengan maksimal 63 karakter biasa; karakter tertentu dapat dihitung lebih dari satu.");
    }
    if (!modes.has(sslmode)) throw new Error("SSL mode tidak valid.");
    if (!editing || password) {
        if (!password || utf8Bytes(password) > 4096 || password.includes("\0")) {
            throw new Error("Isi password dengan maksimal 4096 karakter biasa tanpa karakter NUL; karakter tertentu dapat dihitung lebih dari satu.");
        }
        payload.password = password;
    }
    if (!editing) {
        const host = value("host").trim();
        const databaseName = value("database_name");
        const portValue = value("port");
        if (!host || host.length > 253 || /[\s/?#@\[\]]/.test(host)) throw new Error("Host PostgreSQL tidak valid.");
        if (!/^[0-9]+$/.test(portValue) || Number(portValue) < 1 || Number(portValue) > 65535) {
            throw new Error("Port harus bilangan bulat antara 1 dan 65535.");
        }
        if (!databaseName || utf8Bytes(databaseName) > 63 || databaseName.includes("\0")) {
            throw new Error("Isi nama database target dengan maksimal 63 karakter biasa; karakter tertentu dapat dihitung lebih dari satu.");
        }
        Object.assign(payload, { host, port: Number(portValue), database_name: databaseName });
    }
    return payload;
}

function openEdit(id) {
    if (busy) return;
    const item = entries.find((entry) => entry.id === id);
    if (!item) return;
    selectedID = id;
    byId("connection-edit-name").textContent = item.name;
    byId("connection-edit-host").textContent = item.host;
    byId("connection-edit-port").textContent = String(item.port);
    byId("connection-edit-database").textContent = item.database_name;
    editForm.elements.namedItem("name").value = item.name;
    editForm.elements.namedItem("username").value = item.username;
    editForm.elements.namedItem("password").value = "";
    editForm.elements.namedItem("sslmode").value = item.sslmode;
    setMessage(editError, "");
    editSection.hidden = false;
    editForm.elements.namedItem("name").focus();
}

function closeEdit() {
    selectedID = null;
    editForm.reset();
    editSection.hidden = true;
    setMessage(editError, "");
}

createForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (busy) return;
    setMessage(createError, "");
    let payload;
    try {
        payload = readForm(createForm, false);
    } catch (error) {
        setMessage(createError, error.message, true);
        return;
    }
    setBusy(true);
    try {
        const created = await authenticatedRequest("/api/v1/admin/databases", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        });
        createForm.reset();
        if (!validID(created?.id)) throw new Error("Koneksi dibuat, tetapi respons ID tidak valid. Muat ulang daftar.");
        setMessage(actionMessage, `Koneksi ${created.name} berhasil dibuat (ID ${created.id}). Gunakan tombol Tes untuk memeriksa target sebelum memberi grant.`);
    } catch (error) {
        handleError(error, createError, "Membuat koneksi");
        return;
    } finally {
        createForm.elements.namedItem("password").value = "";
        setBusy(false);
    }
    await refreshConnections();
});

editForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (busy || selectedID === null) return;
    setMessage(editError, "");
    let payload;
    try {
        payload = readForm(editForm, true);
    } catch (error) {
        setMessage(editError, error.message, true);
        return;
    }
    const id = selectedID;
    setBusy(true);
    try {
        const updated = await authenticatedRequest(pathFor(id), {
            method: "PATCH",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        });
        if (!validID(updated?.id) || updated.id !== id) throw new Error("Respons pembaruan tidak valid. Muat ulang daftar.");
        closeEdit();
        setMessage(actionMessage, `Koneksi ${updated.name} berhasil diperbarui.`);
    } catch (error) {
        handleError(error, editError, "Memperbarui koneksi");
        return;
    } finally {
        editForm.elements.namedItem("password").value = "";
        setBusy(false);
    }
    await refreshConnections();
});

async function testConnection(id) {
    if (busy) return;
    const item = entries.find((entry) => entry.id === id);
    if (!item) return;
    setBusy(true);
    setMessage(actionMessage, `Menguji koneksi ${item.name}...`);
    try {
        const result = await authenticatedRequest(`${pathFor(id)}/test`, { method: "POST" });
        if (result?.status !== "ok") throw new Error("Hasil tes koneksi dari backend tidak valid.");
        setMessage(actionMessage, `Koneksi ${item.name} berhasil diuji. Target PostgreSQL dapat diakses.`);
    } catch (error) {
        handleError(error, actionMessage, "Tes koneksi");
    } finally {
        setBusy(false);
    }
}

async function deleteConnection(id) {
    if (busy) return;
    const item = entries.find((entry) => entry.id === id);
    if (!item || !(await confirmAction({
        title: "Hapus koneksi database?",
        message: `Hapus koneksi "${item.name}" (ID ${id})? Tindakan ini tidak dapat dibatalkan.`,
        confirmLabel: "Hapus Koneksi",
        tone: "danger"
    }))) return;
    if (busy) return;
    setBusy(true);
    setMessage(actionMessage, `Menghapus koneksi ${item.name}...`);
    try {
        await authenticatedRequest(pathFor(id), { method: "DELETE" });
        if (selectedID === id) closeEdit();
        setMessage(actionMessage, `Koneksi ${item.name} berhasil dihapus.`);
    } catch (error) {
        handleError(error, actionMessage, "Hapus");
        return;
    } finally {
        setBusy(false);
    }
    await refreshConnections();
}

refreshButton.addEventListener("click", () => {
    setMessage(actionMessage, "");
    refreshConnections();
});
editCancel.addEventListener("click", closeEdit);

async function initialize() {
    try {
        const principal = await requireAuthenticatedUser();
        if (!principal) return;
        if (!principal.is_platform_admin) {
            status.hidden = true;
            forbidden.hidden = false;
            return;
        }
        content.hidden = false;
        await refreshConnections();
    } catch (error) {
        status.hidden = true;
        handleError(error, pageError, "Memeriksa akses");
    }
}

initialize();
