import { authenticatedRequest, requireAuthenticatedUser } from "./auth.js";
import { positiveInt64 } from "./pagination.js";
import { byId, navigateTo } from "./ui.js";

const databaseId = byId("viewer-page").dataset.databaseId;
const endpoint = `/api/v1/databases/${encodeURIComponent(databaseId)}`;
const workspace = byId("viewer-workspace");
const state = { schema: null, table: null, schemas: [], tables: [], offset: 0, limit: 50 };
const controller = new AbortController();
const requestOptions = { signal: controller.signal };

let schemaRun = 0;
let tableRun = 0;
let rowsRun = 0;
let schemaLoadRun = 0;

function validName(value) {
    return typeof value === "string" && value.length > 0 && value.length <= 63 && !value.includes("\0");
}

function systemSchema(name) {
    return name.startsWith("pg_") || name === "information_schema" || name === "_terserah";
}

function showMessage(id, message) {
    const node = byId(id);
    node.textContent = message;
    node.hidden = false;
}

function hideMessage(id) {
    const node = byId(id);
    node.textContent = "";
    node.hidden = true;
}

function errorText(error, fallback) {
    if (error.status === 404) return "Schema, tabel, atau database sudah tidak ditemukan. Muat ulang daftar dan pilih kembali.";
    if (error.status === 413) return "Hasil data melebihi batas 4 MiB. Kurangi jumlah baris per halaman, lalu muat ulang.";
    if (error.status === 503) return "Target PostgreSQL tidak tersedia. Periksa koneksi target, lalu coba muat ulang.";
    if (error.status === 409) return "Database ini tidak dapat diakses sebagai target. Hubungi Platform Admin.";
    return error.message || fallback;
}

function invalidateRequests() {
    schemaLoadRun++;
    schemaRun++;
    tableRun++;
    rowsRun++;
}

window.__spaCleanup = () => {
    invalidateRequests();
    controller.abort();
};

function revokeAccess(message) {
    invalidateRequests();
    workspace.hidden = true;
    byId("viewer-rows-body").replaceChildren();
    byId("viewer-rows-head").replaceChildren();
    for (const id of ["viewer-columns-body", "viewer-constraints-body", "viewer-indexes-body", "viewer-tables-list", "viewer-schemas-list"]) {
        byId(id).replaceChildren();
    }
    showMessage("viewer-error", message);
}

function handleError(error, target, fallback) {
    if (error.status === 401) {
        revokeAccess("Session telah berakhir. Silakan login kembali.");
        navigateTo("/login", { replace: true });
        return;
    }
    if (error.status === 403) {
        revokeAccess("Hak akses database ditolak atau telah dicabut. Kembali ke Dashboard untuk memeriksa grant.");
        return;
    }
    showMessage(target, errorText(error, fallback));
}

function clearRows() {
    rowsRun++;
    byId("viewer-rows-head").replaceChildren();
    byId("viewer-rows-body").replaceChildren();
    byId("viewer-rows-table").hidden = true;
    byId("viewer-rows-empty").hidden = true;
    byId("viewer-rows-summary").textContent = "";
    byId("viewer-rows-prev").disabled = true;
    byId("viewer-rows-next").disabled = true;
    hideMessage("viewer-rows-error");
    byId("viewer-rows-status").hidden = true;
}

function clearTable() {
    tableRun++;
    state.table = null;
    state.offset = 0;
    byId("viewer-detail").hidden = true;
    byId("viewer-rows-panel").hidden = true;
    byId("viewer-metadata").hidden = true;
    for (const type of ["columns", "constraints", "indexes"]) {
        byId(`viewer-${type}-body`).replaceChildren();
        byId(`viewer-${type}-table`).hidden = true;
        byId(`viewer-${type}-empty`).hidden = true;
    }
    hideMessage("viewer-detail-error");
    byId("viewer-detail-status").hidden = true;
    clearRows();
}

