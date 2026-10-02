// Fuel web app, release 1: Login, Day, Chat, Week, Dashboard.
// No framework and no build step. Health data is never stored in the
// browser (no localStorage, no sessionStorage, no cache).

import { el, clear } from './util.js';
import { loadSession, signIn, signOut, onUnauthorized, get } from './api.js';
import { createChat } from './chat.js';
import { renderDay, flushDeletes, dayBusy } from './day.js';
import { renderWeek } from './week.js';
import { renderDashboard } from './dashboard.js';

const app = document.getElementById('app');
const VIEWS = ['day', 'week', 'dashboard', 'chat'];

const state = {
  session: null,
  view: 'day',
  date: null,      // the day shown (null = today)
  today: null,     // the server's today (targets.tz)
  day: null,
  week: null,
  snapshot: null,
  weekTab: 'week',
  error: {},
};
const seq = { day: 0, week: 0, snapshot: 0 };
let chat = null;
let viewBox = null;
let flashBox = null;
let timer = null;
let flashTimer = null;

const ctx = {
  state,
  render: () => renderView(),
  refresh: () => refresh(),
  go: (view, date) => go(view, date),
  flash: (text) => {
    if (!flashBox) return;
    flashBox.textContent = text;
    clearTimeout(flashTimer);
    flashTimer = setTimeout(() => { flashBox.textContent = ''; }, 6000);
  },
};

// ---- login ----

function renderLogin(message) {
  stop();
  document.body.dataset.view = 'login';
  const token = el('input', { type: 'password', name: 'password', id: 'token', autocomplete: 'current-password', required: true, 'aria-label': 'Fuel token', spellcheck: 'false' });
  const user = el('input', { type: 'text', name: 'username', autocomplete: 'username', value: 'fuel', hidden: true });
  const msg = el('p', { class: 'note', role: 'alert', id: 'login-msg' }, message || '');
  const btn = el('button', { class: 'btn primary', type: 'submit' }, 'Sign in');
  const form = el('form', { class: 'login', autocomplete: 'on' }, el('h1', null, 'Fuel'),
    el('label', { for: 'token' }, 'Token'), user, token, btn, msg);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (btn.disabled || !token.value) return;
    btn.disabled = true;
    msg.textContent = '';
    let r;
    try {
      r = await signIn(token.value);
    } catch (err) {
      r = { ok: false, status: 0 };
    }
    token.value = ''; // the token is never kept by the page
    btn.disabled = false;
    if (r.ok) {
      const s = await loadSession();
      if (s) return start(s);
      msg.textContent = 'Signed in, but the session did not start. Are cookies blocked?';
      return;
    }
    if (r.status === 401) msg.textContent = 'Wrong token.';
    else if (r.status === 429) msg.textContent = 'Too many attempts. Wait ' + (r.retryAfter || 60) + ' s.';
    else if (r.code === 'wrong_origin') msg.textContent = 'Open Fuel on its own address to sign in.';
    else if (r.status === 0) msg.textContent = 'No answer from the server.';
    else msg.textContent = 'Sign-in failed (' + r.status + ').';
    token.focus();
  });
  clear(app).append(form);
  token.focus();
}

// ---- shell ----

function start(session) {
  state.session = session;
  chat = createChat(ctx);
  viewBox = el('main', { id: 'view', class: 'view' });
  flashBox = el('p', { class: 'flash', role: 'status', id: 'flash' });
  const nav = el('nav', { class: 'nav', 'aria-label': 'Views' },
    VIEWS.map((v) => el('a', { href: '#/' + v, class: 'tab tab-' + v, data: { nav: v } }, v.charAt(0).toUpperCase() + v.slice(1))));
  const top = el('header', { class: 'top' },
    el('a', { class: 'brand', href: '#/day' }, 'Fuel'),
    session.test_mode ? el('span', { class: 'tag test', id: 'test-banner', title: 'Test instance' }, 'TEST' + (session.food_log_var ? ' · ' + session.food_log_var : '')) : null,
    nav,
    el('button', { class: 'btn ghost', type: 'button', id: 'logout', on: { click: logout } }, 'Sign out'));
  clear(app).append(top, flashBox, el('div', { class: 'layout' }, viewBox, chat.root));
  route();
  timer = setInterval(() => { if (document.visibilityState === 'visible') refresh(); }, 60000);
}

