const encoder = new TextEncoder();
const dateFormatter = new Intl.DateTimeFormat("id-ID", {
    dateStyle: "medium",
    timeStyle: "short"
});

export const byId = (id) => document.getElementById(id);

export const utf8Bytes = (text) => encoder.encode(text).length;

export function formatDate(value, { nullLabel = "-", invalidLabel = "-" } = {}) {
    if (value == null || value === "") return nullLabel;
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? invalidLabel : dateFormatter.format(date);
}

export function addCell(row, value) {
    const cell = document.createElement("td");
    cell.textContent = String(value ?? "-");
    row.appendChild(cell);
    return cell;
}

export function addOption(select, value, text) {
    const option = document.createElement("option");
    option.value = value;
    option.textContent = text;
    select.appendChild(option);
    return option;
}

export function setBanner(element, message, isError = false, extraClass = "") {
    const base = isError ? "error-message" : "success-message";
    element.textContent = message;
    element.className = extraClass ? `${base} ${extraClass}` : base;
    element.hidden = !message;
}

export function setFieldError(input, errorElement, message, helpElement = null) {
    errorElement.textContent = message;
    errorElement.hidden = !message;
    if (helpElement) helpElement.hidden = Boolean(message);
    if (message) {
        input.setAttribute("aria-invalid", "true");
    } else {
        input.removeAttribute("aria-invalid");
    }
}

export function navigateTo(path, { replace = false } = {}) {
    if (typeof globalThis.window?.__spaNavigate === "function") {
        globalThis.window.__spaNavigate(path, { replace });
        return;
    }
    globalThis.window?.location?.replace?.(path);
}
