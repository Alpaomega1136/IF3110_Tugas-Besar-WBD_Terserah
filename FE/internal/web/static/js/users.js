import { confirmAction } from "./confirmation.js";
import {
    authenticatedRequest,
    requireAuthenticatedUser,
    isCSRFError
} from "./auth.js";

const status = document.getElementById("users-status");
const errorMessage = document.getElementById("users-error");
const empty = document.getElementById("users-empty");
const section = document.getElementById("users-section");
const body = document.getElementById("users-body");
const createSection = document.getElementById("create-user-section");
const createForm = document.getElementById("create-user-form");
const usernameInput = document.getElementById("create-username");
const passwordInput = document.getElementById("create-password");
const adminInput = document.getElementById("create-is-admin");
const createButton = document.getElementById("create-user-submit");
const createMessage = document.getElementById("create-user-message");
const usernameError = document.getElementById("create-username-error");
const passwordError = document.getElementById("create-password-error");
const passwordHelp = document.getElementById("create-password-help");

const editSection = document.getElementById("edit-user-section");
const editForm = document.getElementById("edit-user-form");
const editUserName = document.getElementById("edit-user-name");
const editPassword = document.getElementById("edit-password");
const editPasswordError = document.getElementById("edit-password-error");
const editPasswordHelp = document.getElementById("edit-password-help");
const editAdmin = document.getElementById("edit-is-admin");
const editActive = document.getElementById("edit-is-active");
const editSubmit = document.getElementById("edit-user-submit");
const editCancel = document.getElementById("edit-user-cancel");
const editMessage = document.getElementById("edit-user-message");

let currentUserId = null;
let selectedUser = null;
let editSaving = false;

function formatDate(value) {
    if (!value) return "-";

    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "-";

    return new Intl.DateTimeFormat("id-ID", {
        dateStyle: "medium",
        timeStyle: "short",
    }).format(date);
}

function showError(message) {
    errorMessage.textContent = message;
    errorMessage.hidden = false;
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
            const row = document.createElement("li");
            row.className = "users-list-item";
            const details = document.createElement("div");
            details.className = "users-list-details";
            const name = document.createElement("strong");
            name.textContent = item.username;
            const role = document.createElement("p");
            role.textContent = `${item.is_platform_admin ? "Platform Admin" : "User"} · ${item.is_active ? "Aktif" : "Nonaktif"}`;
            const meta = document.createElement("p");
            meta.textContent = `ID ${item.id} · Group: ${Array.isArray(item.group_ids) ? item.group_ids.length : "-"} · Dibuat: ${formatDate(item.created_at)}`;
            details.appendChild(name);
            details.appendChild(role);
            details.appendChild(meta);
            const editButton = document.createElement("button");

            editButton.type = "button";
            editButton.className = "users-edit-button";
            editButton.textContent = "Edit";
            editButton.setAttribute("aria-label", `Edit pengguna ${item.username}`);

            editButton.addEventListener("click", () => {
                openEditUser(item);
            });

            row.appendChild(details);
            row.appendChild(editButton);

            body.appendChild(row);
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

function setFieldError(input, target, message) {
    target.textContent = message;
    target.hidden = !message;
    if (input === passwordInput) passwordHelp.hidden = !!message;
    if (input === editPassword) editPasswordHelp.hidden = !!message;

    if (message) {
        input.setAttribute("aria-invalid", "true");
    } else {
        input.removeAttribute("aria-invalid");
    }
}

function setCreateMessage(message, isError = false) {
    createMessage.textContent = message;
    createMessage.className = isError ? "error-message" : "success-message";
    createMessage.hidden = !message;
}

function validateCreateInput() {
    const username = usernameInput.value.trim();
    const password = passwordInput.value;
    const utf8 = new TextEncoder();

    setFieldError(usernameInput, usernameError, "");
    setFieldError(passwordInput, passwordError, "");

    if (!username || /[ \t\r\n]/.test(username) || utf8.encode(username).length > 100) {
        setFieldError(
            usernameInput,
            usernameError,
            "Isi username tanpa spasi, maksimal 100 huruf atau angka biasa. Karakter tertentu dapat dihitung lebih dari satu."
        );
    }

    const passwordBytes = utf8.encode(password).length;

    if (passwordBytes < 12 || passwordBytes > 1024) {
        setFieldError(
            passwordInput,
            passwordError,
            "Panjang password tidak sesuai. Gunakan 12–1024 huruf, angka, atau simbol biasa; karakter tertentu dapat dihitung lebih dari satu."
        );
    }

    if (!usernameError.hidden || !passwordError.hidden) {
        (usernameError.hidden ? passwordInput : usernameInput).focus();
        return null;
    }

    return {
        username,
        password,
        is_platform_admin: adminInput.checked,
    };
}

createForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    setCreateMessage("");

    const payload = validateCreateInput();
    if (!payload) return;

    createButton.disabled = true;
    createButton.textContent = "Membuat...";

    try {
        await authenticatedRequest("/api/v1/admin/users", {
            method: "POST",
            headers: {
                "Content-Type": "application/json",
            },
            body: JSON.stringify(payload),
        });

        createForm.reset();
        setFieldError(usernameInput, usernameError, "");
        setFieldError(passwordInput, passwordError, "");

        const refreshed = await refreshUsers();

        setCreateMessage(
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
            setCreateMessage("Gunakan username lain.", true);
        } else if (error.status === 403 && isCSRFError(error)) {
            setCreateMessage("Token CSRF tidak valid. Silakan klik Buat Pengguna lagi.", true);
        } else if (error.status === 403) {
            setCreateMessage("Akses ditolak. Hanya Platform Admin yang boleh membuat user.", true);
        } else {
            setCreateMessage(error.message || "Gagal membuat pengguna.", true);
        }
    } finally {
        createButton.disabled = false;
        createButton.textContent = "Buat Pengguna";
    }
});

