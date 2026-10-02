// Logic tests of the page code that guards against double writes and a
// false sign-out (codex review round 1, findings 1 to 4). Run by the Go test
// TestWebJS, or by hand: node --test internal/fuel/webtest/writes.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { write, sendLog, signOut, setWait, definitive, ApiError, loadSession } from '../web/api.js';
import { startDelete, flushDeletes } from '../web/day.js';

const waits = [];
setWait(async (ms) => { waits.push(ms); });

function res(status, body, headers) {
  const h = Object.assign({ 'Content-Type': 'application/json' }, headers || {});
  return { status, ok: status >= 200 && status < 300, headers: { get: (k) => h[k] || null }, json: async () => body };
}
const err = (status, code, retryable, headers) => res(status, { error: { code, message: code, retryable: !!retryable } }, headers);

// script installs a fake fetch that answers from a list and records every call.
function script(answers) {
  const calls = [];
  globalThis.fetch = async (path, opts) => {
    calls.push({ path, method: opts.method, body: opts.body, csrf: opts.headers['X-Fuel-CSRF'] });
    const a = answers.shift();
    if (a === undefined) throw new Error('unexpected request ' + opts.method + ' ' + path);
    if (a === 'down') throw new TypeError('network');
    return typeof a === 'function' ? a() : a;
  };
  return calls;
}

test('write: a pending answer (202) is never a success; the identical body is sent until the final answer', async () => {
  const answers = [];
  for (let i = 0; i < 40; i++) answers.push(res(202, { status: 'pending_reconciliation' }));
  answers.push(res(200, { status: 'done' }));
  const calls = script(answers);
  const body = { client_id: 'web-0123456789abcdef', key: 'k1', scale: 1, local_time: '2026-10-02T12:00:00+02:00' };
  const r = await write('/fuel/relog', body);
  assert.equal(r.status, 200);
  assert.equal(calls.length, 41);
  assert.equal(new Set(calls.map((c) => c.body)).size, 1, 'every try sends the same body (the same client_id)');
});

test('write: no answer, busy, timeout and a retryable 502 keep the action open; it ends only with a final answer', async () => {
  const calls = script(['down', 'down', 'down', 'down', 'down', err(503, 'busy', true, { 'Retry-After': '5' }), err(504, 'timeout', true), err(502, 'upstream_failed', true), err(429, 'rate_limited', true), res(200, { status: 'done' })]);
  let open = 0;
  const r = await write('/fuel/fix', { client_id: 'web-aaaaaaaaaaaaaaaa', row_key: 'it_1', portion_g: 200 }, () => { open++; });
  assert.equal(r.status, 200);
  assert.equal(calls.length, 10);
  assert.equal(open, 9);
  assert.equal(new Set(calls.map((c) => c.body)).size, 1);
});

test('write: a final refusal is thrown at once and is not repeated', async () => {
  for (const [status, code, retryable] of [[400, 'bad_input'], [404, 'not_found'], [409, 'already_undone'], [413, 'too_large'], [502, 'model_invalid', false], [401, 'unauthorized']]) {
    const calls = script([err(status, code, retryable)]);
    await assert.rejects(write('/fuel/undo', { client_id: 'web-bbbbbbbbbbbbbbbb', row_key: 'it_1' }), (e) => e instanceof ApiError && e.status === status);
    assert.equal(calls.length, 1, code);
  }
  assert.equal(definitive(new ApiError(0, 'network', '', true)), false);
  assert.equal(definitive(new ApiError(500, 'internal', '', true)), false);
  assert.equal(definitive(new ApiError(500, 'internal', '', false)), true);
});

test('write: a stale CSRF token is fetched again and the SAME body is sent once more', async () => {
  const calls = script([err(403, 'csrf'), res(200, { csrf: 'new-token', expires_at: 'x' }), res(200, { status: 'done' })]);
  const r = await write('/fuel/undo', { client_id: 'web-cccccccccccccccc', row_key: 'it_1' });
  assert.equal(r.status, 200);
  assert.deepEqual(calls.map((c) => c.method + ' ' + c.path), ['POST /fuel/undo', 'GET /fuel/session', 'POST /fuel/undo']);
  assert.equal(calls[0].body, calls[2].body);
  assert.equal(calls[2].csrf, 'new-token');
});

