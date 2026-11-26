import test from "node:test";
import assert from "node:assert/strict";
import { Element, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";
import { parseExecutionResult, parseJobResponse, executionFeedback, executeErrorMessage } from "../internal/web/static/js/execution.js";

const databaseID = "9007199254740993";
const draftID = "9007199254740995";
const base = `/api/v1/databases/${databaseID}/drafts`;
const draft = {
    id: draftID, database_id: databaseID, revision: "7", title: "Create demo",
    sql: 'CREATE TABLE example(id int);', sql_down: 'DROP TABLE example;', irreversible: false,
    irreversible_reason: null, immutable: false, created_by_username: "editor",
    created_at: "2026-10-24T15:00:00Z", updated_at: "2026-10-24T15:00:00Z"
};
let run = 0;

function reply(body, status = 200) {
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}
function apiError(status, code) {
    return reply({ error: { code, message: code } }, status);
}

async function setup({ access = "edit", selected = true, confirm = true, backend, freshDraft = draft } = {}) {
    clearAuthState();
    const ids = [
        "draft-page", "draft-page-status", "draft-page-error", "draft-workspace", "draft-database-name",
        "draft-target-name", "draft-access", "draft-list-status", "draft-list-error", "draft-list-empty",
        "draft-list-table", "draft-list-body", "draft-new", "draft-pagination", "draft-prev", "draft-next", "draft-page-number", "draft-form", "draft-editor-heading",
        "draft-current-id", "draft-title", "draft-sql", "draft-sql-down", "draft-irreversible", "draft-reason",
        "draft-down-group", "draft-reason-group", "draft-immutable-note", "draft-title-error", "draft-sql-error",
        "draft-sql-down-error", "draft-reason-error",
        "draft-save", "draft-save-message", "draft-detail", "draft-detail-author", "draft-detail-created",
        "draft-detail-updated", "draft-detail-sql", "draft-detail-down", "draft-detail-down-section", "draft-detail-irreversible", "draft-execution", "draft-execute", "draft-reload",
        "draft-explain", "draft-explain-message", "draft-explain-result",
        "draft-execution-revision", "draft-execution-state", "draft-execution-message",
        "draft-execution-result", "draft-result-status", "draft-result-version", "draft-result-id", "draft-result-error", "draft-job-link"
    ];
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    const $ = (id) => elements[id];
    $("draft-page").dataset.databaseId = databaseID;
    for (const id of ["draft-page-error", "draft-workspace", "draft-list-error", "draft-list-empty", "draft-list-table",
        "draft-title-error", "draft-sql-error", "draft-sql-down-error", "draft-reason-error", "draft-save-message", "draft-detail", "draft-execution",
        "draft-execution-message", "draft-execution-result"]) $(id).hidden = true;
    $("draft-execute").disabled = true;
    $("draft-title").value = "";
    $("draft-sql").value = "";
    $("draft-sql-down").value = "";
    $("draft-irreversible").checked = false;
    $("draft-reason").value = "";
    $("draft-form").reset = () => { $("draft-title").value = ""; $("draft-sql").value = ""; $("draft-sql-down").value = ""; $("draft-reason").value = ""; $("draft-irreversible").checked = false; };
    const redirects = [], confirms = [], calls = [];
    globalThis.window = {
        location: { replace: (path) => redirects.push(path) },
        addEventListener() {},
        confirm: (message) => { confirms.push(message); return confirm; }
    };
    globalThis.document = { getElementById: (id) => $(id), createElement: (tag) => new Element(tag) };
    globalThis.fetch = async (url, options = {}) => {
        const method = options.method ?? "GET";
        const body = options.body ? JSON.parse(options.body) : null;
        calls.push({ url, method, body, credentials: options.credentials, headers: new Headers(options.headers) });
        if (backend) {
            const result = backend({ url, method, body, calls });
            if (result !== undefined) return result;
        }
        if (url === "/api/v1/auth/me") return reply({ id: "11", username: "editor", is_platform_admin: false });
        if (url === "/api/v1/databases") return reply({ databases: [{ id: databaseID, name: "Demo", database_name: "disposable", access }] });
        if (url === "/api/v1/auth/csrf") return reply({ csrf_token: "csrf-value" });
        if (url === base || url === `${base}?limit=50`) return method === "GET" ? reply({ drafts: selected ? [draft] : [], has_more: false, next_after: null }) : reply(draft, 201);
        if (url === `${base}/${draftID}` && method === "GET") return reply(freshDraft);
        if (url === `${base}/${draftID}/execute` && method === "POST") {
            return reply({ execution_id: "a1b2c3d4", version: "9007199254740997", status: "success", error_code: null });
        }
        throw new Error(`Unhandled ${method} ${url}`);
    };
    await import(`../internal/web/static/js/drafts.js?run=${++run}`);
    await tick();
    if (selected && access === "edit") {
        await $("draft-list-body").children[0].children[4].children[0].fire("click");
        await tick();
    }
    return { $, elements, calls, confirms, redirects };
}

const executeCalls = (ui) => ui.calls.filter((call) => call.url.endsWith("/execute"));

test("queued jobs keep string IDs and do not claim a committed migration", () => {
    const job = { id: "9007199254740999", database_id: databaseID, status: "queued", error_code: null };
    assert.deepEqual(parseJobResponse({ job }, databaseID), job);
    for (const changed of [{ id: 9007199254740999 }, { database_id: "99" }, { status: "committed" }, { id: "0" }]) {
        assert.throws(() => parseJobResponse({ job: { ...job, ...changed } }, databaseID), /tidak valid/);
    }
    assert.doesNotMatch(executionFeedback("queued").message, /berhasil di-commit/);
});

test("Execute submits once and polls the job until the worker commits", async () => {
    let polls = 0;
    const job = { id: "9007199254740999", database_id: databaseID, status: "queued", error_code: null };
    const ui = await setup({ backend: ({ url }) => {
        if (url.endsWith("/execute")) return reply({ job, replayed: false }, 202);
        if (url.endsWith(`/jobs/${job.id}`)) {
            polls++;
            return reply({ job: { ...job, status: "success" }, attempts: [{ execution_id: "worker-execution", version: "9" }] });
        }
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 1);
    assert.equal(polls, 1);
    assert.equal(ui.$("draft-result-id").textContent, "worker-execution");
    assert.equal(ui.$("draft-job-link").href, `/databases/${databaseID}/jobs/${job.id}`);
    assert.match(ui.$("draft-execution-message").textContent, /berhasil di-commit/);
});

test("job polling follows recovery and backoff without submitting SQL again", async (t) => {
    const timer = globalThis.setTimeout;
    t.mock.method(globalThis, "setTimeout", (fn, delay, ...args) => timer(fn, delay === 1000 ? 0 : delay, ...args));
    const job = { id: "12", database_id: databaseID, status: "queued", attempt_count: 1, max_attempts: 3,
        available_at: "2026-11-21T10:00:00Z", committed_batches: "1", committed_rows: "2" };
    for (const status of ["recovering", "retry_wait"]) {
        assert.equal(parseJobResponse({ job: { ...job, status } }, databaseID).status, status);
        assert.doesNotMatch(executionFeedback(status).message, /berhasil di-commit|transaksi telah di-rollback/);
    }
    for (const changed of [{ available_at: "bad" }, { available_at: 1 }, { attempt_count: -1 }, { max_attempts: 0 }]) {
        assert.throws(() => parseJobResponse({ job: { ...job, ...changed } }, databaseID), /tidak valid/);
    }
    let polls = 0;
    const states = ["unknown", "recovering", "retry_wait", "running", "success"];
    const ui = await setup({ backend: ({ url }) => {
        if (url.endsWith("/execute")) return reply({ job }, 202);
        if (url.endsWith("/jobs/12")) {
            const status = states[polls++];
            return reply({ job: { ...job, status, attempt_count: polls > 3 ? 2 : 1 },
                attempts: [{ execution_id: "retry-execution", version: "10" }] });
        }
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(polls, 5);
    assert.equal(executeCalls(ui).length, 1);
    assert.equal(ui.$("draft-result-id").textContent, "retry-execution");
    assert.match(ui.$("draft-execution-message").textContent, /berhasil di-commit/);
});

test("job polling failure preserves uncertainty and never resubmits SQL", async () => {
    const ui = await setup({ backend: ({ url }) => {
        if (url.endsWith("/execute")) return reply({ job: { id: "12", database_id: databaseID, status: "queued" } }, 202);
        if (url.endsWith("/jobs/12")) return apiError(503, "jobs_unavailable");
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 1);
    assert.doesNotMatch(ui.$("draft-execution-message").textContent, /berhasil di-commit/);
    assert.match(ui.$("draft-execution-message").textContent, /Jangan retry otomatis/);
});

test("replayed Execute preserves a resumed job handoff instead of claiming SQL success", async () => {
    const job = { id: "12", database_id: databaseID, status: "resumed", resumed_by_job_id: "19",
        committed_batches: "1", committed_rows: "2" };
    assert.equal(parseJobResponse({ job }, databaseID).resumed_by_job_id, "19");
    for (const changed of [{ resumed_by_job_id: null }, { resumed_by_job_id: 19 }, { parent_job_id: "0" }, { target_execution_id: 3 }]) {
        assert.throws(() => parseJobResponse({ job: { ...job, ...changed } }, databaseID), /tidak valid/);
    }
    const ui = await setup({ backend: ({ url }) => {
        if (url.endsWith("/execute")) return reply({ job, replayed: true });
        if (url.endsWith("/jobs/12")) return reply({ job, attempts: [{ execution_id: "old-partial", version: "9" }] });
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 1);
    assert.equal(ui.$("draft-result-status").textContent, "resumed");
    assert.equal(ui.$("draft-job-link").href, `/databases/${databaseID}/jobs/19`);
    assert.match(ui.$("draft-execution-message").textContent, /Job resume: 19/);
    assert.doesNotMatch(ui.$("draft-execution-message").textContent, /berhasil di-commit|tidak valid|transaksi telah di-rollback/);
});

test("partial batch keeps exact committed counts and never claims a full rollback", async () => {
    const job = { id: "12", database_id: databaseID, status: "partial", error_code: "23514",
        committed_batches: "2", committed_rows: "9007199254740993" };
    assert.equal(parseJobResponse({ job }, databaseID).status, "partial");
    for (const bad of [-1, "-1", "9223372036854775808", "1.5"]) {
        assert.throws(() => parseJobResponse({ job: { ...job, committed_rows: bad } }, databaseID), /tidak valid/);
    }
    const ui = await setup({ backend: ({ url }) => {
        if (url.endsWith("/execute")) return reply({ job }, 200);
        if (url.endsWith("/jobs/12")) return reply({ job, attempts: [{ execution_id: "partial-attempt", version: "9" }] });
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 1);
    assert.equal(ui.$("draft-result-status").textContent, "partial");
    assert.match(ui.$("draft-execution-message").textContent, /sebelumnya tetap tersimpan/);
    assert.match(ui.$("draft-execution-message").textContent, /9007199254740993/);
    assert.doesNotMatch(ui.$("draft-execution-message").textContent, /berhasil di-commit|transaksi telah di-rollback/);
});

test("a job finishing on the last poll still displays the committed result", async (t) => {
    const timer = globalThis.setTimeout;
    t.mock.method(globalThis, "setTimeout", (fn, delay, ...args) => timer(fn, delay === 1000 ? 0 : delay, ...args));
    let polls = 0;
    const job = { id: "12", database_id: databaseID, status: "queued", error_code: null };
    const ui = await setup({ backend: ({ url }) => {
        if (url.endsWith("/execute")) return reply({ job }, 202);
        if (url.endsWith("/jobs/12")) {
            polls++;
            return reply({ job: { ...job, status: polls === 30 ? "success" : "running" },
                attempts: [{ execution_id: "last-poll-execution", version: "9" }] });
        }
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(polls, 30);
    assert.equal(executeCalls(ui).length, 1);
    assert.match(ui.$("draft-execution-message").textContent, /berhasil di-commit/);
});

test("BE2 Result requires status, string version, execution ID and valid error_code", () => {
    const valid = { execution_id: "id-1", version: "9007199254740997", status: "unknown", error_code: null };
    assert.deepEqual(parseExecutionResult(valid), valid);
    for (const changed of [ { version: 9007199254740997 }, { version: "9223372036854775808" }, { version: "0" },
        { status: "committed" }, { execution_id: null }, { error_code: 7 } ]) {
        assert.throws(() => parseExecutionResult({ ...valid, ...changed }), /tidak valid/);
    }
    for (const status of ["success", "failed", "running", "unknown"]) assert.match(executionFeedback(status).message, /Migration|migration|eksekusi/);
});

test("draft cannot execute before save; creates draft without implicit execute", async () => {
    const ui = await setup({ selected: false });
    assert.equal(ui.$("draft-execution").hidden, true);
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 0);
    ui.$("draft-title").value = "Create demo";
    ui.$("draft-sql").value = "CREATE TABLE example(id int);";
    ui.$("draft-sql-down").value = "DROP TABLE example;";
    await ui.$("draft-form").fire("submit");
    assert.equal(ui.calls.filter((c) => c.method === "POST" && c.url === base).length, 1);
    assert.equal(executeCalls(ui).length, 0);
    assert.equal(ui.$("draft-execution").hidden, false);
});

test("draft create and update enforce the 48 KiB SQL limit in UTF-8 bytes", async () => {
    const prefix = "SELECT 1; --";
    const limit = 48 * 1024;
    for (const selected of [false, true]) {
        for (const extra of [0, 1]) {
            const ui = await setup({ selected });
            const sql = prefix + "€".repeat(Math.floor((limit - prefix.length) / 3)) +
                "a".repeat((limit - prefix.length) % 3 + extra);
            ui.$("draft-title").value = "SQL limit";
            ui.$("draft-sql").value = sql;
            if (!selected) ui.$("draft-sql-down").value = "SELECT 1;";
            await ui.$("draft-form").fire("submit");
            const saves = ui.calls.filter((call) => ["POST", "PATCH"].includes(call.method));
            assert.equal(saves.length, extra === 0 ? 1 : 0);
            if (extra === 0) {
                assert.equal(saves[0].method, selected ? "PATCH" : "POST");
                assert.equal(saves[0].body.sql, sql);
            } else {
                assert.equal(ui.$("draft-sql-error").hidden, false);
                assert.match(ui.$("draft-sql-error").textContent, /48 KiB/);
            }
        }
    }
});

test("draft create and update enforce 64 KiB serialized JSON independently of SQL size", async () => {
    const encoder = new TextEncoder();
    const limit = 64 * 1024;
    const title = "JSON limit";
    for (const selected of [false, true]) {
        const start = "SELECT '" + '"'.repeat(32000);
        const payload = (sql) => ({ title, sql,
            sql_down: selected ? draft.sql_down : "SELECT 1;", irreversible: false, irreversible_reason: null,
            ...(selected ? { revision: draft.revision } : {}) });
        const padding = limit - encoder.encode(JSON.stringify(payload(start + "'"))).length;
        for (const extra of [0, 1]) {
            const ui = await setup({ selected });
            const sql = start + "a".repeat(padding + extra) + "'";
            assert.ok(encoder.encode(sql).length < 48 * 1024);
            assert.equal(encoder.encode(JSON.stringify(payload(sql))).length, limit + extra);
            ui.$("draft-title").value = title;
            ui.$("draft-sql").value = sql;
            if (!selected) ui.$("draft-sql-down").value = "SELECT 1;";
            await ui.$("draft-form").fire("submit");
            const saves = ui.calls.filter((call) => ["POST", "PATCH"].includes(call.method));
            assert.equal(saves.length, extra === 0 ? 1 : 0);
            if (extra === 0) {
                assert.deepEqual(saves[0].body, payload(sql));
            } else {
                assert.equal(ui.$("draft-sql-error").hidden, false);
                assert.match(ui.$("draft-sql-error").textContent, /64 KiB/);
            }
        }
    }
});

test("saved draft preflight, explicit confirmation, exact string revision and success", async () => {
    const ui = await setup();
    assert.equal(ui.$("draft-execution").hidden, false);
    assert.equal(ui.$("draft-execute").disabled, false);
    assert.equal(ui.$("draft-execution-revision").textContent, "7");
    assert.equal(ui.$("draft-detail-sql").textContent, draft.sql);
    await ui.$("draft-execute").fire("click");
    assert.equal(ui.confirms.length, 1);
    assert.match(ui.confirms[0], /disposable/);
    assert.match(ui.confirms[0], /revisi 7/);
    assert.equal(executeCalls(ui).length, 1);
    assert.deepEqual(executeCalls(ui)[0].body, { revision: "7" });
    assert.equal(executeCalls(ui)[0].credentials, "same-origin");
    assert.equal(executeCalls(ui)[0].headers.get("X-CSRF-Token"), "csrf-value");
    assert.equal(ui.$("draft-result-status").textContent, "success");
    assert.equal(ui.$("draft-result-version").textContent, "9007199254740997");
    assert.equal(ui.$("draft-result-id").textContent, "a1b2c3d4");
    assert.match(ui.$("draft-execution-message").textContent, /di-commit/);
    assert.equal(ui.$("draft-execute").disabled, true);
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 1);
});

test("cancel confirmation does not send SQL", async () => {
    const ui = await setup({ confirm: false });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 0);
    assert.match(ui.$("draft-execution-message").textContent, /dibatalkan/);
    assert.equal(ui.$("draft-execute").disabled, false);
});

test("unsaved edits disable execution and allow re-enable after restoring values", async () => {
    const ui = await setup();
    ui.$("draft-sql").value = "DROP TABLE example;";
    await ui.$("draft-sql").fire("input");
    assert.equal(ui.$("draft-execute").disabled, true);
    assert.match(ui.$("draft-execution-state").textContent, /belum disimpan/);
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 0);
    ui.$("draft-sql").value = draft.sql;
    await ui.$("draft-sql").fire("input");
    assert.equal(ui.$("draft-execute").disabled, false);
});

test("server revision changes before execute: blocks POST, offers reload", async () => {
    let reads = 0;
    const ui = await setup({ backend: ({ url, method }) => {
        if (url === `${base}/${draftID}` && method === "GET") {
            reads++;
            return reply(reads === 1 ? draft : { ...draft, revision: "8", sql: "SELECT 1;" });
        }
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 0);
    assert.equal(ui.confirms.length, 0);
    assert.match(ui.$("draft-execution-message").textContent, /sudah berubah/);
    assert.equal(ui.$("draft-execute").disabled, true);
    await ui.$("draft-reload").fire("click");
    await tick();
    assert.equal(ui.$("draft-execution-revision").textContent, "8");
    assert.equal(ui.$("draft-detail-sql").textContent, "SELECT 1;");
});

test("202 running/unknown and failed are never reported as commit", async () => {
    for (const status of ["running", "unknown", "failed"]) {
        const ui = await setup({ backend: ({ url }) => url.endsWith("/execute") ? reply({ execution_id: "abc", version: "8", status, error_code: status === "failed" ? "23505" : null }, status === "failed" ? 200 : 202) : undefined });
        await ui.$("draft-execute").fire("click");
        assert.equal(ui.$("draft-result-status").textContent, status);
        assert.equal(ui.$("draft-execute").disabled, true);
        assert.equal(ui.$("draft-result-error").textContent, status === "failed" ? "23505" : "-");
        assert.doesNotMatch(ui.$("draft-execution-message").textContent, /berhasil di-commit/);
        assert.match(ui.$("draft-execution-message").textContent, status === "failed" ? /rollback/ : /History/);
    }
});

test("403 revoked grant hides workspace and cannot execute again", async () => {
    const ui = await setup({ backend: ({ url }) => url.endsWith("/execute") ? apiError(403, "forbidden") : undefined });
    await ui.$("draft-execute").fire("click");
    assert.equal(ui.$("draft-workspace").hidden, true);
    assert.match(ui.$("draft-page-error").textContent, /dicabut/);
    assert.equal(executeCalls(ui).length, 1);
});

test("401 expired session redirects to login", async () => {
    const ui = await setup({ backend: ({ url }) => url.endsWith("/execute") ? apiError(401, "unauthenticated") : undefined });
    await ui.$("draft-execute").fire("click");
    assert.deepEqual(ui.redirects, ["/login"]);
});

test("409 provisioning, revision conflict, busy, identity failures are specific", async () => {
    const cases = [
        ["execution_conflict", /Buka ulang draft/], ["migration_busy", /sedang menjalankan/],
        ["target_history_unprotected", /provision_target_history.sql/], ["target_identity_conflict", /Identitas database/]
    ];
    for (const [code, pattern] of cases) {
        const ui = await setup({ backend: ({ url }) => url.endsWith("/execute") ? apiError(409, code) : undefined });
        await ui.$("draft-execute").fire("click");
        assert.match(ui.$("draft-execution-message").textContent, pattern);
        assert.equal(ui.$("draft-execute").disabled, true);
        assert.equal(executeCalls(ui).length, 1);
    }
});

test("503 or network outage leaves result uncertain and does not retry", async () => {
    for (const mode of ["503", "network"]) {
        const ui = await setup({ backend: ({ url }) => url.endsWith("/execute") ? (mode === "503" ? apiError(503, "migration_unavailable") : Promise.reject(new Error("offline"))) : undefined });
        await ui.$("draft-execute").fire("click");
        assert.match(ui.$("draft-execution-message").textContent, /Jangan retry otomatis/);
        assert.equal(ui.$("draft-execute").disabled, true);
        assert.equal(ui.$("draft-execution-result").hidden, true);
        await ui.$("draft-execute").fire("click");
        assert.equal(executeCalls(ui).length, 1);
    }
});

test("view only cannot list drafts or attempt execute", async () => {
    const ui = await setup({ access: "view" });
    assert.equal(ui.$("draft-workspace").hidden, true);
    assert.equal(ui.$("draft-execution").hidden, true);
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 0);
});

test("prevent double POST while first execution is outstanding", async () => {
    let release;
    const gate = new Promise((resolve) => { release = resolve; });
    const ui = await setup({ backend: ({ url }) => url.endsWith("/execute") ? gate : undefined });
    const first = ui.$("draft-execute").fire("click");
    const second = ui.$("draft-execute").fire("click");
    await tick();
    assert.equal(executeCalls(ui).length, 1);
    release(reply({ execution_id: "abc", version: "8", status: "success", error_code: null }));
    await Promise.all([first, second]);
    assert.equal(executeCalls(ui).length, 1);
});

test("specific error guidance does not promise success on 409/503", () => {
    assert.match(executeErrorMessage({ status: 409, code: "target_history_unprotected" }), /restricted/);
    assert.match(executeErrorMessage({ status: 409, code: "target_history_unprotected" }), /khusus database ini/);
    assert.match(executeErrorMessage({ status: 503, code: "migration_unavailable" }), /belum pasti/);
    assert.match(executeErrorMessage({ status: 403, code: "csrf_rejected" }), /CSRF/);
});


test("form baru menyimpan reversible dengan SQL Down", async () => {
    const ui = await setup({ selected: false });
    ui.$("draft-title").value = "Tambah kolom";
    ui.$("draft-sql").value = "ALTER TABLE demo ADD COLUMN age INT;";
    ui.$("draft-sql-down").value = "ALTER TABLE demo DROP COLUMN age;";
    await ui.$("draft-form").fire("submit");
    const call = ui.calls.find((c) => c.url === base && c.method === "POST");
    assert.ok(call);
    assert.equal(call.body.sql_down, "ALTER TABLE demo DROP COLUMN age;");
    assert.equal(call.body.irreversible, false);
    assert.equal(call.body.irreversible_reason, null);
});

test("form irreversible wajib alasan dan tidak menyimpan SQL Down", async () => {
    const ui = await setup({ selected: false });
    ui.$("draft-title").value = "Hapus data";
    ui.$("draft-sql").value = "DELETE FROM demo;";
    ui.$("draft-irreversible").checked = true;
    await ui.$("draft-irreversible").fire("change");
    assert.equal(ui.$("draft-down-group").hidden, true);
    assert.equal(ui.$("draft-reason-group").hidden, false);
    await ui.$("draft-form").fire("submit");
    assert.equal(ui.calls.some((c) => c.method === "POST" && c.url === base), false);
    ui.$("draft-reason").value = "Tidak ada backup data";
    await ui.$("draft-form").fire("submit");
    const call = ui.calls.find((c) => c.method === "POST" && c.url === base);
    assert.equal(call.body.sql_down, null);
    assert.equal(call.body.irreversible, true);
    assert.equal(call.body.irreversible_reason, "Tidak ada backup data");
});

test("draft immutable ditampilkan read-only dan tidak bisa disimpan ulang", async () => {
    const ui = await setup({ freshDraft: { ...draft, immutable: true } });
    assert.equal(ui.$("draft-title").disabled, true);
    assert.equal(ui.$("draft-sql-down").disabled, true);
    assert.equal(ui.$("draft-save").disabled, true);
    assert.equal(ui.$("draft-execute").disabled, true);
    assert.equal(ui.$("draft-immutable-note").hidden, false);
    await ui.$("draft-form").fire("submit");
    assert.equal(ui.calls.filter((c) => c.method === "PATCH").length, 0);
});

test("perubahan SQL Down pada server mencegah execute memakai draft basi", async () => {
    let reads = 0;
    const ui = await setup({ backend: ({ url, method }) => {
        if (url === `${base}/${draftID}` && method === "GET") {
            reads++;
            return reply(reads === 1 ? draft : { ...draft, sql_down: "SELECT 99;" });
        }
        return undefined;
    } });
    await ui.$("draft-execute").fire("click");
    assert.equal(executeCalls(ui).length, 0);
    assert.match(ui.$("draft-execution-message").textContent, /sudah berubah/);
});
