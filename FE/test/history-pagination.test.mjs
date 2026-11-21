import test from "node:test";
import assert from "node:assert/strict";
import { Element, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";
import { parseCursorPage, parseReconcileResult, positiveInt64 } from "../internal/web/static/js/pagination.js";

const db = "9007199254740993";
const api = `/api/v1/databases/${db}/migrations`;
const v0 = BigInt("9007199254741199");
const first = Array.from({ length: 50 }, (_, i) => ({
    database_id: db, version: String(v0 - BigInt(i)), draft_id: "9007199254741011", draft_revision: "4",
    execution_id: `exec-${i}`, status: i === 0 ? "unknown" : "success", username: "test user",
    started_at: "2026-10-24T15:00:00Z", finished_at: i === 0 ? null : "2026-10-24T15:00:05Z",
    error_code: null
}));
const second = [{ ...first[0], version: String(v0 - 50n), status: "failed", error_code: "22012" }];
const ids = [
    "history-page", "history-page-status", "history-page-error", "history-workspace", "history-database-name",
    "history-target-name", "history-access", "history-drafts-link", "history-refresh", "history-list-status",
    "history-list-error", "history-list-empty", "history-list-table", "history-list-body", "history-pagination",
    "history-prev", "history-next", "history-page-number", "history-pending-notice", "history-reconcile-panel",
    "history-reconcile", "history-reconcile-status", "history-reconcile-error", "history-reconcile-result",
    "history-reconcile-summary", "history-reconcile-table", "history-reconcile-body", "history-detail",
    "history-detail-status", "history-detail-error", "history-detail-content", "history-detail-sql-section",
    "history-detail-sql", "history-detail-version", "history-detail-execution-id", "history-detail-draft-revision",
    "history-detail-draft-id", "history-detail-result", "history-detail-executor", "history-detail-started",
    "history-detail-finished", "history-detail-error-code", "history-detail-action", "history-detail-target", "history-detail-job", "history-detail-no-job"
];
let generation = 0;
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
const fail = (status, code) => json({ error: { code, message: code } }, status);

test("partial history and reconciliation remain visible without rerunning SQL", async () => {
    const entry = { ...first[0], status: "partial", error_code: "reconciled_partial" };
    assert.equal(parseReconcileResult({ reconciled: [entry] })[0].status, "partial");
    const ui = await setupHistory({ more: false, firstPage: [entry], custom: ({ url }) =>
        url.endsWith("/reconcile") ? json({ reconciled: [entry] }) : undefined });
    assert.equal(ui.$("history-pending-notice").hidden, false);
    assert.match(ui.$("history-list-body").textContent, /Sebagian committed/);
    await ui.$("history-reconcile").fire("click");
    assert.match(ui.$("history-reconcile-summary").textContent, /1 partial/);
    assert.equal(ui.calls.some((call) => call.url.endsWith("/execute")), false);
});

async function setupHistory({ access = "edit", confirm = true, search = "", custom, firstPage = first, more = true } = {}) {
    clearAuthState();
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    const $ = (id) => elements[id];
    $("history-page").dataset.databaseId = db;
    for (const id of ["history-workspace", "history-page-error", "history-list-error", "history-list-empty",
        "history-list-table", "history-pagination", "history-pending-notice", "history-reconcile-panel",
        "history-reconcile-error", "history-reconcile-result", "history-reconcile-table", "history-detail",
        "history-detail-error", "history-detail-content", "history-detail-sql-section", "history-drafts-link"]) $(id).hidden = true;
    const redirects = [], confirms = [], calls = [];
    globalThis.document = { getElementById: (id) => $(id), createElement: (tag) => new Element(tag) };
    globalThis.window = { location: { search, replace: (path) => redirects.push(path) }, confirm: (text) => { confirms.push(text); return confirm; } };
    globalThis.fetch = async (url, opts = {}) => {
        const method = opts.method || "GET";
        calls.push({ url, method, headers: new Headers(opts.headers), credentials: opts.credentials });
        const override = custom?.({ url, method, calls });
        if (override !== undefined) return override;
        if (url === "/api/v1/auth/me") return json({ id: "12", username: "tester", is_platform_admin: false });
        if (url === "/api/v1/auth/csrf") return json({ csrf_token: "phase6-csrf" });
        if (url === "/api/v1/databases") return json({ databases: [{ id: db, name: "Demo", database_name: "target", access }] });
        if (url === `${api}?limit=50`) return json({ migrations: firstPage, has_more: more, next_before: more ? firstPage.at(-1).version : null });
        if (url === `${api}?limit=50&before=${encodeURIComponent(first.at(-1).version)}`) return json({ migrations: second, has_more: false, next_before: null });
        if (url === `${api}/${first[0].version}`) return json({ ...first[0], sql: '<img src=x onerror="alert(1)">;' });
        if (url === `${api}/reconcile` && method === "POST") return json({ reconciled: [{ execution_id: "exec-0", version: first[0].version, status: "success", error_code: null }] });
        throw new Error(`Unexpected ${method} ${url}`);
    };
    await import(`../internal/web/static/js/history.js?phase6=${++generation}`);
    await tick();
    return { $, calls, redirects, confirms };
}

test("cursor validation keeps int64 strings and rejects unsafe numeric cursors", () => {
    assert.equal(positiveInt64("9007199254740993"), true);
    assert.equal(positiveInt64(9007199254740993), false);
    assert.equal(positiveInt64("9223372036854775808"), false);
    const page = parseCursorPage({ drafts: [{ id: "1" }], has_more: true, next_after: db }, "drafts", "next_after");
    assert.equal(page.nextCursor, db);
    assert.throws(() => parseCursorPage({ drafts: [{}], has_more: true, next_after: 123 }, "drafts", "next_after"), /Cursor/);
    assert.throws(() => parseCursorPage({ drafts: [], has_more: true, next_after: "3" }, "drafts", "next_after"), /kosong/);
    assert.throws(() => parseCursorPage({ drafts: [], has_more: false, next_after: "5" }, "drafts", "next_after"), /akhir/);
});

test("reconcile response parser requires real version, status and error_code", () => {
    assert.deepEqual(parseReconcileResult({ reconciled: [] }), []);
    const valid = { execution_id: "", version: "5", status: "unknown", error_code: "receipt_mismatch" };
    assert.deepEqual(parseReconcileResult({ reconciled: [valid] }), [valid]);
    for (const replace of [{ version: 5 }, { status: "committed" }, { error_code: 55 }, { execution_id: null }]) {
        assert.throws(() => parseReconcileResult({ reconciled: [{ ...valid, ...replace }] }), /tidak valid/);
    }
});

test("history list uses cursor and returns to previous page; no numeric precision loss", async () => {
    const ui = await setupHistory();
    assert.equal(ui.$("history-list-body").children.length, 50);
    assert.equal(ui.$("history-page-number").textContent, "1");
    assert.equal(ui.$("history-prev").disabled, true);
    assert.equal(ui.$("history-next").disabled, false);
    assert.match(ui.$("history-list-body").children[0].textContent, /9007199254741199/);
    assert.match(ui.$("history-list-body").children[0].textContent, /exec-0/);
    assert.match(ui.$("history-pending-notice").textContent, /Periksa Jobs untuk status pekerjaan terkini/);
    await ui.$("history-next").fire("click");
    assert.equal(ui.$("history-page-number").textContent, "2");
    assert.equal(ui.$("history-next").disabled, true);
    assert.equal(ui.$("history-prev").disabled, false);
    assert.equal(ui.$("history-list-body").children.length, 1);
    assert.equal(ui.$("history-list-body").children[0].children[4].textContent, "22012");
    await ui.$("history-prev").fire("click");
    assert.equal(ui.$("history-page-number").textContent, "1");
    assert.equal(ui.calls.filter((c) => c.url === `${api}?limit=50`).length, 2);
    await ui.$("history-refresh").fire("click");
    assert.equal(ui.$("history-page-number").textContent, "1");
});

test("history detail loads SQL only after click and renders hostile strings as text", async () => {
    const ui = await setupHistory({ more: false });
    assert.equal(ui.$("history-detail-sql").textContent, "");
    await ui.$("history-list-body").children[0].children.at(-1).children[0].fire("click");
    assert.equal(ui.$("history-detail-sql").textContent, '<img src=x onerror="alert(1)">;');
    assert.equal(ui.$("history-detail-execution-id").textContent, "exec-0");
    assert.equal(ui.$("history-detail-draft-revision").textContent, "4");
    assert.equal(ui.$("history-detail-result").textContent, "Tidak pasti");
});

test("view only can read all history but cannot reconcile or open draft", async () => {
    const ui = await setupHistory({ access: "view", more: false });
    assert.equal(ui.$("history-workspace").hidden, false);
    assert.equal(ui.$("history-reconcile-panel").hidden, true);
    assert.equal(ui.$("history-drafts-link").hidden, true);
    await ui.$("history-reconcile").fire("click");
    assert.equal(ui.calls.filter((call) => call.method === "POST").length, 0);
});

test("history deep link opens the exact attempt and links back to its job", async () => {
    const entry = { ...first[0], job_id: "25", action_type: "resume", target_execution_id: "original-attempt" };
    const ui = await setupHistory({ firstPage: [entry], more: false, search: `?version=${entry.version}`,
        custom: ({ url }) => url === `${api}/${entry.version}` ? json({ ...entry, sql: "historical SQL" }) : undefined });
    assert.equal(ui.$("history-detail-version").textContent, entry.version);
    assert.equal(ui.$("history-detail-action").textContent, "resume");
    assert.equal(ui.$("history-detail-target").textContent, "original-attempt");
    assert.equal(ui.$("history-detail-job").href, `/databases/${db}/jobs/25`);
    assert.equal(ui.$("history-list-body").children[0].children[8].children[1].href, `/databases/${db}/jobs/25`);
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
});

test("reconcile requires confirmation and POST with CSRF; refreshes history; never executes SQL", async () => {
    const denied = await setupHistory({ confirm: false, more: false });
    await denied.$("history-reconcile").fire("click");
    assert.equal(denied.calls.some((c) => c.method === "POST"), false);
    const ui = await setupHistory({ more: false });
    await ui.$("history-reconcile").fire("click");
    assert.equal(ui.confirms.length, 1);
    assert.match(ui.confirms[0], /SQL tidak dijalankan ulang/);
    const post = ui.calls.filter((c) => c.method === "POST");
    assert.equal(post.length, 1);
    assert.equal(post[0].url, `${api}/reconcile`);
    assert.equal(post[0].headers.get("X-CSRF-Token"), "phase6-csrf");
    assert.equal(post[0].credentials, "same-origin");
    assert.match(ui.$("history-reconcile-summary").textContent, /1 eksekusi diperiksa/);
    assert.equal(ui.$("history-reconcile-body").children[0].children[0].textContent, first[0].version);
    assert.equal(ui.calls.filter((c) => c.url === `${api}?limit=50`).length, 2);
});

test("editor can't reconcile after grant is downgraded on refresh check", async () => {
    let databaseReads = 0;
    const ui = await setupHistory({ more: false, custom: ({ url }) => {
        if (url !== "/api/v1/databases") return undefined;
        databaseReads++;
        return json({ databases: [{ id: db, name: "Demo", database_name: "target", access: databaseReads === 1 ? "edit" : "view" }] });
    } });
    await ui.$("history-reconcile").fire("click");
    assert.equal(ui.calls.filter((c) => c.method === "POST").length, 0);
    assert.equal(ui.$("history-workspace").hidden, true);
    assert.match(ui.$("history-page-error").textContent, /dicabut/);
    assert.equal(ui.$("history-pagination").hidden, true);
});

test("403 list hides data; 401 redirects; 409 and 503 reconcile failures do not masquerade as success", async () => {
    const revoked = await setupHistory({ custom: ({ url }) => url === `${api}?limit=50` ? fail(403, "forbidden") : undefined });
    assert.equal(revoked.$("history-workspace").hidden, true);
    assert.equal(revoked.$("history-list-body").children.length, 0);
    const expired = await setupHistory({ custom: ({ url }) => url === `${api}?limit=50` ? fail(401, "unauthenticated") : undefined });
    assert.deepEqual(expired.redirects, ["/login"]);
    for (const [status, code, pattern] of [[409, "migration_busy", /masih berjalan/], [503, "target_unavailable", /tidak dapat dijangkau/]]) {
        const ui = await setupHistory({ more: false, custom: ({ url }) => url === `${api}/reconcile` ? fail(status, code) : undefined });
        await ui.$("history-reconcile").fire("click");
        assert.match(ui.$("history-reconcile-error").textContent, pattern);
        assert.equal(ui.$("history-reconcile-result").hidden, true);
        assert.equal(ui.calls.filter((c) => c.method === "POST").length, 1);
    }
});

const draftIds = [
    "draft-page", "draft-page-status", "draft-page-error", "draft-workspace", "draft-database-name",
    "draft-target-name", "draft-access", "draft-list-status", "draft-list-error", "draft-list-empty",
    "draft-list-table", "draft-list-body", "draft-new", "draft-pagination", "draft-prev", "draft-next",
    "draft-page-number", "draft-form", "draft-editor-heading", "draft-current-id", "draft-title",
    "draft-sql", "draft-title-error", "draft-sql-error", "draft-save", "draft-save-message", "draft-detail",
    "draft-detail-author", "draft-detail-created", "draft-detail-updated", "draft-detail-sql",
    "draft-execution", "draft-execute", "draft-reload", "draft-execution-revision", "draft-execution-state",
    "draft-explain", "draft-explain-message", "draft-explain-result",
    "draft-execution-message", "draft-execution-result", "draft-result-status", "draft-result-version",
    "draft-result-id", "draft-result-error"
];
let draftGeneration = 0;
const draftsAPI = `/api/v1/databases/${db}/drafts`;
const draftsPage1 = Array.from({ length: 50 }, (_, i) => ({
    id: String(v0 + BigInt(i)), database_id: db, title: `Draft ${i}`,
    revision: "1", sql: "SELECT 1", created_by_username: "tester",
    updated_at: "2026-10-24T15:00:00Z"
}));
async function setupDrafts({ custom } = {}) {
    clearAuthState();
    const els = Object.fromEntries(draftIds.map((id) => [id, new Element()]));
    const $ = (id) => els[id];
    $("draft-page").dataset.databaseId = db;
    for (const id of ["draft-workspace", "draft-page-error", "draft-list-error", "draft-list-empty", "draft-list-table", "draft-pagination",
        "draft-detail", "draft-execution", "draft-execution-result", "draft-save-message", "draft-title-error", "draft-sql-error"]) $(id).hidden = true;
    $("draft-form").reset = () => { $("draft-title").value = ""; $("draft-sql").value = ""; };
    const calls = [];
    globalThis.window = { location: { replace() {} }, addEventListener() {}, confirm: () => true };
    globalThis.document = { getElementById: (id) => $(id), createElement: (tag) => new Element(tag) };
    globalThis.fetch = async (url, options = {}) => {
        calls.push({ url, method: options.method ?? "GET" });
        const intercept = custom?.({ url, method: options.method ?? "GET" });
        if (intercept !== undefined) return intercept;
        if (url === "/api/v1/auth/me") return json({ id: "11", username: "tester", is_platform_admin: false });
        if (url === "/api/v1/databases") return json({ databases: [{ id: db, name: "Demo", database_name: "target", access: "edit" }] });
        if (url === `${draftsAPI}?limit=50`) return json({ drafts: draftsPage1, has_more: true, next_after: draftsPage1.at(-1).id });
        if (url === `${draftsAPI}?limit=50&after=${draftsPage1.at(-1).id}`) return json({ drafts: [{ ...draftsPage1[0], id: String(v0 + 50n) }], has_more: false, next_after: null });
        throw new Error(`Unexpected ${url}`);
    };
    await import(`../internal/web/static/js/drafts.js?phase6=${++draftGeneration}`);
    await tick();
    return { $, calls };
}

test("draft pages follow next_after cursor and retain exact string IDs, including previous page", async () => {
    const ui = await setupDrafts();
    assert.equal(ui.$("draft-list-body").children.length, 50);
    assert.equal(ui.$("draft-next").disabled, false);
    await ui.$("draft-next").fire("click");
    assert.equal(ui.$("draft-page-number").textContent, "2");
    assert.equal(ui.$("draft-list-body").children.length, 1);
    assert.equal(ui.$("draft-list-body").children[0].children[0].textContent, String(v0 + 50n));
    assert.equal(ui.$("draft-next").disabled, true);
    await ui.$("draft-prev").fire("click");
    assert.equal(ui.$("draft-list-body").children.length, 50);
    assert.equal(ui.$("draft-page-number").textContent, "1");
});

test("draft pagination errors are displayed without opening imaginary rows", async () => {
    const ui = await setupDrafts({ custom: ({ url }) => url === `${draftsAPI}?limit=50`
        ? json({ drafts: draftsPage1, has_more: true, next_after: 9007199254740993 }) : undefined });
    assert.match(ui.$("draft-list-error").textContent, /Cursor/);
    assert.equal(ui.$("draft-next").disabled, true);
});