function clearSchema() {
    schemaRun++;
    state.schema = null;
    state.tables = [];
    byId("viewer-tables-panel").hidden = true;
    byId("viewer-tables-list").replaceChildren();
    byId("viewer-tables-empty").hidden = true;
    byId("viewer-tables-status").hidden = true;
    hideMessage("viewer-tables-error");
    clearTable();
}

function objectItem(name, description, selected, onSelect) {
    const item = document.createElement("li");
    const button = document.createElement("button");
    button.type = "button";
    button.className = selected ? "viewer-object-button viewer-selected" : "viewer-object-button";
    button.textContent = name;
    button.setAttribute("aria-pressed", String(selected));
    button.addEventListener("click", onSelect);
    item.appendChild(button);
    if (description) {
        const extra = document.createElement("span");
        extra.className = "viewer-object-description";
        extra.textContent = description;
        item.appendChild(extra);
    }
    return item;
}

function renderSchemas() {
    const list = byId("viewer-schemas-list");
    list.replaceChildren();
    for (const name of state.schemas) {
        list.appendChild(objectItem(name, "", name === state.schema, () => selectSchema(name)));
    }
    byId("viewer-schemas-empty").hidden = state.schemas.length !== 0;
}

function renderTables() {
    const list = byId("viewer-tables-list");
    list.replaceChildren();
    for (const table of state.tables) {
        const kind = table.kind === "partitioned_table" ? "Partitioned table" : "Table";
        list.appendChild(objectItem(table.name, kind, table.name === state.table, () => selectTable(table.name)));
    }
    byId("viewer-tables-empty").hidden = state.tables.length !== 0;
}

async function loadSchemas(initial = false) {
    const request = ++schemaLoadRun;
    byId("viewer-refresh-schemas").disabled = true;
    byId("viewer-schemas-status").hidden = false;
    hideMessage("viewer-schemas-error");
    clearSchema();
    byId("viewer-schemas-list").replaceChildren();
    byId("viewer-schemas-empty").hidden = true;
    try {
        const result = await authenticatedRequest(`${endpoint}/schemas`, requestOptions);
        if (request !== schemaLoadRun) return;
        if (!Array.isArray(result?.schemas) || !result.schemas.every(validName)) {
            throw new Error("Format daftar schema tidak valid.");
        }
        state.schemas = result.schemas.filter((name) => !systemSchema(name));
        renderSchemas();
        workspace.hidden = false;
    } catch (error) {
        if (request !== schemaLoadRun) return;
        handleError(error, initial ? "viewer-error" : "viewer-schemas-error", "Gagal memuat schema.");
        if (initial) workspace.hidden = true;
    } finally {
        if (request === schemaLoadRun) {
            byId("viewer-schemas-status").hidden = true;
            byId("viewer-refresh-schemas").disabled = false;
        }
    }
}

async function selectSchema(name) {
    if (!state.schemas.includes(name)) return;
    clearSchema();
    const request = schemaRun;
    state.schema = name;
    byId("viewer-selected-schema").textContent = name;
    byId("viewer-tables-panel").hidden = false;
    byId("viewer-tables-status").hidden = false;
    byId("viewer-refresh-tables").disabled = true;
    renderSchemas();
    try {
        const result = await authenticatedRequest(`${endpoint}/schemas/${encodeURIComponent(name)}/tables`, requestOptions);
        if (request !== schemaRun) return;
        if (!Array.isArray(result?.tables) || !result.tables.every((entry) => validName(entry?.name) && ["table", "partitioned_table"].includes(entry.kind))) {
            throw new Error("Format daftar tabel tidak valid.");
        }
        state.tables = result.tables;
        renderTables();
    } catch (error) {
        if (request !== schemaRun) return;
        handleError(error, "viewer-tables-error", "Gagal memuat tabel.");
    } finally {
        if (request === schemaRun) {
            byId("viewer-tables-status").hidden = true;
            byId("viewer-refresh-tables").disabled = false;
        }
    }
}

