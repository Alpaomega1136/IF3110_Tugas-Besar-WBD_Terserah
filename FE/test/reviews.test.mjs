import test from "node:test";
import assert from "node:assert/strict";
import { Element, tick } from "./mock-dom.mjs";
import { clearAuthState } from "../internal/web/static/js/auth.js";

const dbID = "9007199254740993";
const reviewID = "9007199254741001";
let run = 0;

function json(payload, status = 200) {
    return new Response(JSON.stringify(payload), { status, headers: { "Content-Type": "application/json" } });
}

const sampleReview = {
    id: reviewID,
    database_id: dbID,
    database_name: "Orders DB",
    draft_id: "14",
    draft_title: "Add index",
    draft_revision: "2",
    action_type: "up",
    sql_snapshot: '<script>alert("xss")</script> CREATE INDEX idx ON orders(id);',
    status: "pending_review",
    requested_by: "7",
    requested_by_username: "editor_user",
    rejection_reason: null,
    created_at: "2026-11-16T10:00:00Z",
    decided_at: null,
    can_current_user_act: true,
    approvals: [
        {
            group_id: "10",
            group_name: "Core Backend",
            access_level: "edit",
            leader_user_id: "7",
            leader_username: "editor_user",
            decision: "approved",
            note: "Ready",
            decided_at: "2026-11-16T10:05:00Z"
        },
        {
            group_id: "20",
            group_name: "Analytics Reader",
            access_level: "view",
            leader_user_id: "12",
            leader_username: "viewer_leader",
            decision: "pending",
            note: null,
            decided_at: null
        }
    ]
};

async function setupReviews({ databaseScoped = false, confirm = true, access = "view", custom } = {}) {
    clearAuthState();
    const ids = [
        "reviews-page", "reviews-page-status", "reviews-page-error", "reviews-workspace",
        "reviews-scope-heading", "reviews-scope-description", "reviews-only-actionable", "reviews-refresh",
        "reviews-list-status", "reviews-list-error", "reviews-list-empty", "reviews-list-table",
        "reviews-list-body", "reviews-pagination", "reviews-prev", "reviews-next", "reviews-page-number",
        "review-detail", "review-detail-status", "review-detail-error", "review-detail-content",
        "review-detail-id", "review-detail-database", "review-detail-draft", "review-detail-revision",
        "review-detail-action", "review-detail-target", "review-detail-badge", "review-detail-requester", "review-detail-created",
        "review-detail-decided", "review-detail-rejection", "review-approvals-body", "review-detail-sql",
        "review-job-panel", "review-enqueue", "review-jobs-link", "review-job-message",
        "review-decision-panel", "review-decision-note", "review-approve-button", "review-reject-button",
        "review-decision-message"
    ];
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    const $ = (id) => elements[id];
    $("reviews-page").dataset.databaseId = databaseScoped ? dbID : "";
    $("reviews-page").dataset.reviewId = "";
    $("reviews-only-actionable").checked = false;
    for (const id of [
        "reviews-page-error", "reviews-workspace", "reviews-list-status", "reviews-list-error",
        "reviews-list-empty", "reviews-list-table", "reviews-pagination", "review-detail",
        "review-detail-status", "review-detail-error", "review-detail-content", "review-decision-panel",
        "review-decision-message"
    ]) $(id).hidden = true;

    const calls = [];
    const redirects = [];
    let currentReview = structuredClone(sampleReview);

    globalThis.document = { getElementById: (id) => $(id), createElement: (tag) => new Element(tag) };
    globalThis.window = {
        location: { replace: (url) => redirects.push(url) },
        confirm: () => confirm
    };
    globalThis.fetch = async (url, options = {}) => {
        const method = options.method ?? "GET";
        const body = options.body ? JSON.parse(options.body) : null;
        calls.push({ url, method, body, headers: new Headers(options.headers) });
        if (custom) {
            const out = custom({ url, method, body });
            if (out !== undefined) return out;
        }
        if (url === "/api/v1/auth/me") return json({ id: "12", username: "viewer_leader", is_platform_admin: false });
        if (url === "/api/v1/auth/csrf") return json({ csrf_token: "review-csrf" });
        if (url === "/api/v1/databases") return json({ databases: [{ id: dbID, access }] });
        if (url === "/api/v1/reviews") return json({ reviews: [currentReview] });
        if (url === `/api/v1/databases/${dbID}/reviews?limit=50`) {
            return json({ reviews: [currentReview], has_more: false, next_before: null });
        }
        if (url === `/api/v1/databases/${dbID}/reviews/${reviewID}` && method === "GET") {
            return json({ review: currentReview });
        }
        if (url === `/api/v1/databases/${dbID}/reviews/${reviewID}/approve` && method === "POST") {
            currentReview = {
                ...currentReview,
                status: "approved",
                can_current_user_act: false,
                approvals: currentReview.approvals.map((a) => ({ ...a, decision: "approved" }))
            };
            return json({ review: currentReview });
        }
        if (url === `/api/v1/databases/${dbID}/reviews/${reviewID}/reject` && method === "POST") {
            currentReview = {
                ...currentReview,
                status: "rejected",
                rejection_reason: body.note,
                can_current_user_act: false
            };
            return json({ review: currentReview });
        }
        throw new Error(`Unhandled ${method} ${url}`);
    };

    await import(`../internal/web/static/js/reviews.js?run=${++run}`);
    await tick();
    return { $, calls, redirects };
}

