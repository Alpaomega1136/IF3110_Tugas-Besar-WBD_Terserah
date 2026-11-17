import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { positiveInt64, parseCursorPage } from "./pagination.js";
import { byId, addCell, formatDate, setBanner, navigateTo } from "./ui.js";

const page = byId("reviews-page");
const databaseId = page?.dataset?.databaseId || "";
const initialReviewId = page?.dataset?.reviewId || "";

const pageStatus = byId("reviews-page-status");
const pageError = byId("reviews-page-error");
const workspace = byId("reviews-workspace");
const scopeHeading = byId("reviews-scope-heading");
const onlyActionable = byId("reviews-only-actionable");
const statusFilter = byId("reviews-status-filter");
const refreshButton = byId("reviews-refresh");

const listStatus = byId("reviews-list-status");
const listError = byId("reviews-list-error");
const listEmpty = byId("reviews-list-empty");
const listTable = byId("reviews-list-table");
const listBody = byId("reviews-list-body");
const pagination = byId("reviews-pagination");
const prevButton = byId("reviews-prev");
const nextButton = byId("reviews-next");
const pageNumber = byId("reviews-page-number");

const detail = byId("review-detail");
const detailStatus = byId("review-detail-status");
const detailError = byId("review-detail-error");
const detailContent = byId("review-detail-content");
const detailSQL = byId("review-detail-sql");
const approvalsBody = byId("review-approvals-body");
const decisionPanel = byId("review-decision-panel");
const decisionNote = byId("review-decision-note");
const approveButton = byId("review-approve-button");
const rejectButton = byId("review-reject-button");
const decisionMessage = byId("review-decision-message");

const STATUS_LABELS = {
    pending_review: "Pending Review",
    approved: "Approved",
    rejected: "Rejected",
    queued: "Pending (Queue)",
    cancelled: "Dibatalkan"
};

const DECISION_LABELS = {
    pending: "Menunggu",
    approved: "Disetujui",
    rejected: "Ditolak"
};

let allItems = [];
let selectedReview = null;
let busy = false;
let cursors = [null];
let currentPage = 0;
let hasMore = false;
let nextCursor = null;
const pageLimit = 50;

function makeStatusBadge(status) {
    const span = document.createElement("span");
    span.className = `migration-status migration-status--${status}`;
    span.textContent = STATUS_LABELS[status] ?? String(status ?? "-");
    return span;
}

function approvalProgressText(approvals) {
    if (!Array.isArray(approvals) || approvals.length === 0) return "0/0";
    const approvedCount = approvals.filter((a) => a.decision === "approved").length;
    return `${approvedCount}/${approvals.length} leader`;
}

function setBusy(value) {
    busy = value;
    refreshButton.disabled = value;
    onlyActionable.disabled = value;
    if (statusFilter) statusFilter.disabled = value;
    approveButton.disabled = value;
    rejectButton.disabled = value;
    decisionNote.disabled = value;
    if (databaseId) {
        prevButton.disabled = value || currentPage === 0;
        nextButton.disabled = value || !hasMore;
    }
    for (const button of listBody.querySelectorAll("button")) {
        button.disabled = value;
    }
}

function renderList() {
    listBody.replaceChildren();
    const selectedStatus = statusFilter?.value || "";
    const filtered = allItems.filter((item) => {
        if (onlyActionable.checked && !item.can_current_user_act) return false;
        if (selectedStatus && item.status !== selectedStatus) return false;
        return true;
    });

    for (const item of filtered) {
        const row = document.createElement("tr");
        addCell(row, item.id);
        addCell(row, item.database_name || `DB ${item.database_id}`);
        addCell(row, `#${item.draft_id} (rev ${item.draft_revision}) — ${item.draft_title}`);
        addCell(row, item.action_type);

        const statusCell = document.createElement("td");
        statusCell.appendChild(makeStatusBadge(item.status));
        row.appendChild(statusCell);

        addCell(row, approvalProgressText(item.approvals));
        addCell(row, item.requested_by_username || "-");

        const actionCell = document.createElement("td");
        const button = document.createElement("button");
        button.type = "button";
        button.className = "history-open-button";
        button.textContent = "Detail";
        if (item.can_current_user_act) {
            button.textContent = "Tinjau";
        }
        button.disabled = busy;
        button.addEventListener("click", () => openReviewDetail(item.database_id, item.id));
        actionCell.appendChild(button);
        row.appendChild(actionCell);
        listBody.appendChild(row);
    }

    listEmpty.hidden = filtered.length !== 0;
    listTable.hidden = filtered.length === 0;
}