function textCell(row, value) {
    const cell = document.createElement("td");
    const span = document.createElement("span");
    span.className = "viewer-cell-value";
    if (value === null || value === undefined) {
        span.className += " viewer-null";
        span.textContent = "NULL";
        span.setAttribute("aria-label", "Nilai NULL");
    } else {
        span.textContent = String(value);
        if (value === "") {
            span.className += " viewer-empty-string";
            span.textContent = "(string kosong)";
        }
    }
    cell.appendChild(span);
    row.appendChild(cell);
}

function renderMetadata(result) {
    const types = [
        { name: "columns", values: result.columns, fields: (c) => [c.position, c.name, c.data_type, c.nullable ? "Ya" : "Tidak", c.default, c.identity || "—", c.generated || "—"] },
        { name: "constraints", values: result.constraints, fields: (c) => [c.name, c.type, c.definition] },
        { name: "indexes", values: result.indexes, fields: (c) => [c.name, c.unique ? "Ya" : "Tidak", c.primary ? "Ya" : "Tidak", c.definition] }
    ];
    for (const type of types) {
        const body = byId(`viewer-${type.name}-body`);
        body.replaceChildren();
        for (const item of type.values) {
            const row = document.createElement("tr");
            for (const value of type.fields(item)) textCell(row, value);
            body.appendChild(row);
        }
        byId(`viewer-${type.name}-table`).hidden = type.values.length === 0;
        byId(`viewer-${type.name}-empty`).hidden = type.values.length !== 0;
    }
    byId("viewer-metadata").hidden = false;
}

function validMetadata(result) {
    return ["columns", "constraints", "indexes"].every((field) => Array.isArray(result?.[field]) &&
        result[field].every((item) => item && typeof item === "object"));
}

async function loadMetadata() {
    if (!state.schema || !state.table) return;
    const request = ++tableRun;
    const schema = state.schema;
    const table = state.table;
    byId("viewer-refresh-detail").disabled = true;
    byId("viewer-detail-status").hidden = false;
    byId("viewer-metadata").hidden = true;
    hideMessage("viewer-detail-error");
    clearRows();
    const path = `${endpoint}/schemas/${encodeURIComponent(schema)}/tables/${encodeURIComponent(table)}`;
    try {
        const result = await authenticatedRequest(path, requestOptions);
        if (request !== tableRun) return;
        if (!validMetadata(result)) throw new Error("Format metadata tabel tidak valid.");
        renderMetadata(result);
        await loadRows();
    } catch (error) {
        if (request !== tableRun) return;
        handleError(error, "viewer-detail-error", "Gagal memuat metadata tabel.");
    } finally {
        if (request === tableRun) {
            byId("viewer-detail-status").hidden = true;
            byId("viewer-refresh-detail").disabled = false;
        }
    }
}

async function selectTable(name) {
    if (!state.tables.some((entry) => entry.name === name)) return;
    clearTable();
    state.table = name;
    byId("viewer-selected-table").textContent = `${state.schema}.${name}`;
    byId("viewer-detail").hidden = false;
    byId("viewer-rows-panel").hidden = false;
    renderTables();
    await loadMetadata();
}

function validRows(result) {
    return Array.isArray(result?.columns) && result.columns.every((col) => col && typeof col.name === "string") &&
        Array.isArray(result.rows) && result.rows.every((row) => Array.isArray(row) && row.length === result.columns.length &&
            row.every((value) => value === null || typeof value === "string")) &&
        Number.isSafeInteger(result.offset) && result.offset === state.offset &&
        Number.isSafeInteger(result.limit) && result.limit === state.limit &&
        typeof result.has_more === "boolean" && ["primary_key", "physical"].includes(result.ordering);
}

