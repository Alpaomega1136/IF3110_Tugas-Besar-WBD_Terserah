import test from "node:test";
import assert from "node:assert/strict";
import { compareSchemas, initializeSchemaDiff } from "../internal/web/static/js/schema-diff.js";
import { Element, tick } from "./mock-dom.mjs";

const column = (name, data_type = "text", nullable = true) => ({ name, data_type, nullable, default: null, identity: "", generated: "" });
const table = (name, columns, extra = {}) => ({ name, kind: "table", columns, has_constraints: false, has_indexes: false, ...extra });

test("generates quoted SQL for new tables and nullable columns", () => {
    const before = [table('people', [column('name')])];
    const after = [table('people', [column('name'), column('new"column', 'integer')]), table('new"table', [column('name', 'text', false)])];
    const result = compareSchemas('target"schema', before, after);
    assert.match(result.sql, /ALTER TABLE "target""schema"\."people" ADD COLUMN "new""column" integer;/);
    assert.match(result.sql, /CREATE TABLE "target""schema"\."new""table" \(\n    "name" text NOT NULL\n\);/);
    assert.equal(result.changes.length, 2);
});

test("reports unsupported and potentially destructive changes without generating them", () => {
    const before = [table('people', [column('old'), column('id', 'integer')]), table('obsolete', [column('id')])];
    const after = [table('people', [column('id', 'bigint'), column('required', 'text', false), column('evil', 'text; DROP TABLE users')], { has_constraints: true })];
    const result = compareSchemas('public', before, after);
    assert.equal(result.sql, "");
    assert.match(result.warnings.join('\n'), /obsolete/);
    assert.match(result.warnings.join('\n'), /people\.old/);
    assert.match(result.warnings.join('\n'), /people\.id/);
    assert.match(result.warnings.join('\n'), /people\.required/);
    assert.match(result.warnings.join('\n'), /Constraint dan index/);
});

test("compares two 100-table schemas with two requests and inserts SQL only on demand", async () => {
    const ids = ["diff-current", "diff-desired", "diff-compare", "diff-use", "diff-status", "diff-result"];
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    globalThis.document = { getElementById: (id) => elements[id], createElement: (tag) => new Element(tag) };
    const common = Array.from({ length: 99 }, (_, i) => table("table_" + i, [column("id")]));
    const requests = [];
    globalThis.fetch = async (url) => {
        const path = String(url);
        requests.push(path);
        let result;
        if (path.endsWith('/schemas')) result = { schemas: ['current', 'desired'] };
        else if (path.endsWith('/current/structure')) result = { tables: [...common, table('old', [])] };
        else if (path.endsWith('/desired/structure')) result = { tables: [...common, table('items', [column('name')])] };
        else throw new Error(`Unexpected request ${path}`);
        return new Response(JSON.stringify(result), { status: 200 });
    };
    let inserted = null;
    initializeSchemaDiff('1', (sql) => { inserted = sql; });
    await tick();
    elements['diff-current'].value = 'current';
    elements['diff-desired'].value = 'desired';
    await elements['diff-compare'].fire('click');
    assert.deepEqual(requests, ['/api/v1/databases/1/schemas', '/api/v1/databases/1/schemas/current/structure', '/api/v1/databases/1/schemas/desired/structure']);
    assert.match(elements['diff-result'].textContent, /CREATE TABLE "current"\."items"/);
    assert.equal(inserted, null);
    await elements['diff-use'].fire('click');
    assert.match(inserted, /CREATE TABLE "current"\."items"/);
    elements['diff-desired'].value = 'current';
    await elements['diff-desired'].fire('change');
    assert.equal(elements['diff-use'].disabled, true);
});
