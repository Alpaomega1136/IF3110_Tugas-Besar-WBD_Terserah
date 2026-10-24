import {
    authenticatedRequest,
    requireAuthenticatedUser
} from "./auth.js";

const status = document.getElementById("grants-status");
const errorBox = document.getElementById("grants-error");
const panel = document.getElementById("grants-panel");
const noGroups = document.getElementById("grants-no-groups");
const noGrants = document.getElementById("grants-empty");
const table = document.getElementById("grants-table");
const body = document.getElementById("grants-body");

const form = document.getElementById("grant-form");
const groupSelect = document.getElementById("grant-group");
const databaseInput = document.getElementById("grant-database-id");
const accessSelect = document.getElementById("grant-access");
const submitButton = document.getElementById("grant-submit");
const message = document.getElementById("grant-message");

let groups = [];
let currentUserId = null;
let saving = false;

function showMessage(text, isError = false) {
    message.textContent = text;
    message.className = isError ? "error-message" : "success-message";
    message.hidden = !text;
}

function appendCell(row, text) {
    const td = document.createElement("td");
    td.textContent = String(text);
    row.appendChild(td);
    return td;
}

function groupContainsSelf(group) {
    return (group.user_ids ?? []).some((id) => String(id) === currentUserId);
}

function validDatabaseId(value) {
    return /^[1-9][0-9]*$/.test(value) &&
        BigInt(value) <= 9223372036854775807n;
}

function setSaving(value) {
    saving = value;

    groupSelect.disabled = value || groups.length === 0;
    databaseInput.disabled = value || groups.length === 0;
    accessSelect.disabled = value || groups.length === 0;
    submitButton.disabled = value || groups.length === 0;

    for (const button of body.querySelectorAll("button")) {
        button.disabled = value;
    }

    submitButton.textContent = value
        ? "Menyimpan..."
        : "Simpan Grant";
}

function renderGrants() {
    const previousGroup = groupSelect.value;

    groupSelect.replaceChildren();

    const placeholder = document.createElement("option");
    placeholder.value = "";
    placeholder.textContent = "Pilih group...";
    groupSelect.appendChild(placeholder);

    body.replaceChildren();

    let grantCount = 0;

    for (const group of groups) {
        const option = document.createElement("option");
        option.value = String(group.id);
        option.textContent = group.name;
        groupSelect.appendChild(option);

        for (const grant of group.grants ?? []) {
            grantCount++;

            const row = document.createElement("tr");

            appendCell(row, group.name);
            appendCell(row, grant.database_id);

            appendCell(
                row,
                grant.access_level === "edit"
                    ? "View + Edit"
                    : "View Only"
            );

            const actions = document.createElement("td");

            const edit = document.createElement("button");
            edit.type = "button";
            edit.className = "grant-action";
            edit.textContent = "Ubah";

            edit.addEventListener("click", () => {
                groupSelect.value = String(group.id);
                databaseInput.value = String(grant.database_id);
                accessSelect.value = grant.access_level;

                showMessage("");

                form.scrollIntoView({
                    behavior: "smooth",
                    block: "start"
                });

                accessSelect.focus({ preventScroll: true });
            });

            const revoke = document.createElement("button");
            revoke.type = "button";
            revoke.className = "grant-action grant-revoke";
            revoke.textContent = "Cabut";

            revoke.setAttribute(
                "aria-label",
                `Cabut grant database ${grant.database_id} untuk ${group.name}`
            );

            revoke.addEventListener("click", () => {
                revokeGrant(group, grant);
            });

            actions.append(edit, revoke);
            row.appendChild(actions);
            body.appendChild(row);
        }
    }

    if (groups.some(
        (group) => String(group.id) === previousGroup
    )) {
        groupSelect.value = previousGroup;
    }

    panel.hidden = groups.length === 0;
    noGroups.hidden = groups.length !== 0;

    table.hidden = grantCount === 0;
    noGrants.hidden = grantCount !== 0 || groups.length === 0;

    setSaving(saving);
}

async function reloadGroups() {
    const data = await authenticatedRequest("/api/v1/admin/groups");

    if (!Array.isArray(data?.groups)) {
        throw new Error("Format daftar group dari backend tidak valid.");
    }

    groups = data.groups;
    renderGrants();
}