function stop() {
  clearInterval(timer);
  timer = null;
  if (chat) chat.reset();
  chat = null;
  viewBox = null;
  flashBox = null;
  state.session = null;
  state.day = null;
  state.week = null;
  state.snapshot = null;
}

// Sign out: first every delete that waits for Undo gets its final answer,
// then the session is ended. The login page shows only after the server
// confirmed it; else the app stays and says so.
let leaving = false;
async function logout() {
  if (leaving) return;
  leaving = true;
  const btn = document.getElementById('logout');
  if (btn) { btn.disabled = true; btn.textContent = 'Signing out'; }
  let ok = false;
  try {
    await flushDeletes(ctx);
    ok = !state.session || await signOut(false);
  } catch (e) {
    ok = false;
  }
  leaving = false;
  if (ok) {
    renderLogin('');
    return;
  }
  if (btn) { btn.disabled = false; btn.textContent = 'Sign out'; }
  ctx.flash('Sign out failed. The session is still open. Try again.');
}

onUnauthorized(() => {
  if (state.session) renderLogin('The session ended. Sign in again.');
});

// ---- routing ----

function parse() {
  const m = /^#\/(day|week|dashboard|chat)(?:\/(\d{4}-\d{2}-\d{2}))?$/.exec(location.hash);
  return m ? { view: m[1], date: m[2] || null } : { view: 'day', date: null };
}

function go(view, date) {
  const hash = '#/' + view + (view === 'day' && date && date !== state.today ? '/' + date : '');
  if (location.hash === hash) route();
  else location.hash = hash;
}

function route() {
  if (!state.session) return;
  const r = parse();
  if (r.view !== state.view || r.date !== state.date) flushDeletes(ctx); // a waiting delete is sent when the view changes
  state.view = r.view;
  if (r.view === 'day') state.date = r.date;
  document.body.dataset.view = r.view;
  for (const a of document.querySelectorAll('[data-nav]')) a.classList.toggle('on', a.dataset.nav === r.view);
  renderView();
  refresh();
}

function renderView() {
  if (!viewBox) return;
  if (state.view === 'week') renderWeek(ctx, viewBox);
  else if (state.view === 'dashboard') renderDashboard(ctx, viewBox);
  else renderDay(ctx, viewBox); // the Chat tab of a phone keeps the Day view behind it
}

// ---- data ----

// Every visible consumer is fetched again after each own write, when the tab
// becomes visible and every 60 s. Only the newest answer of a kind is applied.
async function load(kind, path, apply) {
  const mine = ++seq[kind];
  try {
    const d = await get(path);
    if (mine !== seq[kind] || !state.session) return;
    state.error[kind] = '';
    apply(d);
  } catch (e) {
    if (mine !== seq[kind]) return;
    state.error[kind] = e.status === 0 ? 'Offline. Nothing is shown until the server answers.' : e.message;
  }
}

async function refresh() {
  if (!state.session) return;
  const jobs = [chat.refresh()];
  if (state.view === 'week') {
    jobs.push(load('week', '/fuel/week', (d) => { state.week = d; }));
  } else if (state.view === 'dashboard') {
    jobs.push(load('snapshot', '/fuel/snapshot', (d) => { state.snapshot = d; state.today = d.date; }));
  } else {
    const want = state.date;
    jobs.push(load('day', '/fuel/day' + (want ? '?date=' + encodeURIComponent(want) : ''), (d) => {
      if (!want) state.today = d.date; // no date asked = the server's today
      state.day = d;
    }));
    if (!state.today) jobs.push(load('snapshot', '/fuel/snapshot', (d) => { state.today = d.date; }));
  }
  await Promise.all(jobs);
  if (!dayBusy()) renderView(); // an open amount field is not redrawn under the cursor
}

// ---- keys ----

function typing(t) {
  return t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable);
}

document.addEventListener('keydown', (e) => {
  if (!state.session || e.ctrlKey || e.metaKey || e.altKey || typing(e.target)) return;
  if (e.key === '/' || e.key === 'n') {
    e.preventDefault();
    if (window.matchMedia('(max-width: 899px)').matches && state.view !== 'chat') go('chat');
    setTimeout(() => chat && chat.focus(), 0);
  }
});

window.addEventListener('hashchange', route);
document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') refresh(); });

(async function boot() {
  const s = await loadSession();
  if (s) start(s);
  else renderLogin('');
})();
