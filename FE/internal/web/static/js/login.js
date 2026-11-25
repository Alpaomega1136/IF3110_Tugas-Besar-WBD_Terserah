import { authenticatedRequest, isCSRFError, resetAuthSession } from "./auth.js";
import { navigateTo } from "./ui.js";

const form = document.getElementById("login-form");
const button = document.getElementById("login-button");
const message = document.getElementById("login-message");

form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (button.disabled) return;

    const formData = new FormData(form);
    button.disabled = true;
    button.textContent = "Logging in...";
    message.textContent = "";
    message.className = "";

    try {
        const principal = await authenticatedRequest("/api/v1/auth/login", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                username: formData.get("username"),
                password: formData.get("password")
            })
        });
        if (typeof principal?.id !== "string" || typeof principal.username !== "string") {
            throw new Error("Format respons login dari backend tidak valid.");
        }
        // The prelogin token belongs to the old cookie; the new session has its own CSRF token.
        resetAuthSession();
        form.reset();
        navigateTo("/dashboard", { replace: true });
    } catch (error) {
        message.textContent = isCSRFError(error)
            ? "Validasi CSRF ditolak. Muat ulang halaman dan pastikan APP_ORIGIN sesuai origin frontend."
            : error.status === 401
                ? "Username atau password salah."
                : (error.message || "Tidak dapat menghubungi server.");
        message.className = "error-message";
        button.disabled = false;
        button.textContent = "Login";
    }
});
