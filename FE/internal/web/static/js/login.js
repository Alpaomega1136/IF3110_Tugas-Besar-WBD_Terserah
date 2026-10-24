import { apiRequest } from "./api.js";

const form = document.getElementById("login-form");
const button = document.getElementById("login-button");
const message = document.getElementById("login-message");

form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const formData = new FormData(form);

    button.disabled = true;
    button.textContent = "Logging in...";
    message.textContent = "";
    message.className = "";

    try {
        await apiRequest("/api/v1/auth/login", {
            method: "POST",
            headers: {
                "Content-Type": "application/json",
            },
            body: JSON.stringify({
                username: formData.get("username"),
                password: formData.get("password"),
            }),
        });

        form.reset();
        window.location.replace("/dashboard");
    } catch (error) {
        message.textContent = error.message || "Tidak dapat menghubungi server";
        message.className = "error-message";

        button.disabled = false;
        button.textContent = "Login";
    }
});
