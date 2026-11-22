import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { Element, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";
import { dbID, jobFixture, attemptFixture } from "./job-fixtures.mjs";

let run = 0;
const template = readFileSync(new URL("../internal/web/templates/pages/jobs.html", import.meta.url), "utf8");
const json = (data, status = 200) => new Response(JSON.stringify(data), { status });

class Node extends Element {
    appendChild(node) { return this.insertBefore(node, null); }
    insertBefore(node, before) {
        node.parentNode?.removeChild(node);
        const i = before ? this.children.indexOf(before) : this.children.length;
        if (i < 0) throw new Error("Reference node missing");
        this.children.splice(i, 0, node); node.parentNode = this; return node;
    }
    removeChild(node) { this.children.splice(this.children.indexOf(node), 1); node.parentNode = null; return node; }
    replaceChildren(...nodes) { this._text = ""; for (const n of this.children) n.parentNode = null; this.children = []; nodes.forEach((n) => this.appendChild(n)); }
    focus() { document.activeElement = this; }
}

async function setup(t, { access = "edit", detail = true, job = jobFixture(), attempts = [attemptFixture()], backend, confirm = true } = {}) {
    globalThis.window?.__spaCleanup?.();
    clearAuthState();
    const elements = Object.fromEntries([...template.matchAll(/id="([a-z0-9-]+)"/g)].map((m) => [m[1], new Node()]));
    const $ = (id) => elements[id];
    $("jobs-page").dataset = { databaseId: dbID, jobId: detail ? job.id : "" };
    $("jobs-page").isConnected = true;
    $("jobs-auto").checked = true;
    const events = new Map(), windowEvents = new Map(), timers = new Map(), calls = [], navigations = [], confirmations = [];
    let timerID = 0;
    t.mock.method(globalThis, "setTimeout", (fn) => { const id = ++timerID; timers.set(id, fn); return id; });
    t.mock.method(globalThis, "clearTimeout", (id) => timers.delete(id));
    globalThis.document = { getElementById: $, createElement: (tag) => new Node(tag), hidden: false,
        addEventListener: (name, fn) => events.set(name, fn), removeEventListener: (name) => events.delete(name) };
    globalThis.window = { __spaNavigate: (path) => navigations.push(path), location: { replace: (path) => navigations.push(path) },
        addEventListener: (name, fn) => windowEvents.set(name, fn), removeEventListener: (name) => windowEvents.delete(name),
        confirm: (message) => { confirmations.push(message); return confirm; } };
    const state = { access, job, attempts, list: [job], hasMore: false, cursor: null };
    globalThis.fetch = async (url, options = {}) => {
        const method = options.method || "GET", body = options.body ? JSON.parse(options.body) : null;
        calls.push({ url, method, body, signal: options.signal, headers: new Headers(options.headers) });
        if (backend) { const response = backend({ url, method, body, options, state }); if (response !== undefined) return await response; }
        if (url === "/api/v1/auth/me") return json({ id: "3", username: "editor", is_platform_admin: false });
        if (url === "/api/v1/auth/csrf") return json({ csrf_token: "jobs-csrf" });
        if (url === "/api/v1/databases") return json({ databases: [{ id: dbID, name: "Orders", access: state.access }] });
        if (url.includes("/jobs?")) return json({ jobs: state.list, has_more: state.hasMore, next_before: state.cursor });
        if (url.endsWith(`/jobs/${job.id}`)) return json({ job: state.job, attempts: state.attempts });
        throw new Error(`Unhandled ${method} ${url}`);
    };
    await import(`../internal/web/static/js/jobs.js?test=${++run}`);
    await tick();
    const cleanup = window.__spaCleanup;
    t.after(cleanup);
    return { $, state, calls, timers, navigations, confirmations, cleanup,
        poll: async () => { const [id, fn] = timers.entries().next().value || []; assert.ok(fn, "no scheduled poll"); timers.delete(id); await fn(); await tick(); },
        visibility: async (hidden) => { document.hidden = hidden; events.get("visibilitychange")?.(); await tick(); } };
}

test("job monitor preserves exact progress, attempt history and keyboard focus during polling", async (t) => {
    const ui = await setup(t);
    assert.equal(ui.$("jobs-workspace").hidden, false);
    assert.equal(ui.$("jobs-detail-rows").textContent, "2000");
    const anchor = ui.$("jobs-list-body").children[0].children[7].children[0]; anchor.focus();
    ui.state.job = { ...ui.state.job, committed_rows: "9007199254740999", status: "success" };
    ui.state.list = [ui.state.job]; ui.state.attempts = [attemptFixture({ status: "success" })];
    await ui.poll();
    assert.equal(ui.$("jobs-detail-rows").textContent, "9007199254740999");
    assert.match(ui.$("jobs-feedback").textContent, /berhasil di-commit/);
    assert.equal(ui.$("jobs-list-body").children[0].children[7].children[0], anchor);
    assert.equal(document.activeElement, anchor);
    assert.equal(ui.timers.size, 1);
    assert.equal(ui.calls.some((c) => c.method !== "GET"), false);
});

test("polling stops and aborts in-flight reads when leaving the page", async (t) => {
    let release, hold = false;
    const ui = await setup(t, { backend: ({ url }) => {
        if (hold && url.includes("/jobs?")) return new Promise((resolve) => { release = () => resolve(json({ jobs: [], has_more: false, next_before: null })); });
    } });
    hold = true;
    const poll = ui.poll(); await tick();
    const request = ui.calls.at(-1);
    ui.$("jobs-page").isConnected = false;
    ui.cleanup();
    assert.equal(request.signal.aborted, true);
    release(); await poll;
    assert.equal(ui.timers.size, 0);
    assert.equal(ui.$("jobs-list-body").children.length, 1);
});

test("background tabs pause polling and resume with one timer", async (t) => {
    const ui = await setup(t);
    await ui.visibility(true); assert.equal(ui.timers.size, 0);
    await ui.visibility(false); assert.equal(ui.timers.size, 1);
    ui.$("jobs-auto").checked = false; await ui.$("jobs-auto").fire("change");
    assert.equal(ui.timers.size, 0);
});

test("polling refreshes edit controls when current database access changes", async (t) => {
    const ui = await setup(t, { job: jobFixture({ status: "partial" }), attempts: [attemptFixture({ status: "partial" })] });
    assert.equal(ui.$("jobs-request-resume").hidden, false);
    assert.equal(ui.$("jobs-drafts-link").hidden, false);
    ui.state.access = "view"; await ui.poll();
    assert.equal(ui.$("jobs-access").textContent, "View Only");
    assert.equal(ui.$("jobs-request-resume").hidden, true);
    assert.equal(ui.$("jobs-drafts-link").hidden, true);
    ui.state.access = "edit"; await ui.poll();
    assert.equal(ui.$("jobs-access").textContent, "View + Edit");
    assert.equal(ui.$("jobs-request-resume").hidden, false);
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
    assert.equal(ui.timers.size, 1);
});

test("partially reverted jobs request compensation approval for the exact latest attempt", async (t) => {
    const job = jobFixture({ status: "partially_reverted", action_type: "compensate", parent_job_id: "2", target_execution_id: "b".repeat(32), can_compensate: true });
    const attempt = attemptFixture({ status: "partially_reverted" });
    const ui = await setup(t, { job, attempts: [attempt], backend: ({ url, method, body }) => {
        if (url.endsWith("/reviews") && method === "POST") return json({ review: {
            id: "91", database_id: dbID, draft_id: job.draft_id, draft_revision: job.draft_revision,
            action_type: "compensate", target_execution_id: attempt.execution_id
        } }, 201);
    } });
    assert.equal(ui.$("jobs-compensation-panel").hidden, false);
    assert.equal(ui.$("jobs-request-compensation").hidden, false);
    await ui.$("jobs-request-compensation").fire("click");
    const request = ui.calls.find((c) => c.url.endsWith("/reviews") && c.method === "POST");
    assert.deepEqual(request.body, { draft_id: job.draft_id, revision: job.draft_revision, action_type: "compensate", target_execution_id: attempt.execution_id });
    assert.equal(request.headers.get("X-CSRF-Token"), "jobs-csrf");
    assert.deepEqual(ui.navigations, [`/databases/${dbID}/reviews/91`]);
});

test("compensation respects revoked edit access and cancelled confirmation", async (t) => {
    const job = jobFixture({ status: "partially_reverted", action_type: "compensate", parent_job_id: "2", target_execution_id: "b".repeat(32), can_compensate: true });
    const attempt = attemptFixture({ status: "partially_reverted" });
    const ui = await setup(t, { job, attempts: [attempt], confirm: false });
    await ui.$("jobs-request-compensation").fire("click");
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
    assert.equal(document.activeElement, ui.$("jobs-request-compensation"));
    ui.state.access = "view"; await ui.poll();
    assert.equal(ui.$("jobs-request-compensation").hidden, true);
    await ui.$("jobs-request-compensation").fire("click");
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
});

test("read failure pauses automatic requests and manual refresh recovers", async (t) => {
    let offline = false;
    const ui = await setup(t, { backend: ({ url }) => offline && url.includes("/jobs?") ? json({ error: { code: "offline", message: "Backend unavailable" } }, 503) : undefined });
    offline = true; await ui.poll();
    assert.equal(ui.timers.size, 0);
    assert.equal(ui.$("jobs-read-error").hidden, false);
    assert.equal(ui.$("jobs-workspace").hidden, false);
    offline = false; await ui.$("jobs-refresh").fire("click");
    assert.equal(ui.$("jobs-read-error").hidden, true);
    assert.equal(ui.timers.size, 1);
});

test("revoked job access clears private data and stops polling", async (t) => {
    let revoked = false;
    const ui = await setup(t, { backend: ({ url }) => revoked && url.includes("/jobs") ? json({ error: { code: "forbidden" } }, 403) : undefined });
    revoked = true; await ui.poll();
    assert.equal(ui.$("jobs-workspace").hidden, true);
    assert.equal(ui.$("jobs-detail-rows").textContent, "");
    assert.equal(ui.$("jobs-list-body").children.length, 0);
    assert.equal(ui.$("jobs-attempts-body").children.length, 0);
    assert.equal(ui.timers.size, 0);
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
});

test("job pagination keeps before cursors as int64 strings and supports empty pages", async (t) => {
    const ui = await setup(t, { detail: false });
    ui.state.hasMore = true; ui.state.cursor = ui.state.job.id;
    await ui.$("jobs-refresh").fire("click");
    ui.state.hasMore = false; ui.state.cursor = null; ui.state.list = [];
    await ui.$("jobs-next").fire("click");
    assert.match(ui.calls.filter((c) => c.url.includes("/jobs?")).at(-1).url, new RegExp(`before=${ui.state.job.id}`));
    assert.equal(ui.$("jobs-list-empty").hidden, false);
    assert.equal(ui.$("jobs-page-number").textContent, "2");
    await ui.$("jobs-prev").fire("click");
    assert.equal(ui.$("jobs-page-number").textContent, "1");
});

test("resume proposal uses fresh immutable coordinates, confirmation and CSRF", async (t) => {
    const job = jobFixture({ status: "partial" });
    const ui = await setup(t, { job, attempts: [attemptFixture({ status: "partial" })], backend: ({ url, body, method }) => {
        if (url.endsWith("/reviews") && method === "POST") return json({ review: { id: "22", database_id: dbID, draft_id: job.draft_id, draft_revision: "1", action_type: "resume", target_execution_id: body.target_execution_id } }, 201);
    } });
    await ui.$("jobs-request-resume").fire("click");
    const post = ui.calls.find((c) => c.method === "POST");
    assert.deepEqual(post.body, { draft_id: "6", revision: "1", action_type: "resume", target_execution_id: "a".repeat(32) });
    assert.equal(post.headers.get("X-CSRF-Token"), "jobs-csrf");
    assert.equal(ui.confirmations.length, 1);
    assert.equal(ui.navigations.at(-1), `/databases/${dbID}/reviews/22`);
    assert.equal(ui.calls.some((c) => c.url.includes("/execute") || (c.url.endsWith("/jobs") && c.method === "POST")), false);
});

test("view-only, existing approved review and cancelled confirmation never create a proposal", async (t) => {
    const job = jobFixture({ status: "partial" });
    const viewer = await setup(t, { job, access: "view", attempts: [attemptFixture({ status: "partial" })] });
    assert.equal(viewer.$("jobs-request-resume").hidden, true);
    await viewer.$("jobs-request-resume").fire("click");
    assert.equal(viewer.calls.some((c) => c.method === "POST"), false);
    const approved = await setup(t, { job: { ...job, resume_review: { id: "22", status: "approved", target_execution_id: "a".repeat(32) } }, attempts: [attemptFixture({ status: "partial" })] });
    assert.equal(approved.$("jobs-request-resume").hidden, true);
    assert.match(approved.$("jobs-resume-review-link").href, /reviews\/22$/);
    const cancelled = await setup(t, { job, attempts: [attemptFixture({ status: "partial" })], confirm: false });
    await cancelled.$("jobs-request-resume").fire("click");
    assert.equal(cancelled.calls.some((c) => c.method === "POST"), false);
    assert.equal(document.activeElement, cancelled.$("jobs-request-resume"));
});

test("uncertain resume response locks mutations until a fresh manual read", async (t) => {
    const ui = await setup(t, { job: jobFixture({ status: "partial" }), attempts: [attemptFixture({ status: "partial" })], backend: ({ method }) => method === "POST" ? Promise.reject(new Error("connection lost")) : undefined });
    await ui.$("jobs-request-resume").fire("click");
    await ui.$("jobs-request-resume").fire("click");
    assert.equal(ui.calls.filter((c) => c.method === "POST").length, 1);
    assert.equal(ui.timers.size, 0);
    assert.match(ui.$("jobs-action-message").textContent, /Periksa status/);
});

test("fresh permission and review checks prevent stale resume proposals", async (t) => {
    const ui = await setup(t, { job: jobFixture({ status: "partial" }), attempts: [attemptFixture({ status: "partial" })] });
    ui.state.access = "view";
    await ui.$("jobs-request-resume").fire("click");
    assert.equal(ui.$("jobs-request-resume").hidden, true);
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
    const changed = await setup(t, { job: jobFixture({ status: "partial" }), attempts: [attemptFixture({ status: "partial" })] });
    changed.state.job = { ...changed.state.job, resume_review: { id: "22", status: "pending_review", target_execution_id: "a".repeat(32) } };
    await changed.$("jobs-request-resume").fire("click");
    assert.match(changed.$("jobs-resume-review-link").href, /reviews\/22$/);
    assert.equal(changed.calls.some((c) => c.method === "POST"), false);
});
