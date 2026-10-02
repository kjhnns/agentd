// The chat panel: the same feed as on the phone. Every message is answered
// by the agent (3 to 25 s; sometimes 202, then the entry is polled).
//
// Double writes: one send at a time. A message has ONE client_id and ONE
// request body for its whole life. A retry sends that same body, so the
// server answers the first result. A new client_id is only made for a new
// message the user typed.

import { el, clear, sleep, clientId, localTime, num, clock } from './util.js';
import { call, get } from './api.js';

const MAX_PHOTOS = 4;
const LONG_EDGE = 2048;

export function createChat(ctx) {
  let items = [];
  let photos = [];      // { blob, url } waiting in the tray
  let pending = null;   // the one message in flight
  let seq = 0;
  let stick = true;

  const feedBox = el('div', { class: 'feed', role: 'log', 'aria-live': 'polite', 'aria-label': 'Chat' });
  const tray = el('div', { class: 'tray' });
  const note = el('p', { class: 'note', role: 'status' });
  const ta = el('textarea', { rows: 1, maxlength: 2000, placeholder: 'What did you eat?', 'aria-label': 'Message', id: 'composer', autocomplete: 'off' });
  const file = el('input', { type: 'file', accept: 'image/*', multiple: true, hidden: true, id: 'photo-input' });
  const attach = el('label', { class: 'btn ghost square', for: 'photo-input', title: 'Add photos (up to 4)', 'aria-label': 'Add photos' }, '+');
  const sendBtn = el('button', { class: 'btn primary', type: 'submit', id: 'send' }, 'Send');
  const form = el('form', { class: 'composer' }, tray, el('div', { class: 'row' }, attach, file, ta, sendBtn));
  const root = el('aside', { class: 'chat', id: 'chat', 'aria-label': 'Chat' },
    el('div', { class: 'chat-head' }, el('h2', null, 'Chat'), el('span', { class: 'hint' }, 'Press / to write')),
    feedBox, note, form);

  feedBox.addEventListener('scroll', () => {
    stick = feedBox.scrollHeight - feedBox.scrollTop - feedBox.clientHeight < 60;
  });

  // ---- photos: picker, drop, paste ----

  async function toJpeg(f) {
    const bmp = await createImageBitmap(f);
    const s = Math.min(1, LONG_EDGE / Math.max(bmp.width, bmp.height));
    const c = document.createElement('canvas');
    c.width = Math.max(1, Math.round(bmp.width * s));
    c.height = Math.max(1, Math.round(bmp.height * s));
    c.getContext('2d').drawImage(bmp, 0, 0, c.width, c.height);
    if (bmp.close) bmp.close();
    return new Promise((ok, bad) => c.toBlob((b) => (b ? ok(b) : bad(new Error('encode'))), 'image/jpeg', 0.85));
  }

  async function addFiles(list) {
    note.textContent = '';
    for (const f of Array.from(list || [])) {
      if (!f.type || !f.type.startsWith('image/')) continue;
      if (photos.length >= MAX_PHOTOS) {
        note.textContent = 'At most ' + MAX_PHOTOS + ' photos in one message.';
        break;
      }
      try {
        const blob = await toJpeg(f);
        photos.push({ blob, url: URL.createObjectURL(blob) });
      } catch (e) {
        note.textContent = 'This browser cannot read that image. Export it as JPEG.';
      }
    }
    renderTray();
  }

  function renderTray() {
    clear(tray);
    photos.forEach((p, i) => {
      tray.append(el('span', { class: 'thumb-wrap' },
        el('img', { class: 'thumb', src: p.url, alt: 'Photo ' + (i + 1) }),
        el('button', { class: 'thumb-x', type: 'button', 'aria-label': 'Remove photo ' + (i + 1), on: { click: () => {
          URL.revokeObjectURL(p.url);
          photos.splice(i, 1);
          renderTray();
        } } }, 'x')));
    });
  }

  file.addEventListener('change', () => { addFiles(file.files); file.value = ''; });
  root.addEventListener('dragover', (e) => { e.preventDefault(); root.classList.add('drop'); });
  root.addEventListener('dragleave', () => root.classList.remove('drop'));
  root.addEventListener('drop', (e) => {
    e.preventDefault();
    root.classList.remove('drop');
    if (e.dataTransfer) addFiles(e.dataTransfer.files);
  });
  ta.addEventListener('paste', (e) => {
    const files = Array.from((e.clipboardData && e.clipboardData.files) || []).filter((f) => f.type.startsWith('image/'));
    if (files.length) {
      e.preventDefault();
      addFiles(files);
    }
  });

  // ---- composer ----

  function grow() {
    ta.style.height = 'auto';
    ta.style.height = Math.min(ta.scrollHeight, 160) + 'px';
  }
  ta.addEventListener('input', grow);
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submit();
    } else if (e.key === 'Escape') {
      ta.blur();
    }
  });
  form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });

  function lock() {
    sendBtn.disabled = !!pending;
    sendBtn.textContent = pending && pending.phase !== 'failed' ? 'Sending' : 'Send';
  }

  function submit() {
    if (pending) return; // one send at a time
    const text = ta.value.trim();
    if (!text && !photos.length) return;
    const id = clientId();
    const lt = localTime();
    let req;
    if (photos.length) {
      const fd = new FormData();
      fd.append('client_id', id);
      if (text) fd.append('text', text);
      fd.append('local_time', lt);
      photos.forEach((p, i) => fd.append('image', p.blob, 'photo' + (i + 1) + '.jpg'));
      req = { form: fd };
    } else {
      req = { json: { client_id: id, text, local_time: lt } };
    }
    pending = { text, req, urls: photos.map((p) => p.url), known: new Set(items.map((it) => it.id)), started: Date.now(), phase: 'sending', message: '' };
    photos = [];
    ta.value = '';
    grow();
    renderTray();
    note.textContent = '';
    stick = true;
    render();
    ta.focus();
    run();
  }

  async function run() {
    const p = pending;
    p.phase = 'sending';
    p.message = '';
    render();
    for (let attempt = 0; ; attempt++) {
      try {
        const r = await call('POST', '/fuel/log', p.req);
        if (r.status === 202 && r.data && r.data.entry_id) {
          p.phase = 'waiting';
          render();
          await pollEntry(r.data.entry_id);
        }
        return done(p);
      } catch (e) {
        if (e.status === 401) { // signed out: the text goes back, the login page shows
          restore(p);
          return;
        }
        if (e.status === 0 && attempt < 2) { // no answer: the identical request once more
          await sleep(2000);
          continue;
        }
        if (e.status === 400 || e.status === 413 || e.status === 415) {
          // Refused before any write: the text goes back to the composer.
          restore(p);
          note.textContent = e.message;
          return;
        }
        p.phase = 'failed';
        p.message = e.status === 0 ? 'Could not confirm whether this was saved.' : e.message;
        render();
        return;
      }
    }
  }

  async function pollEntry(id) {
    for (let i = 0; i < 90; i++) {
      await sleep(2000);
      try {
        const r = await call('GET', '/fuel/entry/' + encodeURIComponent(id));
        if (r.status === 200) return;
      } catch (e) {
        if (e.status === 401 || e.status === 404) return;
      }
    }
  }

  function release(p) {
    p.urls.forEach((u) => URL.revokeObjectURL(u));
    if (pending === p) pending = null;
  }

  async function done(p) {
    release(p);
    await ctx.refresh();
    render();
  }

  function restore(p) {
    release(p);
    if (!ta.value) {
      ta.value = p.text;
      grow();
    }
    render();
  }

  // Discard asks the server first: when the message is in the feed it did
  // arrive, and the text does NOT go back (a second send would log it twice).
  async function discard() {
    const p = pending;
    if (!p) return;
    await refresh();
    if (arrived(p)) {
      release(p);
      note.textContent = 'The message did arrive. The answer appears here.';
    } else {
      restore(p);
      if (p.urls.length) note.textContent = 'Attach the photos again.';
    }
    render();
  }

  // The server writes the user line before it asks the agent. The local
  // bubble gives way to it, so one message is one bubble.
  function arrived(p) {
    return items.find((it) => it.role === 'user' && !p.known.has(it.id) && (it.text || '').trim() === p.text) || null;
  }

  // ---- feed ----

  async function refresh() {
    const mine = ++seq;
    let d;
    try {
      d = await get('/fuel/feed?limit=40');
    } catch (e) {
      return;
    }
    if (mine !== seq) return; // only the newest answer is applied
    items = (d && d.items) || [];
    render();
  }

  function photo(id) {
    const img = el('img', { class: 'thumb', src: '/fuel/photo/' + encodeURIComponent(id), alt: 'Photo', loading: 'lazy' });
    img.addEventListener('error', () => img.replaceWith(el('span', { class: 'thumb gone', title: 'Photo no longer stored' }, '?')));
    return img;
  }

  function itemLine(s) {
    const e = s.effective || s;
    const amount = s.kind === 'drink' && s.volume_ml != null ? num(s.volume_ml, 'ml') + ' ml' : (s.portion_g != null ? num(s.portion_g, 'g') + ' g' : '');
    return el('li', { class: s.undone ? 'undone' : null },
      el('span', { class: 'name' }, s.item || 'item', s.undone ? ' (removed)' : ''),
      el('span', { class: 'amt' }, amount),
      el('span', { class: 'kc' }, s.undone ? '' : num(e.kcal, 'kcal') + ' kcal'),
      el('span', { class: 'pr' }, s.undone ? '' : num(e.protein_g, 'g') + ' g P'));
  }

  function widget(b, full) {
    const d = b.data || {};
    if (b.widget === 'next_action' && d.text) return el('p', { class: 'w-line' }, d.text);
    const rows = Array.isArray(d.macros) ? d.macros : (Array.isArray(d.intake) ? d.intake : null);
    if (!rows) return null;
    const added = d.added || {};
    const chips = [];
    for (const m of rows) {
      const a = added[m.key];
      const changed = typeof a === 'number' && Math.abs(a) >= 0.05;
      if (!changed && !full) continue;
      if (!changed && m.target == null) continue;
      chips.push(el('span', { class: 'chip' },
        el('b', null, m.label || m.key), ' ',
        changed ? (a > 0 ? '+' : '') + num(a, m.unit) + ' ' : '',
        el('span', { class: 'dim' }, (changed ? '= ' : '') + num(m.consumed, m.unit) + (m.target != null ? ' / ' + num(m.target, m.unit) : '') + ' ' + (m.unit || ''))));
    }
    if (!chips.length) return null;
    return el('div', { class: 'w-macros' + (b.date && b.date !== ctx.state.today ? ' other-day' : '') },
      b.date && ctx.state.today && b.date !== ctx.state.today ? el('span', { class: 'chip day' }, b.date) : null, chips);
  }

  function message(it, newest) {
    if (it.role === 'user') {
      return el('div', { class: 'msg user', data: { id: it.id } },
        el('div', { class: 'bubble' },
          (it.photo_ids || []).length ? el('div', { class: 'photos' }, it.photo_ids.map(photo)) : null,
          it.text ? el('p', null, it.text) : null),
        el('span', { class: 'at' }, clock(it.at)));
    }
    const parts = [];
    for (const b of it.blocks || []) {
      if (b.type === 'text' && b.text) parts.push(el('p', null, b.text));
    }
    if (!parts.length && it.text) parts.push(el('p', null, it.text)); // the text is the same line as the first text block
    if ((it.items || []).length) parts.push(el('ul', { class: 'card' }, it.items.map(itemLine)));
    for (const b of it.blocks || []) {
      if (b.type === 'widget') parts.push(widget(b, newest));
    }
    return el('div', { class: 'msg ' + (it.role === 'coach' ? 'coach' : 'fuel'), data: { id: it.id, entry: it.entry_id || '' } },
      el('div', { class: 'reply' }, parts), el('span', { class: 'at' }, clock(it.at)));
  }

  function render() {
    lock();
    clear(feedBox);
    if (!items.length && !pending) feedBox.append(el('p', { class: 'empty' }, 'No messages yet. Write what you ate, or drop a photo here.'));
    let newest = null;
    for (const it of items) if (it.role === 'fuel') newest = it;
    for (const it of items) feedBox.append(message(it, it === newest));
    if (pending) {
      const srv = arrived(pending);
      if (!srv) {
        feedBox.append(el('div', { class: 'msg user local' },
          el('div', { class: 'bubble' },
            pending.urls.length ? el('div', { class: 'photos' }, pending.urls.map((u) => el('img', { class: 'thumb', src: u, alt: 'Photo' }))) : null,
            pending.text ? el('p', null, pending.text) : null)));
      }
      const answered = srv && srv.entry_id && items.some((it) => it.role !== 'user' && it.entry_id === srv.entry_id);
      if (pending.phase === 'failed') {
        feedBox.append(el('div', { class: 'msg fuel failed', id: 'send-failed' },
          el('div', { class: 'reply' }, el('p', null, pending.message),
            el('div', { class: 'acts' },
              el('button', { class: 'btn', type: 'button', on: { click: () => run() } }, 'Try again'),
              el('button', { class: 'btn ghost', type: 'button', on: { click: () => discard() } }, 'Discard')))));
      } else if (!answered) {
        feedBox.append(el('div', { class: 'msg fuel thinking', id: 'thinking' },
          el('div', { class: 'reply' }, el('p', null, el('span', { class: 'dots' }, 'Thinking'), ' ',
            el('span', { class: 'dim' }, pending.phase === 'waiting' ? 'This one takes longer. The answer appears here.' : '')))));
      }
    }
    if (stick) feedBox.scrollTop = feedBox.scrollHeight;
  }

  function reset() {
    photos.forEach((p) => URL.revokeObjectURL(p.url));
    photos = [];
    items = [];
    if (pending) release(pending);
    seq++;
    renderTray();
    render();
  }

  return {
    root,
    refresh,
    reset,
    focus: () => ta.focus(),
    text: () => ta.value,
  };
}
