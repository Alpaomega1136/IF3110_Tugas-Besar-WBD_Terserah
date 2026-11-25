import { apiRequest } from "./api.js";
import { navigateTo } from "./ui.js";

let csrfToken = null;
let csrfPending = null;
let authVersion = 0;
let sessionVersion = 0;

export function clearAuthState() {
    authVersion += 1;
    csrfToken = null;
    csrfPending = null;
}

export function resetAuthSession() {
    sessionVersion += 1;
    clearAuthState();
}

export function isCSRFError(error) {
    return error?.status === 403 && error.code === "csrf_rejected";
}

export async function getCurrentUser() {
    const principal = await apiRequest("/api/v1/auth/me");
    if (typeof principal?.id !== "string" || typeof principal.username !== "string" ||
        typeof principal.is_platform_admin !== "boolean") {
        throw new Error("Format principal dari backend tidak valid.");
    }
    return principal;
}

export async function requireAuthenticatedUser() {
    const adminNav = document.getElementById("admin-nav");
    const logoutButton = document.getElementById("logout-button");
    const navStatus = document.getElementById("navigation-status");
    try {
        const user = await getCurrentUser();
        if (logoutButton?.isConnected === false) return null;
        if (adminNav) adminNav.hidden = !user.is_platform_admin;
        if (logoutButton) {
            logoutButton.disabled = false;
            logoutButton.addEventListener("click", async () => {
                logoutButton.disabled = true;
                if (navStatus) navStatus.hidden = true;
                try {
                    await logout();
                    navigateTo("/login", { replace: true });
                } catch (error) {
                    if (navStatus) {
                        navStatus.textContent = error.message || "Logout gagal. Silakan coba lagi.";
                        navStatus.hidden = false;
                    }
                    logoutButton.disabled = false;
                }
            });
        }
        return user;
    } catch (error) {
        if (error.status === 401) {
            if (logoutButton?.isConnected !== false) {
                clearAuthState();
                navigateTo("/login", { replace: true });
            }
            return null;
        }
        throw error;
    }
}

export async function getCSRFToken() {
    if (csrfToken) return csrfToken;
    if (csrfPending) return csrfPending;

    const version = authVersion;
    const pending = apiRequest("/api/v1/auth/csrf").then((data) => {
        if (version !== authVersion) throw new Error("Sesi berubah. Silakan ulangi tindakan.");
        if (typeof data?.csrf_token !== "string" || !data.csrf_token) {
            throw new Error("Token CSRF tidak tersedia.");
        }
        csrfToken = data.csrf_token;
        return csrfToken;
    });
    csrfPending = pending;

    try {
        return await pending;
    } finally {
        if (csrfPending === pending) csrfPending = null;
    }
}

export async function authenticatedRequest(path, options = {}) {
    const session = sessionVersion;
    const method = (options.method ?? "GET").toUpperCase();
    const headers = new Headers(options.headers);
    if (!["GET", "HEAD", "OPTIONS"].includes(method)) {
        headers.set("X-CSRF-Token", await getCSRFToken());
    }

    let result;
    try {
        result = await apiRequest(path, { ...options, headers });
    } catch (error) {
        if (session === sessionVersion) {
            if (error.status === 401 || isCSRFError(error)) clearAuthState();
            throw error;
        }
    }
    if (session !== sessionVersion) throw new Error("Sesi berubah. Silakan ulangi tindakan.");
    return result;
}

export async function logout() {
    try {
        await authenticatedRequest("/api/v1/auth/logout", { method: "POST" });
    } catch (error) {
        if (error.status !== 401) {
            clearAuthState();
            throw error;
        }
    }
    resetAuthSession();
}
