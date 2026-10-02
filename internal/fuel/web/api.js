// The fetch layer. The session is a cookie the page cannot read (HttpOnly).
// Every write carries the CSRF token of the session in X-Fuel-CSRF. The Fuel
// token is sent once at sign-in and is never stored by the page.

import { sleep } from './util.js';

let csrf = '';
let unauthorized = () => {};
let wait = sleep;

// Tests replace the wait between two tries.
export function setWait(fn) { wait = fn; }

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

// signOut ends the session on the server. It answers true only when the
// server confirmed it (204), or when no session is left (401). On any other
// answer, or on no answer, the session may still be open: the caller keeps
// the app on screen and offers the action again.
export async function signOut(all) {
  const path = '/fuel/session' + (all ? '?all=1' : '');
  let r = await raw('DELETE', path);
  if (r.res.status === 403 && r.data && r.data.error && r.data.error.code === 'csrf') {
    if (!(await loadSession())) {
      // No answer or no session: only a 401 proves that it is gone.
      const probe = await raw('GET', '/fuel/session');
      if (probe.res.status !== 401) return false;
      csrf = '';
      return true;
    }
    r = await raw('DELETE', path);
  }
  if (r.res.status !== 204 && r.res.status !== 401) return false;
  csrf = '';
  return true;
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

// definitive says whether an error is the final answer for the operation
// itself. Everything else leaves the outcome open: no answer, a busy or
// rate-limited server, a timeout, a retryable upstream failure.
//   final: 400, 401 and 403 (refused before any write), 404, 409, 413, 415,
//          a 502 that is not retryable, any other 4xx.
//   open:  no answer (0), 429, 500 and up when retryable, 503, 504.
export function definitive(e) {
  if (!(e instanceof ApiError)) return true; // a bug in the page, not an answer
  if (e.status === 0 || e.status === 429 || e.status === 503 || e.status === 504) return false;
  if (e.status >= 500) return !e.retryable;
  return true;
}

function pause(e, attempt) {
  const s = e && e.retryAfter ? Math.min(e.retryAfter, 60) : Math.min(2 ** Math.min(attempt, 5), 30);
  return wait(s * 1000);
}

// write sends one user action. `json` holds its client_id. It returns only
// with a FINAL result: the 200 answer, or a thrown final error. While the
// outcome is open (no answer, busy, 202 pending) it repeats the IDENTICAL
// body, so the server replays the first result and never writes a second
// row. The caller keeps the row locked until then; `onOpen` is told when the
// outcome is still open (to show "still saving").
export async function write(path, json, onOpen) {
  for (let attempt = 0; ; attempt++) {
    try {
      const r = await call('POST', path, { json });
      if (r.status !== 202) return r;
      if (onOpen) onOpen(attempt, null);
      await wait(2000);
    } catch (e) {
      if (definitive(e)) throw e;
      if (onOpen) onOpen(attempt, e);
      await pause(e, attempt);
    }
  }
}

// sendLog sends one chat message (POST /fuel/log). `req` is built once and
// is never changed: { json } or { form }. Like write(), it returns only with
// a final result and repeats the identical request while the outcome is
// open. `kick` (optional) is a function that returns a promise which
// resolves when the user asks for the next try now.
//   { ok: true, status, data }            the message is in (200, or 202 = the entry is polled)
//   { ok: false, error }                  a final refusal: nothing was written
export async function sendLog(req, onOpen, kick) {
  for (let attempt = 0; ; attempt++) {
    try {
      const r = await call('POST', '/fuel/log', req);
      return { ok: true, status: r.status, data: r.data };
    } catch (e) {
      if (definitive(e)) return { ok: false, error: e };
      if (onOpen) onOpen(attempt, e);
      if (attempt < 2 && e.status === 0) await wait(2000);
      else await Promise.race([pause(e, attempt), kick ? kick() : new Promise(() => {})]);
    }
  }
}
