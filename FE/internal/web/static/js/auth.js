
import { apiRequest } from "./api.js";

let csrfToken = null;
let csrfPending = null;
let authVersion = 0;

export function clearAuthState() {
    authVersion += 1;
    csrfToken = null;
    csrfPending = null;
}

export async function getCurrentUser() {
    const data = await apiRequest("/api/v1/auth/me");
    return data.user;
}

export async function requireAuthenticatedUser() {
    try {
        return await getCurrentUser();
    } catch (error) {
        if (error.status === 401) {
            clearAuthState();
            window.location.replace("/login");
            return null;
        }
        throw error;
    }
}


export async function getCSRFToken() {
    if (csrfToken) return csrfToken;
    if (csrfPending) return csrfPending;

    const requestVersion = authVersion;

    const request = apiRequest("/api/v1/auth/csrf").then((data) => {
            if (requestVersion !== authVersion) {
                throw new Error("Sesi berubah. Silakan ulangi tindakan.");
            }

            if (typeof data?.csrf_token !== "string" || !data.csrf_token) {
                throw new Error("Token CSRF tidak tersedia.");
            }

            csrfToken = data.csrf_token;
            return csrfToken;
        });

    csrfPending = request;

    try {
        return await request;
    } finally {
        if (csrfPending === request) {
            csrfPending = null;
        }
    }
}

export async function authenticatedRequest(path, options = {}) {
    const method = (options.method ?? "GET").toUpperCase();
    const headers = new Headers(options.headers);

    if (!["GET", "HEAD", "OPTIONS"].includes(method)) {
        headers.set("X-CSRF-Token", await getCSRFToken());
    }

    try {
        return await apiRequest(path, {
            ...options,
            headers,
        });
    } catch (error) {
        if (error.status === 401 || error.code === "csrf_invalid") {
            clearAuthState();
        }
        throw error;
    }
}

export async function logout() {
    try {
        await authenticatedRequest("/api/v1/auth/logout", {
            method: "POST",
        });
    } catch (error) {
        if (error.status !== 401) throw error;
    }
    clearAuthState();
}