function handleError(err) {
    if (err.status === 401) {
        window.location.replace("/login");
        return;
    }

    if (err.status === 403 && err.code === "csrf_invalid") {
        showMessage(
            "Token CSRF tidak valid. Silakan coba lagi.",
            true
        );
    } else if (err.status === 403) {
        showMessage(
            "Akses ditolak. Hanya Platform Admin yang boleh mengelola grant.",
            true
        );
    } else if (err.status === 404) {
        showMessage(
            "Group, database, atau grant tidak ditemukan. Periksa ID dan data terbaru.",
            true
        );
    } else {
        showMessage(
            err.message || "Operasi grant gagal.",
            true
        );
    }
}

form.addEventListener("submit", async (event) => {
    event.preventDefault();

    if (saving) return;

    const group = groups.find(
        (g) => String(g.id) === groupSelect.value
    );

    const databaseId = databaseInput.value.trim();
    const level = accessSelect.value;

    if (!group || !validDatabaseId(databaseId) || !["view", "edit"].includes(level)) {
        showMessage(
            "Pilih group, masukkan ID database positif yang valid, dan pilih izin.",
            true
        );
        return;
    }

    const existing = (group.grants ?? []).find(
        (g) => String(g.database_id) === databaseId
    );

    if (existing?.access_level === level) {
        showMessage(
            "Grant tersebut sudah memiliki tingkat akses yang sama.",
            true
        );
        return;
    }

    const downgrade =
        existing?.access_level === "edit" &&
        level === "view";

    if (downgrade &&
        !window.confirm(
            "Penurunan izin akan mencabut session anggota group. Lanjutkan?"
        )
    ) {
        return;
    }

    showMessage("");
    setSaving(true);

    try {
        await authenticatedRequest(
            `/api/v1/admin/groups/${encodeURIComponent(String(group.id))}/databases/${encodeURIComponent(databaseId)}`,
            {
                method: "PUT",
                headers: {
                    "Content-Type": "application/json"
                },
                body: JSON.stringify({
                    access_level: level
                })
            }
        );

        if (downgrade && groupContainsSelf(group)) {
            window.location.replace("/login");
            return;
        }

        try {
            await reloadGroups();
            showMessage("Grant berhasil disimpan.");
        } catch (refreshError) {
            if (refreshError.status === 401) {
                window.location.replace("/login");
                return;
            }

            showMessage(
                "Grant tersimpan, tetapi daftar terbaru gagal dimuat.",
                true
            );
        }

    } catch (err) {
        handleError(err);
    } finally {
        setSaving(false);
    }
});

async function revokeGrant(group, grant) {
    if (saving) return;

    const confirmed = window.confirm(
        `Cabut akses database ${grant.database_id} dari group ${group.name}? Session anggota group akan dicabut.`
    );

    if (!confirmed) return;

    showMessage("");
    setSaving(true);

    try {
        await authenticatedRequest(
            `/api/v1/admin/groups/${encodeURIComponent(String(group.id))}/databases/${encodeURIComponent(String(grant.database_id))}`,
            {
                method: "DELETE"
            }
        );

        if (groupContainsSelf(group)) {
            window.location.replace("/login");
            return;
        }

        try {
            await reloadGroups();
            showMessage("Grant berhasil dicabut.");
        } catch (refreshError) {
            if (refreshError.status === 401) {
                window.location.replace("/login");
                return;
            }

            showMessage(
                "Grant dicabut, tetapi daftar terbaru gagal dimuat.",
                true
            );
        }

    } catch (err) {
        handleError(err);
    } finally {
        setSaving(false);
    }
}

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();

        if (!user) return;

        if (!user.is_platform_admin) {
            status.hidden = true;

            errorBox.textContent = "Akses ditolak. Halaman ini hanya untuk Platform Admin.";
            errorBox.hidden = false;
            return;
        }

        currentUserId = String(user.id);

        status.textContent = "Memuat group dan grant...";

        await reloadGroups();

        status.hidden = true;

    } catch (err) {
        status.hidden = true;

        if (err.status === 401) {
            window.location.replace("/login");
            return;
        }

        errorBox.textContent = err.message || "Gagal memuat grant.";
        errorBox.hidden = false;
    }
}

initialize();