test("reviews list renders approval progress and opens detail with safe SQL and leader decision controls", async () => {
    const ui = await setupReviews();
    assert.equal(ui.$("reviews-workspace").hidden, false);
    assert.equal(ui.$("reviews-list-body").children.length, 1);
    assert.match(ui.$("reviews-list-body").children[0].children[5].textContent, /1\/2 leader/);

    await ui.$("reviews-list-body").children[0].children[7].children[0].fire("click");
    await tick();

    assert.equal(ui.$("review-detail").hidden, false);
    assert.equal(ui.$("review-detail-sql").textContent, sampleReview.sql_snapshot);
    assert.equal(ui.$("review-detail-sql").children.length, 0);
    assert.equal(ui.$("review-approvals-body").children.length, 2);
    assert.equal(ui.$("review-decision-panel").hidden, false);

    ui.$("review-decision-note").value = "Verified on analytics replica";
    await ui.$("review-approve-button").fire("click");
    await tick();

    const approveCall = ui.calls.find((c) => c.url.endsWith("/approve") && c.method === "POST");
    assert.ok(approveCall);
    assert.equal(approveCall.headers.get("X-CSRF-Token"), "review-csrf");
    assert.equal(ui.$("review-detail-badge").textContent, "Approved");
    assert.equal(ui.$("review-decision-panel").hidden, true);
    assert.match(ui.$("review-decision-message").textContent, /Approved/);
});

test("resume review shows its exact target execution before approval", async () => {
    const target = '<script>execution</script>';
    const ui = await setupReviews({ custom: ({ url }) => {
        if (url === `/api/v1/databases/${dbID}/reviews/${reviewID}`) {
            return json({ review: { ...sampleReview, action_type: "resume", target_execution_id: target } });
        }
        return undefined;
    } });
    await ui.$("reviews-list-body").children[0].children[7].children[0].fire("click");
    await tick();
    assert.equal(ui.$("review-detail-action").textContent, "resume");
    assert.equal(ui.$("review-detail-target").textContent, target);
    assert.equal(ui.$("review-detail-target").children.length, 0);
});

test("reject requires a non-empty reason before sending POST", async () => {
    const ui = await setupReviews();
    await ui.$("reviews-list-body").children[0].children[7].children[0].fire("click");
    await tick();

    ui.$("review-decision-note").value = "   ";
    await ui.$("review-reject-button").fire("click");
    assert.equal(ui.calls.some((c) => c.url.endsWith("/reject")), false);
    assert.match(ui.$("review-decision-message").textContent, /alasan penolakan/);

    ui.$("review-decision-note").value = "Breaking column rename";
    await ui.$("review-reject-button").fire("click");
    await tick();
    assert.equal(ui.calls.some((c) => c.url.endsWith("/reject") && c.method === "POST"), true);
    assert.equal(ui.$("review-detail-badge").textContent, "Rejected");
});

