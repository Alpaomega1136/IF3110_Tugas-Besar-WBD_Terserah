import test from "node:test";
import assert from "node:assert/strict";
import { normalizeDraftDefinition, draftEditorValues, validateDraftEditor, identicalDraftDefinition } from "../internal/web/static/js/migration-form.js";

const fields = { title: "Tambah kolom", sql: "ALTER TABLE demo ADD COLUMN age INT;", sqlDown: "ALTER TABLE demo DROP COLUMN age;", irreversible: false, reason: "" };

test("form reversible menyimpan SQL Up dan SQL Down yang eksplisit", () => {
    const { errors, payload } = validateDraftEditor(fields, "4");
    assert.deepEqual(errors, {});
    assert.deepEqual(payload, { title: fields.title, sql: fields.sql, sql_down: fields.sqlDown,
        irreversible: false, irreversible_reason: null, revision: "4" });
});

test("form irreversible mewajibkan alasan dan tidak mengirim SQL Down", () => {
    const invalid = validateDraftEditor({ ...fields, irreversible: true });
    assert.match(invalid.errors.reason, /Alasan/);
    const valid = validateDraftEditor({ ...fields, irreversible: true, reason: "Tidak ada backup" });
    assert.deepEqual(valid.errors, {});
    assert.equal(valid.payload.sql_down, null);
    assert.equal(valid.payload.irreversible_reason, "Tidak ada backup");
});

test("validasi menangani SQL Down kosong, byte UTF-8, JSON dan NUL", () => {
    assert.match(validateDraftEditor({ ...fields, sqlDown: "" }).errors.sqlDown, /wajib/);
    assert.match(validateDraftEditor({ ...fields, sqlDown: "€".repeat(16385) }).errors.sqlDown, /48 KiB/);
    assert.match(validateDraftEditor({ ...fields, sql: "€".repeat(16385) }).errors.sql, /48 KiB/);
    assert.match(validateDraftEditor({ ...fields, irreversible: true, reason: "€".repeat(167) }).errors.reason, /500 byte/);
    assert.match(validateDraftEditor({ ...fields, title: "test\0" }).errors.title, /judul/i);
    const largeJSON = validateDraftEditor({ ...fields, sql: "\"".repeat(32_700), sqlDown: "\"".repeat(32_700) });
    assert.match(largeJSON.errors.body, /64 KiB/);
});

test("definisi dari backend divalidasi dan perubahan draft termasuk tipe migration", () => {
    const draft = { title: "A", sql: "SELECT 1", sql_down: "SELECT 2", irreversible: false, irreversible_reason: null };
    assert.equal(normalizeDraftDefinition(draft), draft);
    assert.throws(() => normalizeDraftDefinition({ ...draft, irreversible: "no" }), /tidak valid/);
    assert.deepEqual(draftEditorValues(draft), { title: "A", sql: "SELECT 1", sqlDown: "SELECT 2", irreversible: false, reason: "" });
    assert.ok(identicalDraftDefinition(draft, { ...draft }));
    assert.equal(identicalDraftDefinition(draft, { ...draft, sql_down: "SELECT 3" }), false);
});
