// The Day view: the budgets of the day and its items as a table. Amounts
// are edited in the row (POST /fuel/fix), a delete waits 4 s for Undo before
// it is sent (POST /fuel/undo), Repeat logs the item again now (POST /fuel/relog).
//
// Double writes: every action makes its client_id once, when the user acts.
// While it runs the row is locked. A retry sends the same body.

import { el, clear, clientId, localTime, num, amountOf, dateAdd, dateLabel, clock, bar, basisLine, isOver, infoLink } from './util.js';
import { write } from './api.js';

const UNDO_MS = 4000;
const deleting = new Map(); // row_key -> { clientId, name, timer, sent }
const busy = new Set();     // row keys with a write in flight
let editing = null;         // the row key whose amount field is open

export function dayBusy() { return editing !== null; }

// A delete that still waits for Undo is sent at once when the user leaves
// the view. Closing or reloading the page sends nothing (the row is back).
export function flushDeletes(ctx) {
  for (const key of Array.from(deleting.keys())) commitDelete(ctx, key);
}

async function commitDelete(ctx, key) {
  const p = deleting.get(key);
  if (!p || p.sent) return;
  p.sent = true;
  clearTimeout(p.timer);
  ctx.render();
  try {
    await write('/fuel/undo', { client_id: p.clientId, row_key: key });
  } catch (e) {
    if (e.code !== 'already_undone' && e.status !== 401) ctx.flash('Could not remove ' + p.name + ': ' + e.message);
  }
  deleting.delete(key);
  await ctx.refresh();
}

function startDelete(ctx, item) {
  if (deleting.has(item.row_key) || busy.has(item.row_key)) return;
  const p = { clientId: clientId(), name: item.item, sent: false };
  p.timer = setTimeout(() => commitDelete(ctx, item.row_key), UNDO_MS);
  deleting.set(item.row_key, p);
  ctx.render();
}

function undoDelete(ctx, key) {
  const p = deleting.get(key);
  if (!p || p.sent) return;
  clearTimeout(p.timer);
  deleting.delete(key);
  ctx.render();
}

async function act(ctx, item, path, body, okText) {
  if (busy.has(item.row_key)) return;
  busy.add(item.row_key);
  ctx.render();
  try {
    await write(path, body);
    if (okText) ctx.flash(okText);
  } catch (e) {
    if (e.status !== 401) ctx.flash(e.message);
  }
  busy.delete(item.row_key);
  await ctx.refresh();
}

function amountCell(ctx, item) {
  const a = amountOf(item);
  const canFix = (item.actions || []).includes('fix');
  if (!a) return el('td', { class: 'amt' }, '-');
  if (!canFix || busy.has(item.row_key) || deleting.has(item.row_key)) return el('td', { class: 'amt' }, num(a.value, a.unit) + ' ' + a.unit);
  if (editing === item.row_key) {
    const input = el('input', { class: 'amt-input', type: 'number', inputmode: 'decimal', min: '1', max: '5000', step: 'any', value: String(a.value), 'aria-label': 'Amount of ' + item.item + ' in ' + a.unit });
    let closed = false;
    const close = (save) => {
      if (closed) return;
      closed = true;
      editing = null;
      const v = Number(input.value);
      if (save && v > 0 && v !== a.value) {
        // One fix per Enter: the field is closed before the request starts.
        act(ctx, item, '/fuel/fix', { client_id: clientId(), row_key: item.row_key, [a.field]: v });
      } else {
        ctx.render();
      }
    };
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); close(true); }
      else if (e.key === 'Escape') { e.preventDefault(); close(false); }
    });
    input.addEventListener('blur', () => close(false));
    setTimeout(() => { input.focus(); input.select(); }, 0);
    return el('td', { class: 'amt' }, input, ' ' + a.unit);
  }
  return el('td', { class: 'amt' },
    el('button', { class: 'amt-btn', type: 'button', title: 'Change the amount', data: { edit: item.row_key }, on: { click: () => { editing = item.row_key; ctx.render(); } } },
      num(a.value, a.unit) + ' ' + a.unit));
}