test("edit user queues an approved resume once with confirmation and CSRF", async () => {
    const review = { ...sampleReview, action_type: "resume", target_execution_id: "a".repeat(32), status: "approved", can_current_user_act: false };
    const ui = await setupReviews({ access: "edit", custom: ({ url, method }) => {
        if (url.endsWith(`/reviews/${reviewID}`)) return json({ review });
        if (url.endsWith("/jobs") && method === "POST") return json({ job: { id: "55", review_id: reviewID, database_id: dbID, status: "retry_wait" } }, 202);
    } });
    await ui.$("reviews-list-body").children[0].children[7].children[0].fire("click"); await tick();
    assert.equal(ui.$("review-enqueue").hidden, false);
    await ui.$("review-enqueue").fire("click");
    const request = ui.calls.find((c) => c.url.endsWith("/jobs") && c.method === "POST");
    assert.deepEqual(request.body, { review_id: reviewID });
    assert.equal(request.headers.get("X-CSRF-Token"), "review-csrf");
    assert.equal(ui.redirects.at(-1), `/databases/${dbID}/jobs/55`);
    assert.doesNotMatch(ui.$("review-job-message").textContent, /berhasil di-commit/);
});

test("view user and cancelled confirmation never enqueue approved jobs", async () => {
    const review = { ...sampleReview, status: "approved", can_current_user_act: false };
    const custom = ({ url }) => url.endsWith(`/reviews/${reviewID}`) ? json({ review }) : undefined;
    const viewer = await setupReviews({ custom });
    await viewer.$("reviews-list-body").children[0].children[7].children[0].fire("click"); await tick();
    assert.equal(viewer.$("review-enqueue").hidden, true);
    await viewer.$("review-enqueue").fire("click");
    assert.equal(viewer.calls.some((c) => c.url.endsWith("/jobs")), false);
    const cancelled = await setupReviews({ access: "edit", confirm: false, custom });
    await cancelled.$("reviews-list-body").children[0].children[7].children[0].fire("click"); await tick();
    await cancelled.$("review-enqueue").fire("click");
    assert.equal(cancelled.calls.some((c) => c.method === "POST"), false);
});

test("uncertain enqueue response locks the action and never retries SQL automatically", async () => {
    const review = { ...sampleReview, status: "approved", can_current_user_act: false };
    const ui = await setupReviews({ access: "edit", custom: ({ url, method }) => {
        if (url.endsWith(`/reviews/${reviewID}`)) return json({ review });
        if (url.endsWith("/jobs") && method === "POST") throw new Error("lost response");
    } });
    await ui.$("reviews-list-body").children[0].children[7].children[0].fire("click"); await tick();
    await ui.$("review-enqueue").fire("click");
    await ui.$("review-enqueue").fire("click");
    assert.equal(ui.calls.filter((c) => c.url.endsWith("/jobs") && c.method === "POST").length, 1);
    assert.equal(ui.$("review-enqueue").disabled, true);
    assert.match(ui.$("review-job-message").textContent, /Periksa Jobs/);
});

test("enqueue rechecks current edit access and review state before submitting", async () => {
    const review = { ...sampleReview, status: "approved", can_current_user_act: false };
    let revoked = false;
    const ui = await setupReviews({ access: "edit", custom: ({ url }) => {
        if (url.endsWith(`/reviews/${reviewID}`)) return json({ review });
        if (revoked && url === "/api/v1/databases") return json({ databases: [{ id: dbID, access: "view" }] });
    } });
    await ui.$("reviews-list-body").children[0].children[7].children[0].fire("click"); await tick();
    revoked = true;
    await ui.$("review-enqueue").fire("click");
    assert.equal(ui.calls.some((c) => c.method === "POST"), false);
    assert.equal(ui.$("review-enqueue").hidden, true);
});
