// BE2 cursor contracts use JSON strings for int64 identifiers. Never coerce to Number.
export function positiveInt64(value) {
    return typeof value === "string" && /^[1-9][0-9]*$/.test(value) &&
        BigInt(value) <= 9223372036854775807n;
}

export function parseCursorPage(payload, listKey, cursorKey) {
    if (!payload || !Array.isArray(payload[listKey]) || typeof payload.has_more !== "boolean") {
        throw new Error("Format pagination dari backend tidak valid.");
    }
    const cursor = payload[cursorKey];
    if (payload.has_more && !positiveInt64(cursor)) {
        throw new Error("Cursor pagination dari backend tidak valid.");
    }
    if (!payload.has_more && cursor != null) {
        throw new Error("Cursor pagination akhir dari backend tidak valid.");
    }
    if (payload.has_more && payload[listKey].length === 0) {
        throw new Error("Halaman kosong memiliki cursor lanjutan yang tidak valid.");
    }
    return {
        entries: payload[listKey],
        hasMore: payload.has_more,
        nextCursor: payload.has_more ? cursor : null
    };
}

export function parseReconcileResult(payload) {
    if (!payload || !Array.isArray(payload.reconciled)) {
        throw new Error("Format hasil reconcile dari backend tidak valid.");
    }
    for (const item of payload.reconciled) {
        if (!item || !positiveInt64(item.version) ||
            typeof item.execution_id !== "string" ||
            !["running", "unknown", "success", "failed", "partial"].includes(item.status) ||
            !(item.error_code == null || typeof item.error_code === "string")) {
            throw new Error("Format hasil reconcile dari backend tidak valid.");
        }
    }
    return payload.reconciled;
}
