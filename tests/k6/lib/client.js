import http from 'k6/http';
import { check } from 'k6';

export const BASE_URL = (__ENV.K6_BASE_URL || 'http://localhost:3000').replace(/\/$/, '');
export const ORIGIN = __ENV.K6_APP_ORIGIN || BASE_URL;

export function required(name) {
    const value = __ENV[name];
    if (!value) throw new Error(`Missing environment variable: ${name}`);
    return value;
}

// Keep requester and approver cookies isolated, including during setup().
export function client(phase) {
    const jar = new http.CookieJar();
    let csrf = '';
    function request(method, path, body, expected, name, extraTags = {}) {
        const headers = { Origin: ORIGIN, Accept: 'application/json' };
        if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = csrf;
        if (body !== undefined && body !== null) headers['Content-Type'] = 'application/json';
        const res = http.request(method, BASE_URL + path,
            body == null ? null : JSON.stringify(body), {
                jar, headers, redirects: 0, timeout: '15s',
                tags: { ...extraTags, name, phase },
            });
        const ok = check(res, { [`${name}: expected status`]: r => expected.includes(r.status) });
        if (!ok) {
            let code = '';
            try { code = res.json('error.code') || ''; } catch (_) { /* no JSON */ }
            // Do not print request bodies, cookies, credentials or SQL.
            throw new Error(`${name}: HTTP ${res.status}${code ? ` (${code})` : ''}`);
        }
        return res.status === 204 ? null : res.json();
    }
    function refreshCSRF() {
        csrf = request('GET', '/api/v1/auth/csrf', null, [200], 'auth_csrf').csrf_token;
        if (typeof csrf !== 'string' || !csrf) throw new Error('Missing CSRF token');
    }
    return {
        request,
        login(username, password) {
            refreshCSRF();
            const principal = request('POST', '/api/v1/auth/login', { username, password }, [200], 'auth_login');
            refreshCSRF();
            const me = request('GET', '/api/v1/auth/me', null, [200], 'auth_me');
            if (me.username !== username || me.id !== principal.id) throw new Error('Unexpected authenticated principal');
            return me;
        },
        logout() { request('POST', '/api/v1/auth/logout', null, [204], 'auth_logout'); },
    };
}

export function requester(phase) {
    const api = client(phase);
    const user = api.login(required('K6_USERNAME'), required('K6_PASSWORD'));
    return { api, user };
}

export function approver(phase) {
    const api = client(phase);
    const user = api.login(required('K6_LEADER_USERNAME'), required('K6_LEADER_PASSWORD'));
    return { api, user };
}
