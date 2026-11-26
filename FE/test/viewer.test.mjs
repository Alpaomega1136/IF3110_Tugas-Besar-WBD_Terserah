import test from "node:test";
import assert from "node:assert/strict";
import { Element, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";

const id = "9007199254740993";
const schema = "data / %2026";
const table = 'orders / "draft"';
const path = `/api/v1/databases/${id}/schemas/${encodeURIComponent(schema)}/tables/${encodeURIComponent(table)}`;
const metadata = {
    columns: [{ name: "id", position: 1, data_type: "integer", nullable: false, default: null, identity: "", generated: "" },
        { name: "message", position: 2, data_type: "text", nullable: true, default: "'safe'::text", identity: "", generated: "" }],
    constraints: [{ name: "orders_pkey", type: "p", definition: "PRIMARY KEY (id)" }],
    indexes: [{ name: "orders_pkey", unique: true, primary: true, definition: "CREATE UNIQUE INDEX ..." }]
};
const dataRows = {
    columns: [{ name: "id", type_oid: 23 }, { name: "message", type_oid: 25 }],
    rows: [["1", '<img src=x onerror="alert(1)">'], ["2", null], ["3", ""]],
    limit: 50, offset: 0, has_more: true, ordering: "primary_key"
};
let run = 0;

function reply(data, status = 200) {
    return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

function setupDOM() {
    const ids = [
        "viewer-page", "viewer-status", "viewer-error", "viewer-workspace", "viewer-database-name",
        "viewer-target-name", "viewer-access", "viewer-schemas-status",
        "viewer-schemas-error", "viewer-schemas-empty", "viewer-schemas-list", "viewer-refresh-schemas",
        "viewer-tables-panel", "viewer-selected-schema", "viewer-refresh-tables", "viewer-tables-status",
        "viewer-tables-error", "viewer-tables-empty", "viewer-tables-list", "viewer-detail",
        "viewer-selected-table", "viewer-refresh-detail", "viewer-detail-status", "viewer-detail-error", "viewer-metadata",
        "viewer-rows-panel", "viewer-refresh-rows", "viewer-row-limit", "viewer-rows-status",
        "viewer-rows-error", "viewer-rows-head", "viewer-rows-body", "viewer-rows-table",
        "viewer-rows-empty", "viewer-rows-summary", "viewer-rows-prev", "viewer-rows-next",
        "viewer-columns-body", "viewer-constraints-body", "viewer-indexes-body",
        "viewer-columns-table", "viewer-constraints-table", "viewer-indexes-table",
        "viewer-columns-empty", "viewer-constraints-empty", "viewer-indexes-empty"
    ];
    const elements = Object.fromEntries(ids.map((name) => [name, new Element()]));
    elements["viewer-page"].dataset.databaseId = id;
    elements["viewer-row-limit"].value = "50";
    const redirects = [];
    globalThis.document = { getElementById: (name) => elements[name] ?? null, createElement: (tag) => new Element(tag) };
    globalThis.window = { location: { replace: (url) => redirects.push(url) } };
    return { $: (name) => elements[name], redirects };
}

async function setup({ access = "view", available = true, custom } = {}) {
    clearAuthState();
    const ui = setupDOM();
    const calls = [];
    globalThis.fetch = async (url, options = {}) => {
        calls.push({ url, method: options.method ?? "GET", credentials: options.credentials, signal: options.signal });
        if (custom) {
            const result = custom(url);
            if (result !== undefined) return result;
        }
        if (url === "/api/v1/auth/me") return reply({ id: "7", username: "test", is_platform_admin: true });
        if (url === "/api/v1/databases") return reply({ databases: available ? [{ id, name: "Example", database_name: "testing", access }] : [] });
        if (url === `/api/v1/databases/${id}/schemas`) return reply({ schemas: [schema] });
        if (url === `/api/v1/databases/${id}/schemas/${encodeURIComponent(schema)}/tables`) {
            return reply({ tables: [{ name: table, kind: "table" }] });
        }
        if (url === path) return reply(metadata);
        if (url.startsWith(`${path}/rows?`)) {
            const params = new URLSearchParams(url.split("?")[1]);
            const offset = Number(params.get("offset"));
            const limit = Number(params.get("limit"));
            return reply({ ...dataRows, limit, offset, rows: offset ? [["4", "next page"]] : dataRows.rows, has_more: offset === 0 });
        }
        throw new Error(`Unexpected URL ${url}`);
    };
    await import(`../internal/web/static/js/viewer.js?run=${++run}`);
    await tick();
    return { ...ui, calls };
}

async function openTable(ui) {
    await ui.$("viewer-schemas-list").children[0].children[0].fire("click");
    await ui.$("viewer-tables-list").children[0].children[0].fire("click");
}

test("view-only explorer loads schemas, complex table names, metadata, and read-only rows", async () => {
    const ui = await setup();
    assert.equal(ui.$("viewer-workspace").hidden, false);
    await openTable(ui);
    assert.equal(ui.calls.some(({ url }) => url === path), true);
    assert.equal(ui.calls.some(({ url }) => url === `${path}/rows?limit=50&offset=0`), true);
    assert.equal(ui.calls.every(({ method, credentials }) => method === "GET" && credentials === "same-origin"), true);
    assert.equal(ui.$("viewer-columns-body").children.length, 2);
    assert.equal(ui.$("viewer-constraints-body").children.length, 1);
    assert.equal(ui.$("viewer-indexes-body").children.length, 1);
    assert.equal(ui.$("viewer-columns-body").children[0].children[1].textContent, "id");
    assert.equal(ui.$("viewer-rows-body").children.length, 3);
    assert.equal(ui.$("viewer-rows-body").children[0].children[1].textContent, '<img src=x onerror="alert(1)">');
    assert.equal(ui.$("viewer-rows-body").children[0].children[1].children[0].children.length, 0);
    assert.equal(ui.$("viewer-rows-body").children[1].children[1].children[0].attributes.get("aria-label"), "Nilai NULL");
    assert.equal(ui.$("viewer-rows-body").children[2].children[1].textContent, "(string kosong)");
    assert.match(ui.$("viewer-rows-summary").textContent, /Primary Key/);
});

test("pagination uses exact offset and selected limit and supports previous page", async () => {
    const ui = await setup({ access: "edit" });
    await openTable(ui);
    assert.equal(ui.$("viewer-rows-prev").disabled, true);
    assert.equal(ui.$("viewer-rows-next").disabled, false);
    await ui.$("viewer-rows-next").fire("click");
    assert.equal(ui.calls.at(-1).url, `${path}/rows?limit=50&offset=50`);
    assert.equal(ui.$("viewer-rows-next").disabled, true);
    await ui.$("viewer-rows-prev").fire("click");
    assert.equal(ui.calls.at(-1).url, `${path}/rows?limit=50&offset=0`);
    ui.$("viewer-row-limit").value = "10";
    await ui.$("viewer-row-limit").fire("change");
    await tick();
    assert.equal(ui.calls.at(-1).url, `${path}/rows?limit=10&offset=0`);
});

test("leaving Viewer aborts a pending row request and ignores its late response", async () => {
    let finishRequest;
    const ui = await setup({ custom: (url) => {
        if (url.includes("/rows?")) return new Promise((resolve) => { finishRequest = resolve; });
    } });
    const loading = openTable(ui);
    await tick();
    assert.match(ui.calls.at(-1).url, /\/rows\?/);
    const signal = ui.calls.at(-1).signal;
    window.__spaCleanup();
    assert.equal(signal.aborted, true);
    document.getElementById = () => null;
    finishRequest(reply(dataRows));
    await loading;
    assert.equal(ui.$("viewer-rows-body").children.length, 0);
});

test("leaving Viewer during initialization ignores a late access error", async () => {
    let finishRequest;
    const ui = await setup({ custom: (url) => {
        if (url === "/api/v1/databases") return new Promise((resolve) => { finishRequest = resolve; });
    } });
    assert.equal(ui.calls.at(-1).url, "/api/v1/databases");
    window.__spaCleanup();
    document.getElementById = () => null;
    finishRequest(reply({ error: { code: "unauthenticated" } }, 401));
    await tick();
    assert.deepEqual(ui.redirects, []);
});

test("empty schema, empty table, and empty rows present distinct empty states", async () => {
    const empty = await setup({ custom: (url) => url.endsWith("/schemas") ? reply({ schemas: [] }) : undefined });
    assert.equal(empty.$("viewer-schemas-empty").hidden, false);
    assert.equal(empty.$("viewer-tables-panel").hidden, true);
    const noTables = await setup({ custom: (url) => url.endsWith("/tables") ? reply({ tables: [] }) : undefined });
    await noTables.$("viewer-schemas-list").children[0].children[0].fire("click");
    assert.equal(noTables.$("viewer-tables-empty").hidden, false);
    const noRows = await setup({ custom: (url) => url.includes("/rows?") ? reply({ ...dataRows, rows: [], has_more: false }) : undefined });
    await openTable(noRows);
    assert.equal(noRows.$("viewer-rows-empty").hidden, false);
    assert.equal(noRows.$("viewer-rows-table").hidden, true);
});

test("413 row size error is recoverable by reducing limit without losing metadata", async () => {
    let large = true;
    const ui = await setup({ custom: (url) => url.includes("/rows?") && large
        ? reply({ error: { code: "result_too_large", message: "too large" } }, 413) : undefined });
    await openTable(ui);
    assert.match(ui.$("viewer-rows-error").textContent, /4 MiB/);
    assert.equal(ui.$("viewer-metadata").hidden, false);
    large = false;
    ui.$("viewer-row-limit").value = "10";
    await ui.$("viewer-row-limit").fire("change");
    await tick();
    assert.equal(ui.$("viewer-rows-error").hidden, true);
    assert.equal(ui.$("viewer-rows-body").children.length, 3);
});

test("503 target offline can be retried manually", async () => {
    let offline = true;
    const ui = await setup({ custom: (url) => url.includes("/rows?") && offline
        ? reply({ error: { code: "target_unavailable", message: "offline" } }, 503) : undefined });
    await openTable(ui);
    assert.match(ui.$("viewer-rows-error").textContent, /tidak tersedia/);
    offline = false;
    await ui.$("viewer-refresh-rows").fire("click");
    assert.equal(ui.$("viewer-rows-error").hidden, true);
    assert.equal(ui.$("viewer-rows-body").children.length, 3);
});

test("403 after grant revocation clears rendered rows and closes workspace", async () => {
    let revoked = false;
    const ui = await setup({ custom: (url) => url.includes("/rows?") && revoked
        ? reply({ error: { code: "forbidden", message: "No grant" } }, 403) : undefined });
    await openTable(ui);
    assert.equal(ui.$("viewer-rows-body").children.length, 3);
    revoked = true;
    await ui.$("viewer-refresh-rows").fire("click");
    assert.equal(ui.$("viewer-workspace").hidden, true);
    assert.equal(ui.$("viewer-rows-body").children.length, 0);
    assert.match(ui.$("viewer-error").textContent, /dicabut/);
});

test("401 during nested API request redirects to login and drops data", async () => {
    const ui = await setup({ custom: (url) => url === path
        ? reply({ error: { code: "unauthenticated", message: "expired" } }, 401) : undefined });
    await openTable(ui);
    assert.deepEqual(ui.redirects, ["/login"]);
    assert.equal(ui.$("viewer-workspace").hidden, true);
});

test("404 schema lookup remains recoverable with another schema selection", async () => {
    let missing = true;
    const ui = await setup({ custom: (url) => url.endsWith("/tables") && missing
        ? reply({ error: { code: "not_found", message: "gone" } }, 404) : undefined });
    await ui.$("viewer-schemas-list").children[0].children[0].fire("click");
    assert.match(ui.$("viewer-tables-error").textContent, /tidak ditemukan/);
    missing = false;
    await ui.$("viewer-refresh-tables").fire("click");
    assert.equal(ui.$("viewer-tables-error").hidden, true);
    assert.equal(ui.$("viewer-tables-list").children.length, 1);
});

test("system schemas are filtered even if a fixture accidentally returns them", async () => {
    const ui = await setup({ custom: (url) => url.endsWith("/schemas")
        ? reply({ schemas: ["pg_catalog", "information_schema", "_terserah", schema] }) : undefined });
    assert.equal(ui.$("viewer-schemas-list").children.length, 1);
    assert.equal(ui.$("viewer-schemas-list").children[0].textContent, schema);
});

test("ungranted admin cannot request target metadata", async () => {
    const ui = await setup({ available: false });
    assert.equal(ui.$("viewer-workspace").hidden, true);
    assert.match(ui.$("viewer-error").textContent, /tidak memiliki grant/);
    assert.equal(ui.calls.some(({ url }) => url.includes("/schemas")), false);
});

test("malformed metadata never appears as a successful table load", async () => {
    const ui = await setup({ custom: (url) => url === path ? reply({ columns: [], constraints: [] }) : undefined });
    await openTable(ui);
    assert.match(ui.$("viewer-detail-error").textContent, /Format metadata/);
    assert.equal(ui.$("viewer-metadata").hidden, true);
    assert.equal(ui.calls.some(({ url }) => url.includes("/rows?")), false);
});
