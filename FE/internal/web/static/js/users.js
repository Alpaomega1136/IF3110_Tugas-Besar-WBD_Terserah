import { confirmAction } from "./confirmation.js";
import { authenticatedRequest, requireAuthenticatedUser, isCSRFError } from "./auth.js";
import { byId, formatDate, setBanner, setFieldError, utf8Bytes } from "./ui.js";

const status = byId("users-status");
const errorMessage = byId("users-error");
const empty = byId("users-empty");
const section = byId("users-section");
const body = byId("users-body");

const createSection = byId("create-user-section");
const createForm = byId("create-user-form");
const usernameInput = byId("create-username");
const passwordInput = byId("create-password");
const adminInput = byId("create-is-admin");
const createButton = byId("create-user-submit");
const createMessage = byId("create-user-message");
const usernameError = byId("create-username-error");
const passwordError = byId("create-password-error");
const passwordHelp = byId("create-password-help");

const editSection = byId("edit-user-section");
const editForm = byId("edit-user-form");
const editUserName = byId("edit-user-name");
const editPassword = byId("edit-password");
const editPasswordError = byId("edit-password-error");
const editPasswordHelp = byId("edit-password-help");
const editAdmin = byId("edit-is-admin");
const editActive = byId("edit-is-active");
const editSubmit = byId("edit-user-submit");
const editCancel = byId("edit-user-cancel");
const editMessage = byId("edit-user-message");

const PASSWORD_RANGE_MSG = "Panjang password tidak sesuai. Gunakan 12–1024 huruf, angka, atau simbol biasa; karakter tertentu dapat dihitung lebih dari satu.";

let currentUserId = null;
let selectedUser = null;
let editSaving = false;

function showError(message) {
    errorMessage.textContent = message;
    errorMessage.hidden = false;
}

function validPasswordBytes(password) {
    const len = utf8Bytes(password);
    return len >= 12 && len <= 1024;
}

function renderUserRow(item) {
    const row = document.createElement("li");
    row.className = "users-list-item";

    const details = document.createElement("div");
    details.className = "users-list-details";

    const name = document.createElement("strong");
    name.textContent = item.username;

    const role = document.createElement("p");
    role.textContent = `${item.is_platform_admin ? "Platform Admin" : "User"} · ${item.is_active ? "Aktif" : "Nonaktif"}`;

    const meta = document.createElement("p");
    const groupCount = Array.isArray(item.group_ids) ? item.group_ids.length : "-";
    meta.textContent = `ID ${item.id} · Group: ${groupCount} · Dibuat: ${formatDate(item.created_at)}`;

    details.appendChild(name);
    details.appendChild(role);
    details.appendChild(meta);

    const editButton = document.createElement("button");
    editButton.type = "button";
    editButton.className = "users-edit-button";
    editButton.textContent = "Edit";
    editButton.setAttribute("aria-label", `Edit pengguna ${item.username}`);
    editButton.addEventListener("click", () => openEditUser(item));

    row.appendChild(details);
    row.appendChild(editButton);
    return row;
}

async function refreshUsers() {
    status.hidden = false;
    status.textContent = "Memuat daftar pengguna...";
    errorMessage.hidden = true;
    empty.hidden = true;
    section.hidden = true;

    try {
        const result = await authenticatedRequest("/api/v1/admin/users");
        if (!Array.isArray(result?.users)) {
            throw new Error("Format daftar pengguna dari backend tidak valid.");
        }

        body.replaceChildren();
        for (const item of result.users) {
            body.appendChild(renderUserRow(item));
        }

        status.hidden = true;
        empty.hidden = result.users.length !== 0;
        section.hidden = result.users.length === 0;
        return true;
    } catch (error) {
        status.hidden = true;
        if (error.status === 401) {
            window.location.replace("/login");
            return false;
        }
        showError(error.status === 403
            ? "Akses ditolak. Halaman ini hanya untuk Platform Admin."
            : (error.message || "Gagal memuat daftar pengguna."));
        return false;
    }
}

function validateCreateInput() {
    const username = usernameInput.value.trim();
    const password = passwordInput.value;

    setFieldError(usernameInput, usernameError, "");
    setFieldError(passwordInput, passwordError, "", passwordHelp);

    if (!username || /[ \t\r\n]/.test(username) || utf8Bytes(username) > 100) {
        setFieldError(
            usernameInput,
            usernameError,
            "Isi username tanpa spasi, maksimal 100 huruf atau angka biasa. Karakter tertentu dapat dihitung lebih dari satu."
        );
    }
    if (!validPasswordBytes(password)) {
        setFieldError(passwordInput, passwordError, PASSWORD_RANGE_MSG, passwordHelp);
    }
    if (!usernameError.hidden || !passwordError.hidden) {
        (usernameError.hidden ? passwordInput : usernameInput).focus();
        return null;
    }
    return { username, password, is_platform_admin: adminInput.checked };
}

createForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(createMessage, "");

    const payload = validateCreateInput();
    if (!payload) return;

    createButton.disabled = true;
    createButton.textContent = "Membuat...";

    try {
        await authenticatedRequest("/api/v1/admin/users", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        });
        createForm.reset();
        setFieldError(usernameInput, usernameError, "");
        setFieldError(passwordInput, passwordError, "", passwordHelp);

        const refreshed = await refreshUsers();
        setBanner(
            createMessage,
            refreshed
                ? "Pengguna berhasil dibuat. Daftar pengguna sudah diperbarui."
                : "Pengguna berhasil dibuat, tetapi daftar belum dapat diperbarui.",
            !refreshed
        );
    } catch (error) {
        if (error.status === 401) {
            window.location.replace("/login");
            return;
        }
        if (error.status === 409) {
            setFieldError(usernameInput, usernameError, "Username sudah digunakan.");
            usernameInput.focus();
            setBanner(createMessage, "Gunakan username lain.", true);
        } else if (error.status === 403 && isCSRFError(error)) {
            setBanner(createMessage, "Token CSRF tidak valid. Silakan klik Buat Pengguna lagi.", true);
        } else if (error.status === 403) {
            setBanner(createMessage, "Akses ditolak. Hanya Platform Admin yang boleh membuat user.", true);
        } else {
            setBanner(createMessage, error.message || "Gagal membuat pengguna.", true);
        }
    } finally {
        createButton.disabled = false;
        createButton.textContent = "Buat Pengguna";
    }
});

function openEditUser(user) {
    if (editSaving) return;
    selectedUser = user;
    editForm.reset();
    editUserName.textContent = user.username;
    editAdmin.checked = user.is_platform_admin;
    editActive.checked = user.is_active;

    setFieldError(editPassword, editPasswordError, "", editPasswordHelp);
    setBanner(editMessage, "");
    editSection.hidden = false;
    editSection.scrollIntoView({ behavior: "smooth", block: "start" });
    editAdmin.focus({ preventScroll: true });
}

function closeEditUser() {
    if (editSaving) return;
    selectedUser = null;
    editForm.reset();
    editSection.hidden = true;
    setBanner(editMessage, "");
    setFieldError(editPassword, editPasswordError, "", editPasswordHelp);
}

editCancel.addEventListener("click", closeEditUser);

editForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!selectedUser || editSaving) return;

    setBanner(editMessage, "");
    setFieldError(editPassword, editPasswordError, "", editPasswordHelp);

    const payload = {};
    if (editAdmin.checked !== selectedUser.is_platform_admin) {
        payload.is_platform_admin = editAdmin.checked;
    }
    if (editActive.checked !== selectedUser.is_active) {
        payload.is_active = editActive.checked;
    }

    const newPassword = editPassword.value;
    if (newPassword !== "") {
        if (!validPasswordBytes(newPassword)) {
            setFieldError(editPassword, editPasswordError, PASSWORD_RANGE_MSG, editPasswordHelp);
            editPassword.focus();
            return;
        }
        payload.password = newPassword;
    }

    if (Object.keys(payload).length === 0) {
        setBanner(editMessage, "Tidak ada perubahan yang dilakukan.", true);
        return;
    }

    const userId = String(selectedUser.id);
    const selfSessionRevoked = userId === currentUserId && (
        Object.hasOwn(payload, "password") ||
        payload.is_active === false ||
        payload.is_platform_admin === false
    );

    if (selfSessionRevoked) {
        const confirmed = await confirmAction({
            title: "Ubah akun sendiri?",
            message: "Perubahan ini akan mengakhiri session akunmu. Kamu perlu login kembali. Lanjutkan?",
            confirmLabel: "Simpan Perubahan",
            tone: "danger"
        });
        if (!confirmed) return;
    }

    if (editSaving) return;
    editSaving = true;
    editSubmit.disabled = true;
    editCancel.disabled = true;
    editSubmit.textContent = "Menyimpan...";

    try {
        const result = await authenticatedRequest(`/api/v1/admin/users/${encodeURIComponent(userId)}`, {
            method: "PATCH",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        });
        if (selfSessionRevoked) {
            window.location.replace("/login");
            return;
        }
        selectedUser = result.user;
        editPassword.value = "";
        editAdmin.checked = result.user.is_platform_admin;
        editActive.checked = result.user.is_active;

        const refreshed = await refreshUsers();
        setBanner(
            editMessage,
            refreshed
                ? "Perubahan pengguna berhasil disimpan."
                : "Perubahan tersimpan, tetapi daftar gagal diperbarui.",
            !refreshed
        );
    } catch (error) {
        if (error.status === 401) {
            window.location.replace("/login");
            return;
        }
        if (error.status === 403 && isCSRFError(error)) {
            setBanner(editMessage, "Token CSRF tidak valid. Silakan coba simpan lagi.", true);
        } else if (error.status === 409 && error.code === "group_leader_active") {
            setBanner(editMessage, "Pengguna ini masih menjadi Leader Group aktif. Ganti leader group terlebih dahulu sebelum menonaktifkan akun.", true);
        } else if (error.status === 403) {
            setBanner(editMessage, "Akses ditolak. Kamu tidak memiliki izin mengubah pengguna.", true);
        } else if (error.status === 404) {
            setBanner(editMessage, "Pengguna tidak ditemukan. Muat ulang daftar pengguna.", true);
        } else {
            setBanner(editMessage, error.message || "Gagal mengubah pengguna.", true);
        }
    } finally {
        editSaving = false;
        editSubmit.disabled = false;
        editCancel.disabled = false;
        editSubmit.textContent = "Simpan Perubahan";
    }
});

async function initialize() {
    try {
        const user = await requireAuthenticatedUser();
        if (!user) return;
        if (!user.is_platform_admin) {
            status.hidden = true;
            showError("Akses ditolak. Halaman ini hanya untuk Platform Admin.");
            return;
        }
        currentUserId = String(user.id);
        createSection.hidden = false;
        await refreshUsers();
    } catch (error) {
        status.hidden = true;
        showError(error.message || "Gagal memeriksa session.");
    }
}

initialize();
