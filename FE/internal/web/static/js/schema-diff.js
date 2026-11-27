import { authenticatedRequest } from "./auth.js";

const quote = (name) => `"${name.replaceAll('"', '""')}"`;
// shortcut: only built-in scalar types are generated; extend after supporting their PostgreSQL semantics.
const safeType = /^(smallint|integer|bigint|boolean|text|date|uuid|real|double precision|character varying(?:\([1-9]\d{0,5}\))?|character(?:\([1-9]\d{0,5}\))?|numeric(?:\([1-9]\d{0,3}(?:,\s*\d{1,3})?\))?|timestamp(?:\([0-6]\))? (?:with|without) time zone|time(?:\([0-6]\))? (?:with|without) time zone)$/;

function simple(column) {
    return typeof column.name === "string" && typeof column.data_type === "string" &&
        safeType.test(column.data_type) && column.default == null && !column.identity && !column.generated;
}

export function compareSchemas(currentName, current, desired) {
    const changes = [], warnings = [], statements = [];
    const currentTables = new Map(current.map((table) => [table.name, table]));
    const desiredNames = new Set(desired.map((table) => table.name));
    for (const table of current) {
        if (!desiredNames.has(table.name)) warnings.push(`Tabel ${table.name} hanya ada pada schema yang akan diubah; penghapusan tidak dibuat.`);
    }
    for (const table of desired) {
        const existing = currentTables.get(table.name);
        const qualified = `${quote(currentName)}.${quote(table.name)}`;
        const fields = new Map((existing?.columns || []).map((column) => [column.name, column]));
        const desiredFields = new Set(table.columns.map((column) => column.name));
        if (table.kind !== "table" || (existing && existing.kind !== "table")) {
            warnings.push(`Tabel ${table.name} bukan tabel biasa; tinjau manual.`);
            continue;
        }
        if (table.has_constraints || table.has_indexes || existing?.has_constraints || existing?.has_indexes) {
            warnings.push(`Constraint dan index pada ${table.name} tidak dibandingkan atau dibuat; tinjau manual.`);
        }
        if (!existing) {
            changes.push(`Tabel baru: ${table.name}`);
            if (!table.columns.length || !table.columns.every(simple)) {
                warnings.push(`Tabel ${table.name} memiliki kolom yang belum didukung; SQL tabel tidak dibuat.`);
                continue;
            }
            const columns = table.columns.map((c) => `    ${quote(c.name)} ${c.data_type}${c.nullable ? "" : " NOT NULL"}`);
            statements.push(`CREATE TABLE ${qualified} (\n${columns.join(",\n")}\n);`);
            continue;
        }
        for (const column of existing.columns) {
            if (!desiredFields.has(column.name)) warnings.push(`Kolom ${table.name}.${column.name} hanya ada pada schema yang akan diubah; penghapusan tidak dibuat.`);
        }
        for (const column of table.columns) {
            const old = fields.get(column.name);
            if (old) {
                if (["data_type", "nullable", "default", "identity", "generated"].some((key) => old[key] !== column[key])) {
                    warnings.push(`Kolom ${table.name}.${column.name} berbeda; perubahan kolom tidak dibuat.`);
                }
                continue;
            }
            changes.push(`Kolom baru: ${table.name}.${column.name}`);
            if (!simple(column) || !column.nullable) {
                warnings.push(`Kolom ${table.name}.${column.name} tidak dibuat otomatis: tipe/atribut tidak didukung atau NOT NULL tanpa default.`);
                continue;
            }
            statements.push(`ALTER TABLE ${qualified} ADD COLUMN ${quote(column.name)} ${column.data_type};`);
        }
    }
    return { changes, warnings, sql: statements.join("\n\n") };
}

async function readSchema(base, schema) {
    const result = await authenticatedRequest(`${base}/schemas/${encodeURIComponent(schema)}/structure`);
    if (!Array.isArray(result?.tables) || !result.tables.every((table) =>
        typeof table?.name === "string" && typeof table.kind === "string" && Array.isArray(table.columns) &&
        typeof table.has_constraints === "boolean" && typeof table.has_indexes === "boolean")) {
        throw new Error("Metadata schema tidak lengkap.");
    }
    return result.tables;
}

export function initializeSchemaDiff(databaseId, useSQL) {
    const current = document.getElementById("diff-current");
    if (!current) return;
    const desired = document.getElementById("diff-desired");
    const compare = document.getElementById("diff-compare");
    const use = document.getElementById("diff-use");
    const status = document.getElementById("diff-status");
    const output = document.getElementById("diff-result");
    const base = `/api/v1/databases/${encodeURIComponent(databaseId)}`;
    let sql = "";
    let selection = 0;
    const invalidate = () => {
        selection++;
        sql = "";
        use.disabled = true;
        output.textContent = "";
        if (compare.disabled) status.textContent = "Pilihan schema berubah. Jalankan perbandingan kembali.";
    };
    current.addEventListener("change", invalidate);
    desired.addEventListener("change", invalidate);
    use.addEventListener("click", async () => { if (sql) await useSQL(sql); });
    compare.addEventListener("click", async () => {
        invalidate();
        if (!current.value || !desired.value || current.value === desired.value) {
            status.textContent = "Pilih dua schema yang berbeda.";
            return;
        }
        compare.disabled = true;
        status.textContent = "Membandingkan schema...";
        const request = selection;
        const currentSchema = current.value, desiredSchema = desired.value;
        try {
            const [before, after] = await Promise.all([readSchema(base, currentSchema), readSchema(base, desiredSchema)]);
            if (request !== selection) return;
            const result = compareSchemas(currentSchema, before, after);
            sql = result.sql;
            output.textContent = ["Perubahan:", ...(result.changes.length ? result.changes : ["Tidak ada tabel atau kolom baru."]),
                "", "Peringatan:", ...(result.warnings.length ? result.warnings : ["Tidak ada."]), "", "SQL kandidat:", sql || "Tidak ada SQL yang dibuat."].join("\n");
            use.disabled = !sql;
            status.textContent = "Perbandingan selesai. Tinjau peringatan dan SQL sebelum membuat draft.";
        } catch (error) {
            if (request === selection) status.textContent = error.message || "Gagal membaca schema.";
        } finally { compare.disabled = false; }
    });
    authenticatedRequest(`${base}/schemas`).then((result) => {
        if (!Array.isArray(result.schemas) || !result.schemas.every((s) => typeof s === "string")) throw new Error("Daftar schema tidak valid.");
        for (const schema of result.schemas) {
            for (const select of [current, desired]) {
                const option = document.createElement("option");
                option.value = schema;
                option.textContent = schema;
                select.appendChild(option);
            }
        }
        status.textContent = result.schemas.length < 2 ? "Diperlukan dua schema yang dapat diakses." : "Pilih schema yang akan diubah dan schema acuan.";
    }).catch((error) => { status.textContent = error.message || "Gagal memuat schema."; });
}
