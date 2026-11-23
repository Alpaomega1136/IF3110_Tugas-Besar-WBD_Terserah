// Single-Page Application (SPA) router using History API without full page reload.
let navGeneration = 0;
let navigating = false;

function shouldInterceptClick(event) {
    if (event.defaultPrevented || event.button !== 0 ||
        event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
        return null;
    }
    const anchor = event.target?.closest?.("a[href]");
    if (!anchor || anchor.target || anchor.hasAttribute("download")) return null;
    let url;
    try {
        url = new URL(anchor.href, window.location.origin);
    } catch {
        return null;
    }
    if (url.origin !== window.location.origin) return null;
    if (url.pathname.startsWith("/api/") || url.pathname.startsWith("/static/") || url.pathname.startsWith("/health/")) {
        return null;
    }
    return url.pathname + url.search;
}

export async function spaNavigate(path, { replace = false, pushState = true } = {}) {
    if (navigating) return false;
    if (typeof window.__spaBeforeNavigate === "function") {
        const allowed = await window.__spaBeforeNavigate(path);
        if (!allowed) return false;
    }

    navigating = true;
    try {
        const response = await fetch(path, {
            credentials: "same-origin",
            headers: { "X-SPA-Navigation": "1" }
        });
        if (!response.ok) {
            window.location.assign(path);
            return false;
        }
        const html = await response.text();
        const parsed = new DOMParser().parseFromString(html, "text/html");
        const nextRoot = parsed.querySelector(".app-shell, .login-container");
        const currentRoot = document.querySelector(".app-shell, .login-container");
        if (!nextRoot || !currentRoot) {
            window.location.assign(path);
            return false;
        }

        if (typeof window.__spaCleanup === "function") window.__spaCleanup();
        window.__spaCleanup = null;
        window.__spaBeforeNavigate = null;
        if (parsed.title) {
            document.title = parsed.title;
        }
        currentRoot.replaceWith(nextRoot);

        if (pushState) {
            if (replace) {
                window.history.replaceState({ path }, "", path);
            } else if (window.location.pathname + window.location.search !== path) {
                window.history.pushState({ path }, "", path);
            }
        }
        window.scrollTo?.(0, 0);

        const pageScripts = parsed.querySelectorAll('script[type="module"][src]');
        for (const script of pageScripts) {
            const src = script.getAttribute("src");
            if (!src || src.endsWith("/spa.js")) continue;
            await import(`${src}?spa=${++navGeneration}`);
        }
        return true;
    } catch {
        window.location.assign(path);
        return false;
    } finally {
        navigating = false;
    }
}

if (typeof window !== "undefined" && typeof document !== "undefined" && typeof DOMParser !== "undefined") {
    window.__spaNavigate = (path, options = {}) => spaNavigate(path, options);
    document.addEventListener("click", (event) => {
        const targetPath = shouldInterceptClick(event);
        if (!targetPath) return;
        event.preventDefault();
        spaNavigate(targetPath);
    });
    window.addEventListener("popstate", () => {
        spaNavigate(window.location.pathname + window.location.search, { pushState: false });
    });
}
