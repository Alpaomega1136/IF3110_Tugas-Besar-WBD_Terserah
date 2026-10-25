import test from "node:test";
import assert from "node:assert/strict";
import { installDOM, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";

let counter = 0;
const bigID = "9007199254740993";
const example = {
    id: bigID, name: "Layanan", host: "localhost", port: 5432,
    database_name: "app", username: "target", sslmode: "disable", created_at: "2026-10-24T12:00:00Z"
};

function response(data, status = 200) {
    return new Response(status === 204 ? null : JSON.stringify(data), {
        status, headers: { "Content-Type": "application/json" }
    });
}

async function setup({ admin = true, initial = [example], custom } = {}) {
    clearAuthState();
    const ui = installDOM();
    const calls = [];
    const state = { entries: [...initial] };
    globalThis.fetch = async (url, options = {}) => {
        const method = options.method ?? "GET";
        const data = options.body ? JSON.parse(options.body) : null;
        calls.push({ url, method, data, headers: new Headers(options.headers), credentials: options.credentials });
        if (custom) {
            const intercepted = custom({ url, method, data, state });
            if (intercepted !== undefined) return intercepted;
        }
        if (url === "/api/v1/auth/me") return response({ id: "8", username: "admin", is_platform_admin: admin });
        if (url === "/api/v1/auth/csrf") return response({ csrf_token: "test-csrf" });
        if (url === "/api/v1/admin/databases" && method === "GET") return response({ databases: state.entries });
        if (url === "/api/v1/admin/databases" && method === "POST") {
            const created = { ...data, id: "33", created_at: example.created_at };
            delete created.password;
            state.entries.push(created);
            return response(created, 201);
        }
        if (url === `/api/v1/admin/databases/${bigID}` && method === "PATCH") {
            Object.assign(state.entries[0], data);
            return response(state.entries[0]);
        }
        if (url === `/api/v1/admin/databases/${bigID}/test` && method === "POST") return response({ status: "ok" });
        if (url === `/api/v1/admin/databases/${bigID}` && method === "DELETE") {
            state.entries = state.entries.filter((entry) => entry.id !== bigID);
            return response(null, 204);
        }
        throw new Error(`Unhandled: ${method} ${url}`);
    };
    await import(`../internal/web/static/js/connections.js?test=${++counter}`);
    await tick();
    return { ...ui, calls, state, $: (id) => ui.elements[id] };
}

function fillCreate(ui, overrides = {}) {
    const fields = {
        name: "Target Baru", host: "localhost", port: "5432", database_name: "target_demo",
        username: "executor", password: "safe-test-secret", sslmode: "disable", ...overrides
    };
    for (const [key, value] of Object.entries(fields)) ui.field("create", key).value = value;
}

test("non-admin cannot see or load connection management", async () => {
    const ui = await setup({ admin: false });
    assert.equal(ui.$("connections-content").hidden, true);
    assert.equal(ui.$("connections-forbidden").hidden, false);
    assert.equal(ui.calls.some((call) => call.url === "/api/v1/admin/databases"), false);
});

test("unauthenticated session redirects and hides data", async () => {
    const ui = await setup({ custom: ({ url }) => url === "/api/v1/auth/me"
        ? response({ error: { code: "unauthenticated", message: "Login required" } }, 401) : undefined });
    assert.deepEqual(ui.redirects, ["/login"]);
    assert.equal(ui.$("connections-content").hidden, true);
});

test("empty state shows registration form and no table", async () => {
    const ui = await setup({ initial: [] });
    assert.equal(ui.$("connections-content").hidden, false);
    assert.equal(ui.$("connections-empty").hidden, false);
    assert.equal(ui.$("connections-table").hidden, true);
});

test("list shows big IDs exactly and safely renders untrusted database text", async () => {
    const payload = { ...example, name: '<img src=x onerror="alert(1)">' };
    const ui = await setup({ initial: [payload] });
    const row = ui.$("connections-body").children[0];
    assert.equal(row.children[0].textContent, bigID);
    assert.equal(row.children[1].textContent, payload.name);
    assert.equal(row.children[1].children.length, 0);
    assert.equal(row.children[7].children.length, 3);
});

test("create sends exact BE2 fields, JSON+CSRF, never preserves password", async () => {
    const ui = await setup({ initial: [] });
    fillCreate(ui);
    await ui.$("connection-create-form").fire("submit");
    const post = ui.calls.find((call) => call.method === "POST" && call.url === "/api/v1/admin/databases");
    assert.deepEqual(post.data, {
        name: "Target Baru", host: "localhost", port: 5432, database_name: "target_demo",
        username: "executor", password: "safe-test-secret", sslmode: "disable"
    });
    assert.equal(post.headers.get("X-CSRF-Token"), "test-csrf");
    assert.equal(post.headers.get("Content-Type"), "application/json");
    assert.equal(post.credentials, "same-origin");
    assert.equal(ui.field("create", "password").value, "");
    assert.equal(ui.$("connections-body").children.length, 1);
    assert.equal(ui.$("connection-action-message").textContent.includes("berhasil dibuat"), true);
    assert.equal(ui.$("connections-body").textContent.includes("safe-test-secret"), false);
});

test("invalid create input is blocked before network mutation", async () => {
    const ui = await setup({ initial: [] });
    fillCreate(ui, { port: "65536" });
    await ui.$("connection-create-form").fire("submit");
    assert.match(ui.$("connection-create-error").textContent, /Port/);
    assert.equal(ui.calls.some((call) => call.method === "POST"), false);
});

test("edit sends only allowed fields, optional password absent, keeps big ID", async () => {
    const ui = await setup();
    await ui.actions(0)[0].fire("click");
    assert.equal(ui.$("connection-edit-section").hidden, false);
    assert.equal(ui.$("connection-edit-database").textContent, "app");
    assert.equal(ui.field("edit", "password").value, "");
    ui.field("edit", "name").value = "Renamed";
    ui.field("edit", "sslmode").value = "require";
    await ui.$("connection-edit-form").fire("submit");
    const patch = ui.calls.find((call) => call.method === "PATCH");
    assert.equal(patch.url, `/api/v1/admin/databases/${bigID}`);
    assert.deepEqual(patch.data, { name: "Renamed", username: "target", sslmode: "require" });
    assert.equal(ui.$("connection-edit-section").hidden, true);
});

test("edit with new password sends password but not host, port, database_name", async () => {
    const ui = await setup();
    await ui.actions(0)[0].fire("click");
    ui.field("edit", "password").value = "new-password";
    await ui.$("connection-edit-form").fire("submit");
    const patch = ui.calls.find((call) => call.method === "PATCH");
    assert.equal(patch.data.password, "new-password");
    for (const field of ["host", "port", "database_name"]) assert.equal(field in patch.data, false);
    assert.equal(ui.field("edit", "password").value, "");
});

test("test connection success and 503 target_unavailable have distinct feedback", async () => {
    const ui = await setup({ custom: ({ url, method }) => url.endsWith("/test") && method === "POST"
        ? response({ error: { code: "target_unavailable", message: "Cannot connect" } }, 503) : undefined });
    await ui.actions(0)[1].fire("click");
    assert.match(ui.$("connection-action-message").textContent, /tidak dapat dihubungi/);
    assert.equal(ui.$("connection-action-message").attributes.get("role"), "alert");
    const pass = await setup();
    await pass.actions(0)[1].fire("click");
    assert.match(pass.$("connection-action-message").textContent, /berhasil diuji/);
});

test("metadata database is explicitly rejected in test feedback", async () => {
    const ui = await setup({ custom: ({ url }) => url.endsWith("/test")
        ? response({ error: { code: "metadata_target_forbidden", message: "Forbidden" } }, 409) : undefined });
    await ui.actions(0)[1].fire("click");
    assert.match(ui.$("connection-action-message").textContent, /metadata platform tidak boleh/);
});

test("delete requires confirmation and refreshes list after 204", async () => {
    const ui = await setup();
    ui.setConfirm(false);
    await ui.actions(0)[2].fire("click");
    assert.equal(ui.calls.some((call) => call.method === "DELETE"), false);
    ui.setConfirm(true);
    await ui.actions(0)[2].fire("click");
    assert.equal(ui.calls.some((call) => call.method === "DELETE" && call.url.endsWith(bigID)), true);
    assert.equal(ui.$("connections-empty").hidden, false);
    assert.match(ui.$("connection-action-message").textContent, /berhasil dihapus/);
});

test("409 dependent records leaves target intact and shows conflict", async () => {
    const ui = await setup({ custom: ({ method }) => method === "DELETE"
        ? response({ error: { code: "connection_conflict", message: "Dependent records" } }, 409) : undefined });
    await ui.actions(0)[2].fire("click");
    assert.match(ui.$("connection-action-message").textContent, /masih memiliki grant atau riwayat/);
    assert.equal(ui.$("connections-body").children.length, 1);
});

test("server-side forbidden on list hides admin actions", async () => {
    const ui = await setup({ custom: ({ url }) => url === "/api/v1/admin/databases"
        ? response({ error: { code: "forbidden", message: "Access denied" } }, 403) : undefined });
    assert.equal(ui.$("connections-content").hidden, true);
    assert.equal(ui.$("connections-forbidden").hidden, false);
});

test("csrf_rejected displays origin guidance instead of permission message", async () => {
    const ui = await setup({ custom: ({ url, method }) => url.endsWith("/test") && method === "POST"
        ? response({ error: { code: "csrf_rejected", message: "Rejected" } }, 403) : undefined });
    await ui.actions(0)[1].fire("click");
    assert.match(ui.$("connection-action-message").textContent, /APP_ORIGIN/);
    assert.equal(ui.$("connections-content").hidden, false);
});

test("duplicate target 409 does not claim creation succeeded", async () => {
    const ui = await setup({ custom: ({ url, method }) => url === "/api/v1/admin/databases" && method === "POST"
        ? response({ error: { code: "connection_conflict", message: "Already registered" } }, 409) : undefined });
    fillCreate(ui);
    await ui.$("connection-create-form").fire("submit");
    assert.match(ui.$("connection-create-error").textContent, /sudah terdaftar/);
    assert.equal(ui.$("connection-action-message").textContent, "");
    assert.equal(ui.$("connections-body").children.length, 1);
});

test("identity conflict on PATCH remains visible and edit form stays open", async () => {
    const ui = await setup({ custom: ({ url, method }) => url.endsWith(bigID) && method === "PATCH"
        ? response({ error: { code: "connection_conflict", message: "Identity change" } }, 409) : undefined });
    await ui.actions(0)[0].fire("click");
    await ui.$("connection-edit-form").fire("submit");
    assert.match(ui.$("connection-edit-error").textContent, /identitas target/);
    assert.equal(ui.$("connection-edit-section").hidden, false);
});

test("list 503 displays recoverable page error without misleading empty state", async () => {
    const ui = await setup({ custom: ({ url }) => url === "/api/v1/admin/databases"
        ? response({ error: { code: "database_unavailable", message: "DB unavailable" } }, 503) : undefined });
    assert.match(ui.$("connections-error").textContent, /tidak tersedia/);
    assert.equal(ui.$("connections-empty").hidden, true);
    assert.equal(ui.$("connections-table").hidden, true);
    assert.equal(ui.$("connections-content").hidden, false);
});
