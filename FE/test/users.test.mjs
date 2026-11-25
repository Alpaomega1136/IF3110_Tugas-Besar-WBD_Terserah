import test from "node:test";
import assert from "node:assert/strict";
import { Element, tick } from "./mock-dom.mjs";
import { authenticatedRequest, clearAuthState, resetAuthSession } from "../internal/web/static/js/auth.js";

let generation = 0;
const account = { id: "22", username: "existing", is_platform_admin: false, is_active: true, group_ids: [] };
const json = (body) => new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });

async function setup() {
    clearAuthState();
    const ids = [
        "users-status", "users-error", "users-empty", "users-section", "users-body", "create-user-section",
        "create-user-form", "create-username", "create-password", "create-is-admin", "create-user-submit",
        "create-user-message", "create-username-error", "create-password-error", "create-password-help", "edit-user-section",
        "edit-user-form", "edit-user-name", "edit-password", "edit-password-error", "edit-password-help", "edit-is-admin",
        "edit-is-active", "edit-user-submit", "edit-user-cancel", "edit-user-message"
    ];
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    const $ = (id) => elements[id];
    $("edit-user-section").hidden = true;
    $("create-username").value = "new-user";
    $("create-is-admin").checked = false;
    $("create-user-form").reset = () => { $("create-password").value = ""; };
    $("edit-user-form").reset = () => { $("edit-password").value = ""; };
    const calls = [];
    globalThis.document = { getElementById: $, createElement: (tag) => new Element(tag) };
    globalThis.window = { location: { replace() {} }, confirm: () => true };
    globalThis.fetch = async (url, options = {}) => {
        const method = options.method ?? "GET";
        const body = options.body ? JSON.parse(options.body) : null;
        calls.push({ url, method, body, headers: new Headers(options.headers) });
        if (url === "/api/v1/auth/me") return json({ id: "11", username: "admin", is_platform_admin: true });
        if (url === "/api/v1/auth/csrf") return json({ csrf_token: "fixture-csrf" });
        if (url === "/api/v1/admin/users") return json(method === "GET" ? { users: [account] } : { user: account });
        if (url === "/api/v1/admin/users/22" && method === "PATCH") return json({ user: { ...account, ...body } });
        throw new Error(`Unexpected ${method} ${url}`);
    };
    await import(`../internal/web/static/js/users.js?passwordLimits=${++generation}`);
    await tick();
    return { $, calls };
}

const mutations = (ui) => ui.calls.filter((call) => ["POST", "PATCH"].includes(call.method));

test("edit panel stays hidden until an account is selected and closes on cancel", async () => {
    const ui = await setup();
    assert.equal(ui.$("edit-user-section").hidden, true);
    await ui.$("users-body").children[0].children[1].fire("click");
    assert.equal(ui.$("edit-user-section").hidden, false);
    assert.equal(ui.$("edit-user-name").textContent, "existing");
    await ui.$("edit-user-cancel").fire("click");
    assert.equal(ui.$("edit-user-section").hidden, true);
});

test("create user rejects passwords below 12 or above 1024 UTF-8 bytes before POST", async () => {
    for (const password of ["a".repeat(11), "€".repeat(3), "a".repeat(1025)]) {
        const ui = await setup();
        ui.$("create-password").value = password;
        await ui.$("create-user-form").fire("submit");
        assert.equal(mutations(ui).length, 0);
        assert.equal(ui.$("create-password-error").hidden, false);
        assert.equal(ui.$("create-password-help").hidden, true);
        assert.match(ui.$("create-password-error").textContent, /12.*1024/);
    }
});

test("create user accepts 12 through 1024 bytes, including Unicode, with CSRF", async () => {
    for (const password of ["a".repeat(12), "€".repeat(4), "a".repeat(1024)]) {
        const ui = await setup();
        ui.$("create-password").value = password;
        await ui.$("create-user-form").fire("submit");
        assert.equal(mutations(ui).length, 1);
        assert.equal(mutations(ui)[0].body.password, password);
        assert.equal(mutations(ui)[0].headers.get("X-CSRF-Token"), "fixture-csrf");
    }
});

test("edit user enforces the same password byte limits before PATCH", async () => {
    for (const [password, valid] of [["a".repeat(11), false], ["€".repeat(3), false],
        ["a".repeat(1025), false], ["a".repeat(12), true], ["€".repeat(4), true], ["a".repeat(1024), true]]) {
        const ui = await setup();
        await ui.$("users-body").children[0].children[1].fire("click");
        ui.$("edit-password").value = password;
        await ui.$("edit-user-form").fire("submit");
        assert.equal(mutations(ui).length, valid ? 1 : 0);
        if (valid) {
            assert.equal(mutations(ui)[0].method, "PATCH");
            assert.equal(mutations(ui)[0].body.password, password);
        } else {
            assert.match(ui.$("edit-password-error").textContent, /12.*1024/);
            assert.equal(ui.$("edit-password-help").hidden, true);
        }
    }
});

test("edit user can change flags without supplying a new password", async () => {
    const ui = await setup();
    await ui.$("users-body").children[0].children[1].fire("click");
    ui.$("edit-is-active").checked = false;
    await ui.$("edit-user-form").fire("submit");
    assert.equal(mutations(ui).length, 1);
    assert.deepEqual(mutations(ui)[0].body, { is_active: false });
});

test("login submits a legacy password and replaces the login entry via SPA", async () => {
    clearAuthState();
    const elements = Object.fromEntries(["login-form", "login-button", "login-message"].map((id) => [id, new Element()]));
    elements["login-form"].reset = () => {};
    const calls = [], redirects = [];
    const originalFormData = globalThis.FormData;
    globalThis.FormData = class { get(name) { return name === "username" ? "legacy" : "old-pass"; } };
    globalThis.document = { getElementById: (id) => elements[id] };
    globalThis.window = {
        __spaNavigate: (path, options) => redirects.push([path, options]),
        location: { replace: () => assert.fail("login reloaded the page") }
    };
    globalThis.fetch = async (url, options = {}) => {
        if (url === "/api/v1/auth/csrf") return json({ csrf_token: "fixture-csrf" });
        calls.push({ url, body: JSON.parse(options.body) });
        return json({ id: "22", username: "legacy", is_platform_admin: false });
    };
    try {
        await import("../internal/web/static/js/login.js");
        await elements["login-form"].fire("submit");
        assert.equal(calls.length, 1);
        assert.equal(calls[0].body.password, "old-pass");
        assert.deepEqual(redirects, [["/dashboard", { replace: true }]]);
    } finally {
        globalThis.FormData = originalFormData;
    }
});

test("a late 401 is discarded after the session changes", async () => {
    let finish;
    globalThis.fetch = () => new Promise((resolve) => { finish = resolve; });
    const request = authenticatedRequest("/api/v1/databases");
    resetAuthSession();
    finish(new Response(null, { status: 401 }));
    await assert.rejects(request, (error) => error.status === undefined && /Sesi berubah/.test(error.message));
});