function renderReviewDetail(review) {
    selectedReview = review;
    const fields = {
        "review-detail-id": review.id,
        "review-detail-database": `${review.database_name} (ID ${review.database_id})`,
        "review-detail-draft": `#${review.draft_id} — ${review.draft_title}`,
        "review-detail-revision": review.draft_revision,
        "review-detail-action": review.action_type,
        "review-detail-requester": review.requested_by_username || "-",
        "review-detail-created": formatDate(review.created_at),
        "review-detail-decided": formatDate(review.decided_at),
        "review-detail-rejection": review.rejection_reason || "-"
    };
    for (const [id, value] of Object.entries(fields)) {
        byId(id).textContent = String(value);
    }
    const badge = byId("review-detail-badge");
    badge.className = `migration-status migration-status--${review.status}`;
    badge.textContent = STATUS_LABELS[review.status] ?? review.status;

    approvalsBody.replaceChildren();
    for (const item of review.approvals ?? []) {
        const row = document.createElement("tr");
        addCell(row, item.group_name);
        let accessLabel = "View Only";
        if (item.access_level === "edit") {
            accessLabel = "View + Edit";
        }
        addCell(row, accessLabel);
        addCell(row, item.leader_username);
        addCell(row, DECISION_LABELS[item.decision] ?? item.decision);
        addCell(row, item.note || "-");
        addCell(row, formatDate(item.decided_at));
        approvalsBody.appendChild(row);
    }

    detailSQL.textContent = review.sql_snapshot || "";
    decisionPanel.hidden = !review.can_current_user_act;
    setBanner(decisionMessage, "");
    detailContent.hidden = false;
    detail.hidden = false;
}

async function openReviewDetail(dbId, reviewId) {
    if (busy) return;
    decisionNote.value = "";
    selectedReview = null;
    setBusy(true);
    detail.hidden = false;
    detailContent.hidden = true;
    detailError.hidden = true;
    detailStatus.textContent = `Memuat detail review #${reviewId}...`;
    detailStatus.hidden = false;

    try {
        const result = await authenticatedRequest(
            `/api/v1/databases/${encodeURIComponent(String(dbId))}/reviews/${encodeURIComponent(String(reviewId))}`
        );
        if (!result?.review || !positiveInt64(result.review.id)) {
            throw new Error("Format detail review dari backend tidak valid.");
        }
        renderReviewDetail(result.review);
    } catch (error) {
        if (error.status === 401) {
            navigateTo("/login", { replace: true });
            return;
        }
        detailError.textContent = error.message || "Gagal memuat detail review.";
        detailError.hidden = false;
    } finally {
        detailStatus.hidden = true;
        setBusy(false);
    }
}

async function loadReviews(index = currentPage, reset = false) {
    if (busy) return;
    if (reset) {
        cursors = [null];
        index = 0;
    }
    setBusy(true);
    listError.hidden = true;
    listStatus.textContent = "Memuat daftar pengajuan review...";
    listStatus.hidden = false;

    try {
        if (databaseId) {
            const cursor = cursors[index];
            const query = new URLSearchParams({ limit: String(pageLimit) });
            if (cursor !== null) query.set("before", cursor);
            const pageResult = parseCursorPage(
                await authenticatedRequest(`/api/v1/databases/${encodeURIComponent(databaseId)}/reviews?${query}`),
                "reviews",
                "next_before"
            );
            allItems = pageResult.entries;
            currentPage = index;
            hasMore = pageResult.hasMore;
            nextCursor = pageResult.nextCursor;
            cursors = cursors.slice(0, index + 1);
            pagination.hidden = false;
            pageNumber.textContent = `Halaman ${currentPage + 1}`;
        } else {
            const result = await authenticatedRequest("/api/v1/reviews");
            if (!Array.isArray(result?.reviews)) {
                throw new Error("Format daftar review dari backend tidak valid.");
            }
            allItems = result.reviews;
            pagination.hidden = true;
        }
        renderList();
    } catch (error) {
        if (error.status === 401) {
            navigateTo("/login", { replace: true });
            return;
        }
        listError.textContent = error.message || "Gagal memuat daftar review.";
        listError.hidden = false;
    } finally {
        listStatus.hidden = true;
        setBusy(false);
    }
}

