// DOM and format helpers. Text from the server (item names, the agent's
// answer) is untrusted: it is only ever written as a text node.

export function el(tag, props, ...kids) {
  const n = document.createElement(tag);
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') n.className = v;
      else if (k === 'text') n.textContent = v;
      else if (k === 'on') for (const [ev, fn] of Object.entries(v)) n.addEventListener(ev, fn);
      else if (k === 'data') for (const [dk, dv] of Object.entries(v)) n.dataset[dk] = dv;
      else if (v === true) n.setAttribute(k, '');
      else n.setAttribute(k, String(v));
    }
  }
  for (const kid of kids.flat()) {
    if (kid === null || kid === undefined || kid === false) continue;
    n.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return n;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A client id names one user action. A retry of that action sends the same
// id, so the server answers the first result and never writes twice.
export function clientId() {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return 'web-' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

export function num(v, unit) {
  if (v === null || v === undefined || Number.isNaN(v)) return '-';
  const a = Math.abs(v);
  let d = 0;
  if (unit === 'kg' || unit === '%') d = 1;
  else if (unit !== 'kcal' && unit !== 'ml' && unit !== 'mg' && a < 10 && Math.round(v) !== v) d = 1;
  return v.toLocaleString('en-GB', { minimumFractionDigits: 0, maximumFractionDigits: d });
}

export function amountOf(item) {
  if (item.volume_ml !== null && item.volume_ml !== undefined && (item.kind === 'drink' || item.portion_g === null || item.portion_g === undefined)) {
    return { field: 'volume_ml', value: item.volume_ml, unit: 'ml' };
  }
  if (item.portion_g !== null && item.portion_g !== undefined) return { field: 'portion_g', value: item.portion_g, unit: 'g' };
  return null;
}

export function dateAdd(date, days) {
  const d = new Date(date + 'T12:00:00Z');
  d.setUTCDate(d.getUTCDate() + days);
  return d.toISOString().slice(0, 10);
}

export function dateLabel(date, opts) {
  return new Date(date + 'T12:00:00Z').toLocaleDateString('en-GB',
    Object.assign({ weekday: 'short', day: 'numeric', month: 'short', timeZone: 'UTC' }, opts));
}

export function clock(rfc) {
  if (!rfc) return '';
  const d = new Date(rfc);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' });
}

// RFC3339 with the offset of the browser (the server reads the local time of a log from it).
export function localTime() {
  const d = new Date();
  const off = -d.getTimezoneOffset();
  const p = (n) => String(Math.floor(Math.abs(n))).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds()) +
    (off >= 0 ? '+' : '-') + p(off / 60) + ':' + p(off % 60);
}

// An info link of the framework page (contract 18.8). Only an https URL
// becomes a link; it opens in a new tab with no opener and no referrer.
export function infoLink(url, topic) {
  if (typeof url !== 'string' || !url.startsWith('https:')) return null;
  return el('a', { class: 'info', href: url, target: '_blank', rel: 'noopener noreferrer', title: 'About ' + topic, 'aria-label': 'About ' + topic }, 'i');
}

// A bar of consumed against target with an optional pace tick. Widths are
// set through the style object (the page has no inline style attribute).
export function bar(consumed, target, pace, over) {
  const b = el('div', { class: 'bar' + (over ? ' over' : ''), 'aria-hidden': 'true' });
  if (!(target > 0)) return b;
  const scale = Math.max(target, consumed || 0, 1);
  const fill = el('div', { class: 'fill' });
  fill.style.width = (100 * Math.min(1, Math.max(0, consumed || 0) / scale)).toFixed(1) + '%';
  b.append(fill);
  if (scale > target) {
    const t = el('div', { class: 'mark' });
    t.style.left = (100 * target / scale).toFixed(1) + '%';
    b.append(t);
  }
  if (pace > 0 && pace < scale) {
    const t = el('div', { class: 'tick' });
    t.style.left = (100 * pace / scale).toFixed(1) + '%';
    b.append(t);
  }
  return b;
}

export function kindWord(kind) {
  if (kind === 'floor') return 'at least';
  if (kind === 'cap' || kind === 'budget') return 'at most';
  return '';
}

export function isOver(m) {
  return (m.kind === 'cap' || m.kind === 'budget') && m.target !== null && m.target !== undefined && m.consumed > m.target;
}
