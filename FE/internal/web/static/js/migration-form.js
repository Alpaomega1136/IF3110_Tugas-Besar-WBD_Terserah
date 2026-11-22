import { utf8Bytes } from "./ui.js";

const maxSQLBytes = 48 * 1024;
const maxBodyBytes = 64 * 1024;

// Nilai yang dikirim harus eksplisit agar backend membedakan reversible dan irreversible.
export function normalizeDraftDefinition(draft) {
    if (!draft || typeof draft.sql !== "string" ||
        (draft.sql_down != null && typeof draft.sql_down !== "string") ||
        typeof draft.irreversible !== "boolean" ||
        (draft.irreversible_reason != null && typeof draft.irreversible_reason !== "string")) {
        throw new Error("Format definisi migration dari backend tidak valid.");
    }
    return draft;
}

export function draftEditorValues(draft) {
    return {
        title: draft?.title ?? "", sql: draft?.sql ?? "", sqlDown: draft?.sql_down ?? "",
        irreversible: draft?.irreversible ?? false, reason: draft?.irreversible_reason ?? ""
    };
}

export function validateDraftEditor(values, revision = null) {
    const title = values.title.trim(), sql = values.sql.trim();
    const sqlDown = values.sqlDown.trim(), reason = values.reason.trim();
    const errors = {};
    if (!title || utf8Bytes(title) > 200 || title.includes("\0")) errors.title = "Judul wajib diisi dan maksimal 200 byte.";
    if (!sql || utf8Bytes(sql) > maxSQLBytes) errors.sql = "SQL Up wajib diisi dan maksimal 48 KiB.";
    if (values.irreversible) {
        if (!reason || utf8Bytes(reason) > 500 || reason.includes("\0")) errors.reason = "Alasan irreversible wajib diisi dan maksimal 500 byte.";
    } else if (!sqlDown || utf8Bytes(sqlDown) > maxSQLBytes) {
        errors.sqlDown = "SQL Down wajib diisi untuk migration reversible dan maksimal 48 KiB.";
    }
    const payload = {
        title, sql, sql_down: values.irreversible ? null : sqlDown,
        irreversible: Boolean(values.irreversible), irreversible_reason: values.irreversible ? reason : null
    };
    if (revision !== null) payload.revision = revision;
    if (utf8Bytes(JSON.stringify(payload)) > maxBodyBytes) errors.body = "Ukuran JSON melebihi 64 KiB.";
    return { errors, payload };
}

export function identicalDraftDefinition(a, b) {
    return a.title === b.title && a.sql === b.sql && a.sql_down === b.sql_down &&
        a.irreversible === b.irreversible && a.irreversible_reason === b.irreversible_reason;
}
