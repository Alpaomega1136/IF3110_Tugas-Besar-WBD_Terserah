import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { byId, addCell, addOption, setBanner, setFieldError, utf8Bytes } from "./ui.js";

const status = byId("groups-status");
const error = byId("groups-error");
const empty = byId("groups-empty");
const section = byId("groups-section");
const body = byId("groups-body");

const createSection = byId("create-group-section");
const createForm = byId("create-group-form");
const groupName = byId("create-group-name");
const groupNameError = byId("create-group-error");
const createButton = byId("create-group-submit");
const createMessage = byId("create-group-message");

const membershipSection = byId("membership-section");
const membershipForm = byId("membership-form");
const membershipGroup = byId("membership-group");
const membershipUser = byId("membership-user");
const membershipSubmit = byId("membership-submit");
const membershipMessage = byId("membership-message");

let groupsData = [];
let usersData = null;
let membershipSaving = false;
let currentUserId = null;

function updateGroupChoices() {
    const previousGroup = membershipGroup.value;
    membershipGroup.replaceChildren();
    addOption(membershipGroup, "", "Pilih group...");

    for (const group of groupsData) {
        addOption(membershipGroup, String(group.id), group.name);
    }
    if (groupsData.some((group) => String(group.id) === previousGroup)) {
        membershipGroup.value = previousGroup;
    }

    membershipSection.hidden = groupsData.length === 0;
    membershipGroup.disabled = membershipSaving || groupsData.length === 0;
    updateUserChoices();
}

function updateUserChoices() {
    const previousUser = membershipUser.value;
    const group = groupsData.find((item) => String(item.id) === membershipGroup.value);
    const memberIds = new Set(Array.isArray(group?.user_ids) ? group.user_ids.map(String) : []);

    const candidates = group && Array.isArray(usersData)
        ? usersData.filter((user) => !memberIds.has(String(user.id)))
        : [];

    membershipUser.replaceChildren();
    addOption(
        membershipUser,
        "",
        group && candidates.length === 0 ? "Semua pengguna sudah menjadi anggota" : "Pilih pengguna..."
    );

    for (const user of candidates) {
        const label = user.is_active ? user.username : `${user.username} (nonaktif)`;
        addOption(membershipUser, String(user.id), label);
    }
    if (candidates.some((user) => String(user.id) === previousUser)) {
        membershipUser.value = previousUser;
    }

    membershipUser.disabled = membershipSaving || candidates.length === 0;
    membershipSubmit.disabled = membershipSaving || !group || !membershipUser.value;
}

function renderGroupsTable() {
    body.replaceChildren();
    const usersById = new Map((usersData ?? []).map((user) => [String(user.id), user]));

    for (const group of groupsData) {
        const row = document.createElement("tr");
        const userIds = Array.isArray(group.user_ids) ? group.user_ids : [];
        const grants = Array.isArray(group.grants) ? group.grants : [];

        const grantText = grants.length
            ? grants.map((grant) => {
                const level = grant.access_level === "edit"
                    ? "View + Edit"
                    : grant.access_level === "view" ? "View Only" : grant.access_level;
                return `DB ${grant.database_id}: ${level}`;
            }).join(", ")
            : "Belum ada grant";

        addCell(row, group.id);
        addCell(row, group.name);

        const membersCell = document.createElement("td");
        if (userIds.length === 0) {
            membersCell.textContent = "Belum ada anggota";
        } else {
            const count = document.createElement("p");
            count.textContent = `${userIds.length} anggota`;
            count.className = "group-member-count";
            membersCell.appendChild(count);

            const list = document.createElement("div");
            list.className = "group-member-list";
            for (const rawId of userIds) {
                const userId = String(rawId);
                const user = usersById.get(userId);
                const label = user
                    ? `${user.username}${user.is_active ? "" : " (nonaktif)"}`
                    : `User ID ${userId}`;

                const item = document.createElement("div");
                item.className = "group-member-item";
                const name = document.createElement("span");
                name.textContent = label;

                const button = document.createElement("button");
                button.type = "button";
                button.className = "remove-member-button";
                button.textContent = "Keluarkan";
                button.disabled = membershipSaving;
                button.setAttribute("aria-label", `Keluarkan ${label} dari group ${group.name}`);
                button.addEventListener("click", () => removeMember(group, userId, label));

                item.append(name, button);
                list.appendChild(item);
            }
            membersCell.appendChild(list);
        }

        row.appendChild(membersCell);
        addCell(row, grantText);
        body.appendChild(row);
    }
}

