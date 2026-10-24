import { requireAuthenticatedUser, logout } from "./auth.js";

const status = document.getElementById("dashboard-status");
const content = document.getElementById("dashboard-content");
const username = document.getElementById("welcome-username");
const role = document.getElementById("account-role");
const adminPanel = document.getElementById("admin-panel");
const logoutButton = document.getElementById("logout-button");

async function loadDashboard() {
    try {
        const user = await requireAuthenticatedUser();

        if (!user) return;

        username.textContent = user.username;
        role.textContent = user.is_platform_admin ? "Platform Admin" : "Pengguna";

        adminPanel.hidden = !user.is_platform_admin;
        content.hidden = false;
        status.hidden = true;
        logoutButton.disabled = false;
    } catch (error) {
        status.textContent = error.message || "Gagal memuat dashboard.";
        status.className = "error-message";
    }
}

logoutButton.addEventListener("click", async () => {
    logoutButton.disabled = true;
    status.hidden = false;
    status.className = "";
    status.textContent = "Sedang logout...";

    try {
        await logout();
        window.location.replace("/login");
    } catch (error) {
        status.className = "error-message";
        status.textContent = error.message || "Logout gagal. Silakan coba lagi.";
        logoutButton.disabled = false;
    }
});

loadDashboard();