async function submitDecision(approve) {
    if (busy || !selectedReview) return;
    const note = decisionNote.value.trim();
    if (!approve && !note) {
        setBanner(decisionMessage, "Isi alasan penolakan sebelum melakukan Reject.", true);
        decisionNote.focus();
        return;
    }

    const target = `${selectedReview.database_name} (Draft #${selectedReview.draft_id}, revisi ${selectedReview.draft_revision})`;
    let confirmation = {
        title: "Reject perubahan migration?",
        message: `Tolak perubahan pada ${target}?`,
        confirmLabel: "Reject",
        tone: "danger"
    };
    if (approve) {
        confirmation = {
            title: "Approve perubahan migration?",
            message: `Setujui perubahan pada ${target}?`,
            confirmLabel: "Approve",
            tone: "warning"
        };
    }
    const confirmed = await confirmAction(confirmation);
    if (!confirmed || busy) return;

    setBusy(true);
    setBanner(decisionMessage, "Menyimpan keputusan review...");
    try {
        let action = "reject";
        if (approve) action = "approve";
        const result = await authenticatedRequest(
            `/api/v1/databases/${encodeURIComponent(selectedReview.database_id)}/reviews/${encodeURIComponent(selectedReview.id)}/${action}`,
            {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ note })
            }
        );
        if (!result?.review) {
            throw new Error("Respons keputusan review tidak valid.");
        }
        decisionNote.value = "";
        renderReviewDetail(result.review);
        let message = "Perubahan telah ditolak (Status: Rejected).";
        if (approve && result.review.status === "approved") {
            message = "Persetujuan tercatat. Seluruh leader group telah menyetujui perubahan ini (Status: Approved).";
        } else if (approve) {
            message = "Persetujuan group kamu tercatat. Menunggu persetujuan leader group lainnya.";
        }
        setBanner(decisionMessage, message);
    } catch (error) {
        if (error.status === 401) {
            navigateTo("/login", { replace: true });
            return;
        }
        let msg = error.message || "Gagal menyimpan keputusan review.";
        if (isCSRFError(error)) {
            msg = "Token CSRF tidak valid. Silakan coba lagi.";
        }
        setBanner(decisionMessage, msg, true);
    } finally {
        setBusy(false);
    }
    await loadReviews(currentPage, false);
}

onlyActionable.addEventListener("change", renderList);
statusFilter?.addEventListener("change", renderList);
refreshButton.addEventListener("click", () => loadReviews(0, true));
prevButton?.addEventListener("click", () => loadReviews(currentPage - 1));
nextButton?.addEventListener("click", () => {
    if (busy || !hasMore || !positiveInt64(nextCursor)) return;
    cursors[currentPage + 1] = nextCursor;
    return loadReviews(currentPage + 1);
});
approveButton.addEventListener("click", () => submitDecision(true));
rejectButton.addEventListener("click", () => submitDecision(false));

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;

        if (databaseId) {
            const heading = scopeHeading ?? byId("reviews-list-heading");
            if (heading) {
                heading.textContent = `Review Database (ID ${databaseId})`;
            }
        }
        workspace.hidden = false;
        await loadReviews(0, true);
        if (databaseId && initialReviewId) {
            await openReviewDetail(databaseId, initialReviewId);
        }
    } catch (error) {
        if (error.status === 401) {
            navigateTo("/login", { replace: true });
            return;
        }
        pageError.textContent = error.message || "Gagal memuat halaman review.";
        pageError.hidden = false;
    } finally {
        pageStatus.hidden = true;
    }
}

initialize();
