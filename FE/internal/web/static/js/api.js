export async function apiRequest(path, options = {}) {
    const response = await fetch(path, {
        credentials: "same-origin",
        ...options,
    });
    const data = await response.json().catch(() => null);

    if (!response.ok) {
        throw new Error(data?.error?.message || `Request gagal (${response.status})`);
    }
    return data;
}