function renderRows(result) {
    const head = byId("viewer-rows-head");
    const body = byId("viewer-rows-body");
    head.replaceChildren();
    body.replaceChildren();
    for (const column of result.columns) {
        const cell = document.createElement("th");
        cell.setAttribute("scope", "col");
        cell.textContent = column.name;
        head.appendChild(cell);
    }
    for (const values of result.rows) {
        const row = document.createElement("tr");
        for (const value of values) textCell(row, value);
        body.appendChild(row);
    }
    byId("viewer-rows-table").hidden = result.rows.length === 0;
    byId("viewer-rows-empty").hidden = result.rows.length !== 0;
    const first = result.rows.length ? result.offset + 1 : 0;
    const last = result.offset + result.rows.length;
    const ordering = result.ordering === "primary_key" ? "Primary Key" : "Fisik (tableoid, ctid)";
    byId("viewer-rows-summary").textContent = `Baris ${first}–${last} · Urutan: ${ordering}.`;
    byId("viewer-rows-prev").disabled = result.offset === 0;
    byId("viewer-rows-next").disabled = !result.has_more || result.offset > Number.MAX_SAFE_INTEGER - state.limit;
}

async function loadRows() {
    if (!state.schema || !state.table) return;
    const schema = state.schema;
    const table = state.table;
    const offset = state.offset;
    const limit = state.limit;
    clearRows();
    const active = rowsRun;
    byId("viewer-refresh-rows").disabled = true;
    byId("viewer-row-limit").disabled = true;
    byId("viewer-rows-status").hidden = false;
    const path = `${endpoint}/schemas/${encodeURIComponent(schema)}/tables/${encodeURIComponent(table)}/rows`;
    try {
        const result = await authenticatedRequest(`${path}?limit=${limit}&offset=${offset}`, requestOptions);
        if (active !== rowsRun) return;
        if (!validRows(result)) throw new Error("Format data baris tidak valid.");
        renderRows(result);
    } catch (error) {
        if (active !== rowsRun) return;
        handleError(error, "viewer-rows-error", "Gagal memuat data baris.");
    } finally {
        if (active === rowsRun) {
            byId("viewer-rows-status").hidden = true;
            byId("viewer-refresh-rows").disabled = false;
            byId("viewer-row-limit").disabled = false;
        }
    }
}

async function initialize() {
    workspace.hidden = true;
    try {
        const user = await requireAuthenticatedUser();
        if (!user || controller.signal.aborted) return;
        if (!positiveInt64(databaseId)) throw new Error("ID database tidak valid.");
        const data = await authenticatedRequest("/api/v1/databases", requestOptions);
        if (controller.signal.aborted) return;
        if (!Array.isArray(data?.databases)) throw new Error("Format database tidak valid.");
        const database = data.databases.find((entry) => entry.id === databaseId);
        if (!database || !["view", "edit"].includes(database.access)) {
            throw new Error("Kamu tidak memiliki grant untuk membuka database ini.");
        }
        byId("viewer-database-name").textContent = database.name;
        byId("viewer-target-name").textContent = database.database_name;
        byId("viewer-access").textContent = database.access === "edit" ? "View + Edit" : "View Only";
        await loadSchemas(true);
    } catch (error) {
        if (!controller.signal.aborted) handleError(error, "viewer-error", "Gagal membuka database viewer.");
    } finally {
        if (!controller.signal.aborted) byId("viewer-status").hidden = true;
    }
}

byId("viewer-refresh-schemas").addEventListener("click", () => loadSchemas());
byId("viewer-refresh-tables").addEventListener("click", () => selectSchema(state.schema));
byId("viewer-refresh-detail").addEventListener("click", () => loadMetadata());
byId("viewer-refresh-rows").addEventListener("click", () => loadRows());
byId("viewer-row-limit").addEventListener("change", () => {
    const next = Number(byId("viewer-row-limit").value);
    if (![10, 25, 50, 100, 200].includes(next)) return;
    state.limit = next;
    state.offset = 0;
    loadRows();
});
byId("viewer-rows-prev").addEventListener("click", () => {
    if (state.offset === 0) return;
    state.offset = Math.max(0, state.offset - state.limit);
    loadRows();
});
byId("viewer-rows-next").addEventListener("click", () => {
    if (byId("viewer-rows-next").disabled || state.offset > Number.MAX_SAFE_INTEGER - state.limit) return;
    state.offset += state.limit;
    loadRows();
});

initialize();