test('sendLog: an ambiguous send keeps its client_id and body until the server answers; it never ends as "failed, type it again"', async () => {
  const calls = script(['down', 'down', 'down', err(503, 'busy', true), 'down', res(200, { status: 'done', entry_id: 'en_1' })]);
  const req = { json: { client_id: 'web-dddddddddddddddd', text: '150 g skyr', local_time: '2026-10-02T12:00:00+02:00' } };
  const open = [];
  const r = await sendLog(req, (attempt, e) => open.push(e.status));
  assert.deepEqual(r, { ok: true, status: 200, data: { status: 'done', entry_id: 'en_1' } });
  assert.equal(calls.length, 6);
  assert.equal(new Set(calls.map((c) => c.body)).size, 1, 'one body, one client_id');
  assert.deepEqual(open, [0, 0, 0, 503, 0]);
});

test('sendLog: only a final refusal gives the text back', async () => {
  for (const status of [400, 413, 415, 401]) {
    const calls = script([err(status, 'x')]);
    const r = await sendLog({ json: { client_id: 'web-eeeeeeeeeeeeeeee', text: 't' } });
    assert.equal(r.ok, false);
    assert.equal(r.error.status, status);
    assert.equal(calls.length, 1);
  }
  // "Try again now" wakes the wait; it does not make a second request body.
  let kick;
  const calls = script([err(503, 'busy', true), res(202, { status: 'pending', entry_id: 'en_2' })]);
  setWait(() => new Promise(() => {})); // the timer never fires: only the kick continues
  const p = sendLog({ json: { client_id: 'web-ffffffffffffffff', text: 't' } }, () => setTimeout(() => kick(), 0), () => new Promise((ok) => { kick = ok; }));
  const r = await p;
  setWait(async (ms) => { waits.push(ms); });
  assert.equal(r.status, 202);
  assert.equal(calls[0].body, calls[1].body);
});

test('signOut: true only when the server confirmed it', async () => {
  script([res(204, null)]);
  assert.equal(await signOut(false), true);
  script([err(401, 'unauthorized')]);
  assert.equal(await signOut(false), true, 'no session left is signed out');
  script([err(500, 'internal', true)]);
  assert.equal(await signOut(false), false, 'a server error is not a sign-out');
  script([err(403, 'forbidden')]);
  assert.equal(await signOut(false), false, 'a refusal is not a sign-out');
  script(['down']);
  await assert.rejects(signOut(false), (e) => e.status === 0, 'no answer is not a sign-out');
  // A stale CSRF token: fetched again, then the DELETE once more.
  let calls = script([err(403, 'csrf'), res(200, { csrf: 'fresh', expires_at: 'x' }), res(204, null)]);
  assert.equal(await signOut(false), true);
  assert.deepEqual(calls.map((c) => c.method), ['DELETE', 'GET', 'DELETE']);
  assert.equal(calls[2].csrf, 'fresh');
  // A stale CSRF token and no answer to the probe: not signed out.
  script([err(403, 'csrf'), 'down', 'down']);
  await assert.rejects(signOut(false));
  script([err(403, 'csrf'), err(500, 'internal'), err(500, 'internal')]);
  assert.equal(await signOut(false), false);
  calls = script([res(204, null)]);
  await signOut(true);
  assert.equal(calls[0].path, '/fuel/session?all=1');
});

test('flushDeletes: a delete in its Undo window is sent once and is finished before the caller goes on (sign-out waits for it)', async () => {
  script([res(200, { csrf: 'c', expires_at: 'x' })]);
  await loadSession();
  const order = [];
  let answer;
  const calls = script([() => new Promise((ok) => { answer = () => { order.push('undo answered'); ok(res(200, { status: 'done' })); }; })]);
  const ctx = { render() {}, flash() {}, refresh: async () => { order.push('refreshed'); } };
  startDelete(ctx, { row_key: 'it_del_1', item: 'apple' });
  const a = flushDeletes(ctx);
  const b = flushDeletes(ctx); // a second flush (view change, then sign-out) is the same request
  let finished = false;
  a.then(() => { finished = true; });
  await new Promise((ok) => setTimeout(ok, 20));
  assert.equal(finished, false, 'the flush is not done while the undo has no answer');
  assert.equal(calls.length, 1);
  answer();
  await Promise.all([a, b]);
  order.push('sign-out may start');
  assert.deepEqual(order, ['undo answered', 'refreshed', 'sign-out may start']);
  assert.equal(calls.length, 1, 'one POST /fuel/undo');
  assert.equal(calls[0].path, '/fuel/undo');
  assert.equal(calls[0].csrf, 'c');
  // Nothing waits any more: a later flush sends nothing.
  script([]);
  await flushDeletes(ctx);
});
