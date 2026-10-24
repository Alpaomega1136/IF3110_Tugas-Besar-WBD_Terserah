export class ApiError extends Error {
    constructor(message, status = 0, code = "network_error") {
        super(message);
        this.name = "ApiError";
        this.status = status;
        this.code = code;
    }
}

export async function apiRequest(path, options = {}) {
    let response;

    try {
        response = await fetch(path, {...options, credentials: "same-origin"});
    } catch {
        throw new ApiError(
            "Tidak dapat terhubung ke server."
        );
    }

    const raw = await response.text();
    let data = null;

    if (raw) {
        try {
            data = JSON.parse(raw);
        } catch {

        }
    }

    if (!response.ok) {
        const code = data?.error?.code || "http_error";
        const message = data?.error?.message || (response.status === 502 ? "Backend API tidak tersedia." : `Permintaan gagal (HTTP ${response.status}).`);
        throw new ApiError(message, response.status, code);
    }

    return data;
}
