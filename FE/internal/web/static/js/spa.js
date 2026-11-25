// Single-Page Application (SPA) router using History API without full page reload.
let navGeneration = 0;
let navigating = false;
let pendingNavigation = null;

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
    if (navigating) {
        if (replace || !pushState) pendingNavigation = { path, replace, pushState };
        return false;
    }

    navigating = true;
    const fallback = () => {
        if (pendingNavigation) return;
        if (replace) window.location.replace(path);
        else window.location.assign(path);
    };
    try {
        if (!replace && pushState && typeof window.__spaBeforeNavigate === "function") {
            const allowed = await window.__spaBeforeNavigate(path);
            if (!allowed || pendingNavigation) return false;
        }
        const response = await fetch(path, {
            credentials: "same-origin",
            headers: { "X-SPA-Navigation": "1" }
        });
        if (!response.ok) {
            fallback();
            return false;
        }
        const html = await response.text();
        if (pendingNavigation) return false;
        const parsed = new DOMParser().parseFromString(html, "text/html");
        const nextRoot = parsed.querySelector(".app-shell, .login-container");
        const currentRoot = document.querySelector(".app-shell, .login-container");
        if (!nextRoot || !currentRoot) {
            fallback();
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
        fallback();
        return false;
    } finally {
        navigating = false;
        if (pendingNavigation) {
            const next = pendingNavigation;
            pendingNavigation = null;
            void spaNavigate(next.path, next);
        }
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
