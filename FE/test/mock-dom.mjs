export class Element {
    constructor(tag = "div") {
        this.tagName = tag.toUpperCase();
        this.children = [];
        this.listeners = new Map();
        this.attributes = new Map();
        this.dataset = {};
        this.hidden = false;
        this.disabled = false;
        this.value = "";
        this.className = "";
        this._text = "";
    }

    get textContent() { return this._text + this.children.map((child) => child.textContent).join(""); }
    set textContent(value) { this._text = String(value); this.children = []; }
    appendChild(child) { this.children.push(child); return child; }
    append(...children) { for (const child of children) this.appendChild(child); }
    scrollIntoView() {}
    removeAttribute(key) { this.attributes.delete(key); }
    replaceChildren(...children) { this._text = ""; this.children = children; }
    addEventListener(name, fn) { this.listeners.set(name, fn); }
    setAttribute(key, value) { this.attributes.set(key, value); }
    focus() { this.focused = true; }
    querySelectorAll(tag) {
        return this.children.flatMap((child) => [
            ...(child.tagName.toLowerCase() === tag ? [child] : []), ...child.querySelectorAll(tag)
        ]);
    }
    async fire(name) {
        const fn = this.listeners.get(name);
        if (!fn) throw new Error(`No listener for ${name}`);
        return await fn({ preventDefault() {} });
    }
}

function form(fields) {
    const el = new Element("form");
    const fieldMap = new Map(Object.entries(fields).map(([name, value]) => [name, Object.assign(new Element("input"), { value })]));
    const controls = [...fieldMap.values()];
    controls.namedItem = (name) => fieldMap.get(name);
    el.elements = controls;
    el.reset = () => {
        for (const [name, input] of fieldMap) input.value = fields[name];
    };
    return el;
}

export function installDOM() {
    const ids = [
        "connections-status", "connections-error", "connections-forbidden", "connections-content",
        "connections-empty", "connections-table", "connections-body", "connections-refresh",
        "connection-action-message", "connection-create-error", "connection-create-submit",
        "connection-edit-section", "connection-edit-error", "connection-edit-cancel", "connection-edit-submit",
        "connection-edit-name", "connection-edit-host", "connection-edit-port", "connection-edit-database"
    ];
    const elements = Object.fromEntries(ids.map((id) => [id, new Element()]));
    for (const id of ["connections-error", "connections-forbidden", "connections-content", "connections-empty",
        "connections-table", "connection-action-message", "connection-create-error", "connection-edit-section",
        "connection-edit-error"]) elements[id].hidden = true;
    elements["connection-create-form"] = form({
        name: "", host: "", port: "5432", database_name: "", username: "", password: "", sslmode: "verify-full"
    });
    elements["connection-edit-form"] = form({ name: "", username: "", password: "", sslmode: "verify-full" });
    const redirects = [];
    let confirmed = true;
    globalThis.document = {
        getElementById: (id) => elements[id] ?? null,
        createElement: (tag) => new Element(tag)
    };
    globalThis.window = {
        location: { replace: (path) => redirects.push(path) },
        confirm: () => confirmed
    };
    return {
        elements, redirects,
        setConfirm: (value) => { confirmed = value; },
        field: (type, name) => elements[`connection-${type}-form`].elements.namedItem(name),
        actions: (rowIndex) => elements["connections-body"].children[rowIndex].children[7].children
    };
}

export async function tick() {
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
}