async function removeMember(group, userId, label) {
    if (membershipSaving) return;
    const isSelf = userId === currentUserId;
    const question = isSelf
        ? `Keluarkan akunmu sendiri dari group ${group.name}? Session kamu akan dicabut dan kamu harus login kembali.`
        : `Keluarkan ${label} dari group ${group.name}? Session aktif pengguna tersebut akan dicabut.`;

    if (!(await confirmAction({
        title: isSelf ? "Keluar dari group?" : "Keluarkan anggota group?",
        message: question,
        confirmLabel: "Keluarkan",
        tone: "danger"
    })) || membershipSaving) return;

    membershipSaving = true;
    setBanner(membershipMessage, "");
    updateGroupChoices();
    renderGroupsTable();

    try {
        await authenticatedRequest(
            `/api/v1/admin/groups/${encodeURIComponent(String(group.id))}/users/${encodeURIComponent(userId)}`,
            { method: "DELETE" }
        );
        if (isSelf) {
            window.location.replace("/login");
            return;
        }
        const refreshed = await loadGroups();
        setBanner(
            membershipMessage,
            refreshed
                ? `${label} berhasil dikeluarkan dari group ${group.name}.`
                : "Anggota sudah dikeluarkan, tetapi daftar group gagal diperbarui.",
            !refreshed
        );
    } catch (err) {
        if (err.status === 401) {
            window.location.replace("/login");
            return;
        }
        if (err.status === 403 && isCSRFError(err)) {
            setBanner(membershipMessage, "Token CSRF tidak valid. Silakan coba lagi.", true);
        } else if (err.status === 403) {
            setBanner(membershipMessage, "Akses ditolak. Kamu tidak boleh mengubah membership.", true);
        } else if (err.status === 404) {
            setBanner(membershipMessage, "Keanggotaan tidak ditemukan. Muat ulang halaman dan coba lagi.", true);
        } else {
            setBanner(membershipMessage, err.message || "Gagal mengeluarkan anggota.", true);
        }
    } finally {
        membershipSaving = false;
        updateGroupChoices();
        renderGroupsTable();
    }
}

async function loadGroups() {
    status.hidden = false;
    status.textContent = "Memeriksa akses...";
    error.hidden = true;
    empty.hidden = true;
    section.hidden = true;

    try {
        const user = await requireAuthenticatedUser();
        if (!user) return false;
        currentUserId = String(user.id);

        if (!user.is_platform_admin) {
            status.hidden = true;
            createSection.hidden = true;
            error.textContent = "Akses ditolak. Halaman ini hanya untuk Platform Admin.";
            error.hidden = false;
            return false;
        }

        createSection.hidden = false;
        status.textContent = "Memuat daftar group...";
        const result = await authenticatedRequest("/api/v1/admin/groups");
        if (!Array.isArray(result?.groups)) {
            throw new Error("Format daftar group dari backend tidak valid.");
        }

        groupsData = result.groups;
        renderGroupsTable();
        if (usersData !== null) updateGroupChoices();

        status.hidden = true;
        empty.hidden = result.groups.length !== 0;
        section.hidden = result.groups.length === 0;
        return true;
    } catch (err) {
        status.hidden = true;
        if (err.status === 401) {
            window.location.replace("/login");
            return false;
        }
        error.textContent = err.status === 403
            ? "Akses ditolak. Kamu tidak memiliki izin melihat group."
            : (err.message || "Gagal memuat daftar group.");
        error.hidden = false;
        return false;
    }
}

createForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (createButton.disabled) return;

    setBanner(createMessage, "");
    setFieldError(groupName, groupNameError, "");

    const name = groupName.value.trim();
    const nameLength = utf8Bytes(name);
    if (nameLength < 1 || nameLength > 100) {
        setFieldError(
            groupName,
            groupNameError,
            "Isi nama group dengan maksimal 100 karakter biasa; karakter tertentu dapat dihitung lebih dari satu."
        );
        groupName.focus();
        return;
    }

    createButton.disabled = true;
    createButton.textContent = "Membuat...";

    try {
        await authenticatedRequest("/api/v1/admin/groups", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name })
        });
        createForm.reset();
        setFieldError(groupName, groupNameError, "");

        const refreshed = await loadGroups();
        setBanner(
            createMessage,
            refreshed
                ? "Group berhasil dibuat. Daftar group sudah diperbarui."
                : "Group berhasil dibuat, tetapi daftar belum dapat diperbarui.",
            !refreshed
        );
    } catch (err) {
        if (err.status === 401) {
            window.location.replace("/login");
            return;
        }
        if (err.status === 409) {
            setFieldError(groupName, groupNameError, "Nama group sudah digunakan.");
            groupName.focus();
            setBanner(createMessage, "Gunakan nama group yang berbeda.", true);
        } else if (err.status === 403 && isCSRFError(err)) {
            setBanner(createMessage, "Token CSRF tidak valid. Silakan klik Buat Group lagi.", true);
        } else if (err.status === 403) {
            setBanner(createMessage, "Akses ditolak. Hanya Platform Admin yang boleh membuat group.", true);
        } else {
            setBanner(createMessage, err.message || "Gagal membuat group.", true);
        }
    } finally {
        createButton.disabled = false;
        createButton.textContent = "Buat Group";
    }
});

membershipGroup.addEventListener("change", () => {
    setBanner(membershipMessage, "");
    updateUserChoices();
});
membershipUser.addEventListener("change", () => {
    setBanner(membershipMessage, "");
    updateUserChoices();
});

membershipForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (membershipSaving) return;

    const group = groupsData.find((item) => String(item.id) === membershipGroup.value);
    const user = usersData?.find((item) => String(item.id) === membershipUser.value);
    if (!group || !user || (group.user_ids ?? []).map(String).includes(String(user.id))) {
        setBanner(membershipMessage, "Pilih group dan pengguna yang belum menjadi anggota.", true);
        return;
    }

    membershipSaving = true;
    membershipGroup.disabled = true;
    membershipUser.disabled = true;
    membershipSubmit.disabled = true;
    membershipSubmit.textContent = "Menambahkan...";
    setBanner(membershipMessage, "");

    try {
        await authenticatedRequest(
            `/api/v1/admin/groups/${encodeURIComponent(String(group.id))}/users/${encodeURIComponent(String(user.id))}`,
            { method: "PUT" }
        );
        const refreshed = await loadGroups();
        setBanner(
            membershipMessage,
            refreshed
                ? `${user.username} berhasil ditambahkan ke group ${group.name}.`
                : "Anggota berhasil ditambahkan, tetapi daftar group gagal diperbarui.",
            !refreshed
        );
    } catch (err) {
        if (err.status === 401) {
            window.location.replace("/login");
            return;
        }
        if (err.status === 403 && isCSRFError(err)) {
            setBanner(membershipMessage, "Token CSRF tidak valid. Silakan coba lagi.", true);
        } else if (err.status === 403) {
            setBanner(membershipMessage, "Akses ditolak. Hanya Platform Admin yang boleh mengubah membership.", true);
        } else if (err.status === 404) {
            setBanner(membershipMessage, "Group atau user tidak ditemukan. Refresh halaman lalu coba lagi.", true);
        } else {
            setBanner(membershipMessage, err.message || "Gagal menambahkan anggota.", true);
        }
    } finally {
        membershipSaving = false;
        membershipSubmit.textContent = "Tambahkan Anggota";
        if (usersData !== null) updateGroupChoices();
    }
});

async function initializePage() {
    const loaded = await loadGroups();
    if (!loaded) return;

    try {
        const result = await authenticatedRequest("/api/v1/admin/users");
        if (!Array.isArray(result?.users)) {
            throw new Error("Format daftar pengguna dari backend tidak valid.");
        }
        usersData = result.users;
        updateGroupChoices();
        renderGroupsTable();
    } catch (err) {
        if (err.status === 401) {
            window.location.replace("/login");
            return;
        }
        membershipSection.hidden = false;
        setBanner(
            membershipMessage,
            err.status === 403
                ? "Akses ditolak saat mengambil daftar pengguna."
                : (err.message || "Gagal memuat daftar pengguna."),
            true
        );
    }
}

initializePage();
