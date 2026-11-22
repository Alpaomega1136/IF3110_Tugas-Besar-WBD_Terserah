import { positiveInt64 } from "./pagination.js";

export const ACTION_LABELS = {
    up: "Up", rollback: "Rollback", forward: "Forward", resume: "Resume", compensate: "Kompensasi"
};

export function migrationAction(state, entry) {
    if (!state || state.has_unresolved || !entry || typeof entry.execution_id !== "string") return null;
    const id = entry.execution_id;
    if (entry.status === "success") {
        if (state.next_rollback?.execution_id === id && state.next_rollback.can_rollback) {
            return { type: "rollback", target: id, label: "Rollback" };
        }
        if (state.next_forward?.rollback_execution_id === id) {
            return { type: "forward", target: id, label: "Forward" };
        }
    }
    if (entry.status === "failed") {
        if (!positiveInt64(entry.draft_id) || !positiveInt64(entry.draft_revision)) return null;
        if (entry.action_type === "up") {
            return { type: "retry_up", target: id, label: "Retry Up" };
        }
        if (entry.action_type === "rollback" && state.next_rollback?.execution_id === entry.target_execution_id) {
            return { type: "retry_rollback", target: entry.target_execution_id, retry: id, label: "Retry Rollback" };
        }
        if (entry.action_type === "forward" && state.next_forward?.rollback_execution_id === entry.target_execution_id) {
            return { type: "retry_forward", target: entry.target_execution_id, retry: id, label: "Retry Forward" };
        }
    }
    return null;
}

export function operationRequest(base, entry, action) {
    if (!action) throw new Error("Aksi migration tidak tersedia.");
    if (action.type === "retry_up") {
        return {
            url: `${base}/drafts/${encodeURIComponent(entry.draft_id)}/retry`,
            body: { revision: entry.draft_revision, failed_execution_id: entry.execution_id }
        };
    }
    const type = action.type.replace("retry_", "");
    return {
        url: `${base}/migration-executions/${encodeURIComponent(action.target)}/${type}`,
        body: action.retry ? { failed_execution_id: action.retry } : {}
    };
}
