// The fetch layer. The session is a cookie the page cannot read (HttpOnly).
// Every write carries the CSRF token of the session in X-Fuel-CSRF. The Fuel
// token is sent once at sign-in and is never stored by the page.

import { sleep } from './util.js';

let csrf = '';
let unauthorized = () => {};

export function onUnauthorized(fn) { unauthorized = fn; }

export class ApiError extends Error {
  constructor(status, code, message, retryable, retryAfter) {
    super(message);
    this.status = status;
    this.code = code;
    this.retryable = !!retryable;
    this.retryAfter = retryAfter || 0;
  }
}

async function raw(method, path, body, headers) {
  const h = Object.assign({}, headers);
  if (method !== 'GET') h['X-Fuel-CSRF'] = csrf;
  let res;
  try {
    res = await fetch(path, { method, headers: h, body, credentials: 'same-origin', cache: 'no-store', redirect: 'error' });
  } catch (e) {
    throw new ApiError(0, 'network', 'No answer from the server.', true);
  }
  let data = null;
  if (res.status !== 204 && (res.headers.get('Content-Type') || '').includes('application/json')) {
    try { data = await res.json(); } catch (e) { data = null; }
  }
  return { res, data };
}

export async function loadSession() {
  try {
    const { res, data } = await raw('GET', '/fuel/session');
    if (res.status === 200 && data && typeof data.csrf === 'string') {
      csrf = data.csrf;
      return data;
    }
  } catch (e) { /* offline: no session known */ }
  return null;
}

// signIn sends the token once. The caller clears the field afterwards.
export async function signIn(token) {
  const { res, data } = await raw('POST', '/fuel/session', JSON.stringify({ token }), { 'Content-Type': 'application/json' });
  if (res.status === 204) return { ok: true };
  return { ok: false, status: res.status, code: data && data.error ? data.error.code : '', retryAfter: Number(res.headers.get('Retry-After')) || 0 };
}

export async function signOut(all) {
  try { await raw('DELETE', '/fuel/session' + (all ? '?all=1' : '')); } catch (e) { /* the cookie stays; the user can try again */ }
  csrf = '';
}

export async function call(method, path, opts) {
  const o = opts || {};
  let body;
  const headers = {};
  if (o.json !== undefined) {
    body = JSON.stringify(o.json);
    headers['Content-Type'] = 'application/json';
  } else if (o.form) {
    body = o.form;
  }
  let { res, data } = await raw(method, path, body, headers);
  if (res.status === 403 && data && data.error && data.error.code === 'csrf') {
    // The CSRF token was stale. The refused request wrote nothing; the same
    // body (the same client_id) is sent once more with the new token.
    if (await loadSession()) ({ res, data } = await raw(method, path, body, headers));
  }
  if (res.status === 401) {
    unauthorized();
    throw new ApiError(401, 'unauthorized', 'Signed out.', false);
  }
  if (!res.ok) {
    const e = (data && data.error) || {};
    throw new ApiError(res.status, e.code || 'error', e.message || ('The server answered ' + res.status + '.'), e.retryable, Number(res.headers.get('Retry-After')) || 0);
  }
  if (res.status !== 204 && data === null) {
    // A 2xx that cannot be read: the outcome is unknown, treat it as no answer.
    throw new ApiError(0, 'network', 'The answer could not be read.', true);
  }
  return { status: res.status, data };
}

export const get = (path) => call('GET', path).then((r) => r.data);

// write sends one user action. `json` holds its client_id. A lost answer, a
// busy server or a pending answer (202) repeats the IDENTICAL body, so the
// server replays the first result and never writes a second row.
export async function write(path, json) {
  for (let attempt = 0; ; attempt++) {
    try {
      const r = await call('POST', path, { json });
      if (r.status === 202 && attempt < 30) {
        await sleep(2000);
        continue;
      }
      return r;
    } catch (e) {
      const again = e.status === 0 || e.status === 503 || e.status === 504 || (e.status === 502 && e.retryable);
      if (again && attempt < 3) {
        await sleep(Math.min(e.retryAfter || attempt + 1, 10) * 1000);
        continue;
      }
      throw e;
    }
  }
}