function row(ctx, item) {
  const m = item.macros || {};
  const gone = deleting.get(item.row_key);
  const acts = item.actions || [];
  let actions;
  if (gone && !gone.sent) {
    actions = [el('span', { class: 'dim' }, 'Removed. '), el('button', { class: 'btn', type: 'button', data: { undo: item.row_key }, on: { click: () => undoDelete(ctx, item.row_key) } }, 'Undo')];
  } else if (gone || busy.has(item.row_key)) {
    actions = [el('span', { class: 'dim' }, 'Saving')];
  } else {
    actions = [
      acts.includes('repeat') && item.recent_key ? el('button', { class: 'btn ghost', type: 'button', title: 'Log this again now', data: { repeat: item.row_key },
        on: { click: () => act(ctx, item, '/fuel/relog', { client_id: clientId(), key: item.recent_key, scale: 1, local_time: localTime() }, 'Logged ' + item.item + ' again.') } }, 'Repeat') : null,
      acts.includes('delete') ? el('button', { class: 'btn ghost', type: 'button', title: 'Remove this item', data: { del: item.row_key }, on: { click: () => startDelete(ctx, item) } }, 'Delete') : null,
    ];
  }
  const g = (v) => num(v, 'g');
  return el('tr', { class: gone ? 'gone' : null, data: { row: item.row_key } },
    el('td', { class: 'time' }, clock(item.eaten_at)),
    el('td', { class: 'item' }, item.item,
      item.check ? el('span', { class: 'tag', title: String(item.check) }, 'check') : null,
      item.source && item.source !== 'fuel' ? el('span', { class: 'tag' }, item.source) : null),
    amountCell(ctx, item),
    el('td', { class: 'n kcal' }, num(m.kcal, 'kcal')),
    el('td', { class: 'n prot' }, g(m.protein_g)),
    el('td', { class: 'n wide' }, g(m.carbs_g)),
    el('td', { class: 'n wide' }, g(m.fat_g)),
    el('td', { class: 'n wide' }, g(m.sat_fat_g)),
    el('td', { class: 'n wide' }, g(m.fiber_g)),
    el('td', { class: 'acts' }, actions));
}

export function budgetTiles(budgets) {
  return el('div', { class: 'tiles' }, (budgets || []).map((b) => {
    const none = b.target === null || b.target === undefined;
    return el('div', { class: 'tile' + (isOver(b) ? ' over' : ''), data: { budget: b.key } },
      el('div', { class: 'tile-head' }, el('span', { class: 'label' }, b.label || b.key), b.provisional ? el('span', { class: 'tag' }, 'Provisional') : null, infoLink(b.info, b.label || b.key)),
      el('div', { class: 'tile-num' }, el('b', null, num(b.consumed, b.unit)), none ? ' ' + b.unit : ' / ' + num(b.target, b.unit) + ' ' + b.unit),
      none ? el('div', { class: 'dim small' }, b.key === 'carbs_g' ? 'No floor on this day' : 'No target') : bar(b.consumed, b.target, b.pace_target_now, isOver(b)),
      none ? null : el('div', { class: 'dim small' }, basisLine(b)));
  }));
}

export function renderDay(ctx, box) {
  clear(box);
  const st = ctx.state;
  const d = st.day;
  const today = st.today;
  const date = st.date || today;
  const min = today ? dateAdd(today, -34) : null;

  const pick = el('input', { type: 'date', class: 'date', value: date || '', min, max: today, 'aria-label': 'Day', id: 'day-pick' });
  pick.addEventListener('change', () => { if (pick.value) ctx.go('day', pick.value); });
  box.append(el('div', { class: 'daynav' },
    el('button', { class: 'btn square', type: 'button', id: 'day-prev', 'aria-label': 'Day before', disabled: !date || date <= min, on: { click: () => ctx.go('day', dateAdd(date, -1)) } }, '<'),
    el('h1', { id: 'day-title' }, date ? dateLabel(date) + (date === today ? ' (today)' : '') : 'Day'),
    el('button', { class: 'btn square', type: 'button', id: 'day-next', 'aria-label': 'Day after', disabled: !date || date >= today, on: { click: () => ctx.go('day', dateAdd(date, 1)) } }, '>'),
    pick,
    date !== today ? el('button', { class: 'btn ghost', type: 'button', on: { click: () => ctx.go('day', today) } }, 'Today') : null,
    d && d.snapshot && d.snapshot.day_class && d.snapshot.day_class !== 'unknown' ? el('span', { class: 'tag' }, d.snapshot.day_class + ' day') : null));

  if (!d || d.date !== date) {
    box.append(el('p', { class: 'empty' }, st.error.day || 'Loading the day.'));
    return;
  }
  const snap = d.snapshot || {};
  box.append(budgetTiles(snap.budgets || snap.macros));

  const items = d.items || [];
  if (!items.length) {
    box.append(el('p', { class: 'empty', id: 'day-empty' }, 'Nothing logged on this day.'));
    return;
  }
  const head = el('tr', null,
    el('th', { class: 'time' }, 'Time'), el('th', null, 'Item'), el('th', { class: 'amt' }, 'Amount'),
    el('th', { class: 'n' }, 'kcal'), el('th', { class: 'n' }, 'Protein'), el('th', { class: 'n wide' }, 'Carbs'),
    el('th', { class: 'n wide' }, 'Fat'), el('th', { class: 'n wide' }, 'Sat fat'), el('th', { class: 'n wide' }, 'Fibre'), el('th', { class: 'acts' }, ''));
  box.append(el('div', { class: 'table-wrap' },
    el('table', { class: 'day', id: 'day-table' }, el('thead', null, head), el('tbody', null, items.map((it) => row(ctx, it))))));
  box.append(el('p', { class: 'dim small' }, items.length + (items.length === 1 ? ' item' : ' items') + '. Click an amount to change it. Data as of ' + clock(snap.data_as_of || snap.as_of) + '.'));
}