function setEditMessage(message, isError = false) {
    editMessage.textContent = message;
    editMessage.className = isError
        ? "error-message"
        : "success-message";
    editMessage.hidden = !message;
}

function openEditUser(user) {
    if (editSaving) return;

    selectedUser = user;

    editForm.reset();
    editUserName.textContent = user.username;

    editAdmin.checked = user.is_platform_admin;
    editActive.checked = user.is_active;

    setFieldError(editPassword, editPasswordError, "");
    setEditMessage("");

    editSection.hidden = false;
    editSection.scrollIntoView({
        behavior: "smooth",
        block: "start"
    });

    editAdmin.focus({ preventScroll: true });
}

function closeEditUser() {
    if (editSaving) return;

    selectedUser = null;
    editForm.reset();
    editSection.hidden = true;

    setEditMessage("");
    setFieldError(editPassword, editPasswordError, "");
}

editCancel.addEventListener("click", closeEditUser);

editForm.addEventListener("submit", async (event) => {
    event.preventDefault();

    if (!selectedUser || editSaving) return;

    setEditMessage("");
    setFieldError(editPassword, editPasswordError, "");

    const payload = {};

    if (editAdmin.checked !== selectedUser.is_platform_admin) {
        payload.is_platform_admin = editAdmin.checked;
    }

    if (editActive.checked !== selectedUser.is_active) {
        payload.is_active = editActive.checked;
    }

    const newPassword = editPassword.value;

    if (newPassword !== "") {
        const passwordLength =
            new TextEncoder().encode(newPassword).length;

        if (passwordLength < 12 || passwordLength > 1024) {
            setFieldError(
                editPassword,
                editPasswordError,
                "Panjang password tidak sesuai. Gunakan 12–1024 huruf, angka, atau simbol biasa; karakter tertentu dapat dihitung lebih dari satu."
            );
            editPassword.focus();
            return;
        }

        payload.password = newPassword;
    }

    if (Object.keys(payload).length === 0) {
        setEditMessage("Tidak ada perubahan yang dilakukan.", true);
        return;
    }

    const userId = String(selectedUser.id);

    const selfSessionRevoked =
        userId === currentUserId &&
        (
            Object.hasOwn(payload, "password") ||
            payload.is_active === false ||
            payload.is_platform_admin === false
        );

    if (selfSessionRevoked) {
        const confirmed = await confirmAction({
            title: "Ubah akun sendiri?",
            message: "Perubahan ini akan mengakhiri session akunmu. " +
                "Kamu perlu login kembali. Lanjutkan?",
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
        const result = await authenticatedRequest(
            `/api/v1/admin/users/${encodeURIComponent(userId)}`,
            {
                method: "PATCH",
                headers: {
                    "Content-Type": "application/json"
                },
                body: JSON.stringify(payload)
            }
        );

        if (selfSessionRevoked) {
            window.location.replace("/login");
            return;
        }

        selectedUser = result.user;
        editPassword.value = "";
        editAdmin.checked = result.user.is_platform_admin;
        editActive.checked = result.user.is_active;

        const refreshed = await refreshUsers();

        setEditMessage(
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

        if (
            error.status === 403 &&
            isCSRFError(error)
        ) {
            setEditMessage(
                "Token CSRF tidak valid. Silakan coba simpan lagi.",
                true
            );
        } else if (error.status === 403) {
            setEditMessage(
                "Akses ditolak. Kamu tidak memiliki izin mengubah pengguna.",
                true
            );
        } else if (error.status === 404) {
            setEditMessage(
                "Pengguna tidak ditemukan. Muat ulang daftar pengguna.",
                true
            );
        } else {
            setEditMessage(
                error.message || "Gagal mengubah pengguna.",
                true
            );
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
