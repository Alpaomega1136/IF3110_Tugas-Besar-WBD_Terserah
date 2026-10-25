/**
 * Unified, accessible confirmation prompt for the server-rendered frontend.
 * Never use innerHTML for titles/messages: they can include database/user input.
 * In minimal test DOMs or browsers without native <dialog>, use the native
 * confirmation as a compatibility fallback; production pages include the dialog.
 */
export function confirmAction({
    title = "Konfirmasi tindakan",
    message,
    confirmLabel = "Lanjutkan",
    cancelLabel = "Batal",
    tone = "warning"
}) {
    if (typeof message !== "string" || !message.trim()) {
        throw new TypeError("Pesan konfirmasi harus diisi.");
    }

    const dialog = document.getElementById("global-confirm-dialog");
    if (!dialog || typeof dialog.showModal !== "function") {
        return Promise.resolve(Boolean(window.confirm(message)));
    }

    // Never allow a second action to piggyback on an existing confirmation.
    if (dialog.open) return Promise.resolve(false);

    const titleElement = document.getElementById("global-confirm-title");
    const messageElement = document.getElementById("global-confirm-message");
    const confirmButton = document.getElementById("global-confirm-accept");
    const cancelButton = document.getElementById("global-confirm-cancel");

    titleElement.textContent = title;
    messageElement.textContent = message;
    confirmButton.textContent = confirmLabel;
    cancelButton.textContent = cancelLabel;
    confirmButton.className = tone === "danger"
        ? "confirm-dialog-accept confirm-dialog-accept--danger"
        : "confirm-dialog-accept";
    dialog.returnValue = "";

    return new Promise((resolve) => {
        const done = () => {
            dialog.removeEventListener("close", done);
            resolve(dialog.returnValue === "confirm");
        };
        dialog.addEventListener("close", done);
        try {
            dialog.showModal();
            // Safer keyboard default, especially for destructive actions.
            cancelButton.focus();
        } catch {
            dialog.removeEventListener("close", done);
            resolve(false);
        }
    });
}
