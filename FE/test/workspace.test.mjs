import test from "node:test";
import assert from "node:assert/strict";
import { Element, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";

const big = "9007199254740993";
const connection = { id: big, name: '<img onerror="alert(1)">', database_name: "target", access: "edit" };
const group = { id: "19", name: "Editor", user_ids: ["99"], grants: [{ database_id: big, access_level: "view" }] };
let run = 0;

function response(payload, status = 200) {
    return new Response(JSON.stringify(payload), { status, headers: { "Content-Type": "application/json" } });
}

function installDOM(ids) {
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    const redirects = [];
    let confirm = true;
    globalThis.document = {
        getElementById: (id) => elements[id] ?? null,
        createElement: (tag) => new Element(tag)
    };
    globalThis.window = {
        location: { replace: (url) => redirects.push(url) },
        confirm: () => confirm
    };
    return { elements, redirects, setConfirm: (value) => { confirm = value; }, $: (id) => elements[id] };
}

async function setupGrants({ admin = true, connections = [connection], groups = [group], custom } = {}) {
    clearAuthState();
    const ui = installDOM([
        "grants-status", "grants-error", "grants-panel", "grants-no-groups", "grants-empty",
        "grants-table", "grants-body", "grant-form", "grant-group", "grant-database",
        "grants-no-databases", "grants-refresh", "grant-access", "grant-submit", "grant-message"
    ]);
    for (const id of ["grants-error", "grants-panel", "grants-no-groups", "grants-empty", "grants-table",
        "grants-no-databases", "grant-message"]) ui.$(id).hidden = true;
    ui.$("grant-access").value = "view";
    const calls = [];
    const state = { groups: structuredClone(groups), connections: [...connections] };
    globalThis.fetch = async (url, options = {}) => {
        const method = options.method ?? "GET";
        const data = options.body ? JSON.parse(options.body) : null;
        calls.push({ url, method, data, headers: new Headers(options.headers), credentials: options.credentials });
        if (custom) {
            const intercepted = custom({ url, method, data, state });
            if (intercepted !== undefined) return intercepted;
        }
        if (url === "/api/v1/auth/me") return response({ id: "1", username: "admin", is_platform_admin: admin });
        if (url === "/api/v1/auth/csrf") return response({ csrf_token: "csrf" });
        if (url === "/api/v1/admin/databases") return response({ databases: state.connections });
        if (url === "/api/v1/admin/groups") return response({ groups: state.groups });
        if (url.includes("/databases/") && method === "PUT") {
            const id = url.split("/").at(-1);
            const g = state.groups[0];
            const previous = g.grants.find((x) => x.database_id === id);
            if (previous) previous.access_level = data.access_level;
            else g.grants.push({ database_id: id, access_level: data.access_level });
            return response({ grant: { group_id: g.id, database_id: id, access_level: data.access_level } });
        }
        if (url.includes("/databases/") && method === "DELETE") {
            state.groups[0].grants = state.groups[0].grants.filter((g) => g.database_id !== url.split("/").at(-1));
            return response({ status: "ok" });
        }
        throw new Error(`unhandled ${method} ${url}`);
    };
    await import(`../internal/web/static/js/grants.js?run=${++run}`);
    await tick();
    return { ...ui, calls, state, actions: (i) => ui.$("grants-body").children[i].children[3].children };
}

async function setupDashboard({ admin = false, databases = [{ ...connection, access: "view" }], custom } = {}) {
    clearAuthState();
    const ui = installDOM([
        "dashboard-status", "dashboard-content", "welcome-username", "account-role", "admin-nav", "logout-button", "navigation-status",
        "databases-status", "databases-error", "databases-empty", "databases-table", "databases-body"
    ]);
    const calls = [];
    for (const id of ["dashboard-content", "admin-nav", "databases-error", "databases-empty", "databases-table"]) ui.$(id).hidden = true;
    globalThis.fetch = async (url, options = {}) => {
        calls.push({ url, method: options.method ?? "GET" });
        if (custom) {
            const intercepted = custom(url);
            if (intercepted !== undefined) return intercepted;
        }
        if (url === "/api/v1/auth/me") return response({ id: "99", username: "viewer", is_platform_admin: admin });
        if (url === "/api/v1/databases") return response({ databases });
        if (url === "/api/v1/auth/csrf") return response({ csrf_token: "csrf" });
        if (url === "/api/v1/auth/logout") return new Response(null, { status: 204 });
        throw new Error(`unhandled ${url}`);
    };
    await import(`../internal/web/static/js/dashboard.js?run=${++run}`);
    await tick();
    return { ...ui, calls };
}

async function setupViewer({ access = "view", schemas = ["public"], listed = true, custom } = {}) {
    clearAuthState();
    const ui = installDOM([
        "viewer-page", "viewer-status", "viewer-error", "viewer-workspace", "viewer-schemas-status",
        "viewer-database-name", "viewer-target-name", "viewer-access", "viewer-drafts-link",
        "viewer-schemas-list", "viewer-schemas-empty", "viewer-schemas-error", "viewer-refresh-schemas",
        "viewer-tables-panel", "viewer-selected-schema", "viewer-refresh-tables", "viewer-tables-status",
        "viewer-tables-error", "viewer-tables-empty", "viewer-tables-list", "viewer-detail",
        "viewer-selected-table", "viewer-refresh-detail", "viewer-detail-status", "viewer-detail-error", "viewer-metadata",
        "viewer-rows-panel", "viewer-refresh-rows", "viewer-row-limit", "viewer-rows-status", "viewer-rows-error",
        "viewer-rows-head", "viewer-rows-body", "viewer-rows-table", "viewer-rows-empty", "viewer-rows-summary",
        "viewer-rows-prev", "viewer-rows-next", "viewer-columns-body", "viewer-constraints-body", "viewer-indexes-body",
        "viewer-columns-table", "viewer-constraints-table", "viewer-indexes-table", "viewer-columns-empty",
        "viewer-constraints-empty", "viewer-indexes-empty"
    ]);
    ui.$("viewer-page").dataset.databaseId = big;
    ui.$("viewer-row-limit").value = "50";
    for (const id of ["viewer-error", "viewer-workspace", "viewer-schemas-empty", "viewer-drafts-link"]) ui.$(id).hidden = true;
    const calls = [];
    globalThis.fetch = async (url) => {
        calls.push(url);
        if (custom) {
            const intercepted = custom(url);
            if (intercepted !== undefined) return intercepted;
        }
        if (url === "/api/v1/auth/me") return response({ id: "5", username: "u", is_platform_admin: true });
        if (url === "/api/v1/databases") return response({ databases: listed ? [{ ...connection, access }] : [] });
        if (url === `/api/v1/databases/${big}/schemas`) return response({ schemas });
        throw new Error(`unhandled ${url}`);
    };
    await import(`../internal/web/static/js/viewer.js?run=${++run}`);
    await tick();
    return { ...ui, calls };
}

test("grant select is populated from BE2 and preserves IDs beyond JS safe integer", async () => {
    const ui = await setupGrants();
    assert.equal(ui.$("grant-database").children[1].value, big);
    assert.match(ui.$("grant-database").children[1].textContent, /ID 9007199254740993/);
    assert.equal(ui.$("grants-body").children[0].children[1].textContent, `${connection.name} (ID ${big})`);
    assert.equal(ui.$("grants-body").children[0].children[1].children.length, 0);
    assert.equal(ui.calls.some(({ url }) => url === "/api/v1/admin/databases"), true);
});

test("admin can set edit grant using selected target with CSRF", async () => {
    const ui = await setupGrants();
    ui.$("grant-group").value = "19";
    ui.$("grant-database").value = big;
    ui.$("grant-access").value = "edit";
    await ui.$("grant-form").fire("submit");
    const put = ui.calls.find((c) => c.method === "PUT");
    assert.equal(put.url, `/api/v1/admin/groups/19/databases/${big}`);
    assert.deepEqual(put.data, { access_level: "edit" });
    assert.equal(put.headers.get("X-CSRF-Token"), "csrf");
    assert.equal(put.credentials, "same-origin");
    assert.match(ui.$("grant-message").textContent, /berhasil/);
});

test("free text or unknown target cannot be submitted", async () => {
    const ui = await setupGrants();
    ui.$("grant-group").value = "19";
    ui.$("grant-database").value = "33";
    await ui.$("grant-form").fire("submit");
    assert.equal(ui.calls.some((c) => c.method === "PUT"), false);
    assert.match(ui.$("grant-message").textContent, /Pilih group/);
});

test("missing database remains visible with fallback and cannot be edited", async () => {
    const ui = await setupGrants({ connections: [] });
    assert.match(ui.$("grants-body").children[0].children[1].textContent, /Koneksi tidak tersedia/);
    assert.equal(ui.actions(0)[0].disabled, true);
    assert.equal(ui.actions(0)[1].disabled, false);
    assert.equal(ui.$("grant-submit").disabled, true);
    assert.equal(ui.$("grants-no-databases").hidden, false);
    ui.setConfirm(false);
    await ui.actions(0)[1].fire("click");
    assert.equal(ui.calls.some((c) => c.method === "DELETE"), false);
});

test("confirmed revoke uses exact ID and refreshes list", async () => {
    const ui = await setupGrants();
    await ui.actions(0)[1].fire("click");
    assert.equal(ui.calls.some((c) => c.method === "DELETE" && c.url.endsWith(big)), true);
    assert.equal(ui.$("grants-empty").hidden, false);
});

test("self-member downgrade redirects because backend may revoke session", async () => {
    const ui = await setupGrants({ groups: [{ ...group, user_ids: ["1"], grants: [{ database_id: big, access_level: "edit" }] }] });
    ui.$("grant-group").value = "19";
    ui.$("grant-database").value = big;
    ui.$("grant-access").value = "view";
    await ui.$("grant-form").fire("submit");
    assert.deepEqual(ui.redirects, ["/login"]);
});

test("non-admin does not fetch admin resources", async () => {
    const ui = await setupGrants({ admin: false });
    assert.equal(ui.$("grants-panel").hidden, true);
    assert.equal(ui.calls.some(({ url }) => url === "/api/v1/admin/groups"), false);
});

test("403 during admin data fetch is not treated as success", async () => {
    const ui = await setupGrants({ custom: ({ url }) => url === "/api/v1/admin/groups"
        ? response({ error: { code: "forbidden", message: "No grant" } }, 403) : undefined });
    assert.equal(ui.$("grants-panel").hidden, true);
    assert.match(ui.$("grants-error").textContent, /Akses admin/);
});

test("grant update 404 does not report saved success", async () => {
    const ui = await setupGrants({ custom: ({ method }) => method === "PUT"
        ? response({ error: { code: "not_found", message: "gone" } }, 404) : undefined });
    ui.$("grant-group").value = "19";
    ui.$("grant-database").value = big;
    ui.$("grant-access").value = "edit";
    await ui.$("grant-form").fire("submit");
    assert.match(ui.$("grant-message").textContent, /tidak ditemukan/);
});

test("refresh retrieves newly registered database without reloading whole page", async () => {
    const ui = await setupGrants({ connections: [] });
    ui.state.connections.push(connection);
    await ui.$("grants-refresh").fire("click");
    assert.equal(ui.$("grant-database").children.length, 2);
    assert.equal(ui.$("grants-no-databases").hidden, true);
});

test("view-only dashboard offers Viewer and History but not Draft", async () => {
    const ui = await setupDashboard();
    const links = ui.$("databases-body").children[0].children[4].children;
    assert.deepEqual(links.map((link) => link.href), [
        `/databases/${big}/viewer`, `/databases/${big}/history`
    ]);
    assert.equal(ui.$("admin-nav").hidden, true);
});

test("edit dashboard offers all workspace destinations", async () => {
    const ui = await setupDashboard({ databases: [connection] });
    assert.equal(ui.$("databases-body").children[0].children[4].children.length, 3);
    assert.equal(ui.$("databases-body").children[0].children[4].children[2].href, `/databases/${big}/drafts`);
});

test("admin without group grant sees empty state rather than target bypass", async () => {
    const ui = await setupDashboard({ admin: true, databases: [] });
    assert.equal(ui.$("databases-empty").hidden, false);
    assert.match(ui.$("databases-empty").textContent, /tidak otomatis/);
    assert.equal(ui.$("admin-nav").hidden, false);
});

test("navigation logout uses the authenticated session", async () => {
    const ui = await setupDashboard({ admin: true });
    assert.equal(ui.$("logout-button").disabled, false);
    await ui.$("logout-button").fire("click");
    assert.equal(ui.calls.some((call) => call.url === "/api/v1/auth/logout" && call.method === "POST"), true);
    assert.deepEqual(ui.redirects, ["/login"]);
});

test("revoked session redirects and rejected database list hides previous entries", async () => {
    const ui = await setupDashboard({ custom: (url) => url === "/api/v1/databases"
        ? response({ error: { code: "unauthenticated", message: "expired" } }, 401) : undefined });
    assert.deepEqual(ui.redirects, ["/login"]);
    assert.equal(ui.$("databases-body").children.length, 0);
});

test("viewer checks grant then reads schema list safely", async () => {
    const ui = await setupViewer({ schemas: ['<img src=x onerror="x">'] });
    assert.equal(ui.$("viewer-workspace").hidden, false);
    assert.equal(ui.$("viewer-drafts-link").hidden, true);
    assert.equal(ui.$("viewer-schemas-list").children[0].textContent, '<img src=x onerror="x">');
    assert.equal(ui.$("viewer-schemas-list").children[0].children[0].textContent, '<img src=x onerror="x">');
});

test("viewer denies admin without grant, no schema request is made", async () => {
    const ui = await setupViewer({ listed: false });
    assert.equal(ui.$("viewer-workspace").hidden, true);
    assert.match(ui.$("viewer-error").textContent, /tidak memiliki grant/);
    assert.equal(ui.calls.some((c) => c.endsWith("/schemas")), false);
});

test("viewer handles 403 after grant revoked without revealing schema", async () => {
    const ui = await setupViewer({ custom: (url) => url.endsWith("/schemas")
        ? response({ error: { code: "forbidden", message: "revoked" } }, 403) : undefined });
    assert.equal(ui.$("viewer-workspace").hidden, true);
    assert.match(ui.$("viewer-error").textContent, /dicabut/);
});

test("viewer edit access shows draft shortcut and 503 remains an error", async () => {
    const ui = await setupViewer({ access: "edit", custom: (url) => url.endsWith("/schemas")
        ? response({ error: { code: "target_unavailable", message: "offline" } }, 503) : undefined });
    assert.equal(ui.$("viewer-workspace").hidden, true);
    assert.match(ui.$("viewer-error").textContent, /tidak tersedia/);
});
