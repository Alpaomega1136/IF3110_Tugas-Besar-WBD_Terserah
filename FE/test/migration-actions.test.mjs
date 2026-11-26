import test from "node:test";
import assert from "node:assert/strict";
import { migrationAction, operationRequest } from "../internal/web/static/js/migration-actions.js";

const base = "/api/v1/databases/9007199254740993";
const entry = { execution_id: "exec-1", status: "success", action_type: "up", draft_id: "2", draft_revision: "5" };
const state = { has_unresolved: false, next_rollback: { execution_id: "exec-1", can_rollback: true }, next_forward: null };

test("rollback hanya untuk eksekusi aktif terakhir dan reversible", () => {
    assert.deepEqual(migrationAction(state, entry), { type: "rollback", target: "exec-1", label: "Rollback" });
    assert.equal(migrationAction({ ...state, has_unresolved: true }, entry), null);
    assert.equal(migrationAction({ ...state, next_rollback: { execution_id: "exec-2", can_rollback: true } }, entry), null);
    assert.equal(migrationAction({ ...state, next_rollback: { execution_id: "exec-1", can_rollback: false } }, entry), null);
});

test("forward mengacu execution rollback bukan nama draft", () => {
    const afterRollback = { ...entry, execution_id: "rollback-2", action_type: "rollback" };
    const s = { has_unresolved: false, next_forward: { rollback_execution_id: "rollback-2" } };
    assert.equal(migrationAction(s, afterRollback).type, "forward");
    assert.equal(migrationAction(s, entry), null);
});

test("batch cancellation uses the compensation endpoint and completed compensation can be forwarded", () => {
    const batch = { ...entry, execution_mode: "batch" };
    const action = migrationAction(state, batch);
    assert.equal(action.type, "compensate");
    assert.deepEqual(operationRequest(base, batch, action), { url: `${base}/migration-executions/exec-1/compensate`, body: {} });
    const completed = { ...batch, execution_id: "comp-1", action_type: "compensate" };
    assert.equal(migrationAction({ has_unresolved: false, next_forward: { rollback_execution_id: "comp-1" } }, completed).type, "forward");
});

test("retry hanya untuk attempt gagal dan target yang tepat", () => {
    const failedUp = { ...entry, status: "failed" };
    assert.equal(migrationAction(state, failedUp).type, "retry_up");
    const failedRollback = { ...failedUp, action_type: "rollback", target_execution_id: "exec-1" };
    assert.deepEqual(migrationAction(state, failedRollback), { type: "retry_rollback", target: "exec-1", retry: "exec-1", label: "Retry Rollback" });
    assert.equal(migrationAction({ ...state, next_rollback: null }, failedRollback), null);
    assert.equal(migrationAction(state, { ...failedUp, draft_id: 2 }), null);
});

test("request eksekusi dan retry mempertahankan string int64", () => {
    const rollback = migrationAction(state, entry);
    assert.deepEqual(operationRequest(base, entry, rollback), {
        url: `${base}/migration-executions/exec-1/rollback`, body: {}
    });
    const retry = { type: "retry_up", target: "exec-1" };
    assert.deepEqual(operationRequest(base, { ...entry, status: "failed" }, retry), {
        url: `${base}/drafts/2/retry`, body: { revision: "5", failed_execution_id: "exec-1" }
    });
});
