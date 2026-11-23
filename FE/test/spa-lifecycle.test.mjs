import test from "node:test";
import assert from "node:assert/strict";

let run = 0;
async function router({ allow = true, status = 200 } = {}) {
    const events = [], current = { replaceWith() { events.push("replace"); } };
    globalThis.window = {
        location: { origin: "http://localhost:3000", pathname: "/dashboard", search: "", assign: () => events.push("fallback") },
        history: { pushState: () => events.push("push"), replaceState() {} }, scrollTo() {}, addEventListener() {},
        __spaBeforeNavigate: async () => allow,
        __spaCleanup: () => events.push("cleanup")
    };
    globalThis.document = { title: "old", querySelector: () => current, addEventListener() {} };
    globalThis.DOMParser = class { parseFromString() { return { title: "Jobs", querySelector: () => ({}), querySelectorAll: () => [] }; } };
    globalThis.fetch = async () => { events.push("fetch"); return new Response("page", { status }); };
    const module = await import(`../internal/web/static/js/spa.js?test=${++run}`);
    return { events, navigate: module.spaNavigate };
}

test("successful SPA navigation cleans up polling before replacing the old root", async () => {
    const r = await router();
    assert.equal(await r.navigate("/databases/1/jobs"), true);
    assert.deepEqual(r.events, ["fetch", "cleanup", "replace", "push"]);
    assert.equal(window.__spaCleanup, null);
});

test("cancelled navigation leaves the current page and its polling intact", async () => {
    const r = await router({ allow: false });
    assert.equal(await r.navigate("/databases/1/jobs"), false);
    assert.deepEqual(r.events, []);
    assert.equal(typeof window.__spaCleanup, "function");
});

test("failed page fetch preserves cleanup until browser fallback unloads the page", async () => {
    const r = await router({ status: 503 });
    assert.equal(await r.navigate("/databases/1/jobs"), false);
    assert.deepEqual(r.events, ["fetch", "fallback"]);
});
