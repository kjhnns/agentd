// The Week view (GET /fuel/week, contract section 19): per budget the week
// target, the sum so far and what is left, and one cell per day. The server
// decides every day result; the page only draws it.

import { el, clear, num, dateLabel, clock, bar, infoLink } from './util.js';

const MARK = { met: ['ok', 'met'], missed: ['x', 'missed'], over: ['!', 'over'], open: ['..', 'today, open'], incomplete: ['?', 'no full record'], future: ['', 'not yet'], none: ['-', 'no target'] };

function cell(c, unit, today) {
  const m = MARK[c.result] || MARK.none;
  return el('div', { class: 'cell r-' + (MARK[c.result] ? c.result : 'none') + (c.date === today ? ' today' : ''), title: dateLabel(c.date) + ': ' + m[1] },
    el('span', { class: 'wd' }, dateLabel(c.date, { day: undefined, month: undefined }).slice(0, 2)),
    el('span', { class: 'mk' }, m[0]),
    el('span', { class: 'val' }, c.result === 'future' || c.actual === null || c.actual === undefined ? '' : num(c.actual, unit)));
}

function budgetCard(b, today, isWeek) {
  const facts = [];
  if (isWeek && b.target != null) {
    if (b.remaining != null) facts.push(b.kind === 'floor' ? num(Math.max(0, b.remaining), b.unit) + ' ' + b.unit + ' to go' : (b.remaining >= 0 ? num(b.remaining, b.unit) + ' ' + b.unit + ' left' : num(-b.remaining, b.unit) + ' ' + b.unit + ' over'));
    if (b.pace_now != null) facts.push('even pace now ' + num(b.pace_now, b.unit));
  }
  if (b.days_judged > 0) facts.push(b.days_met + ' of ' + b.days_judged + ' days met');
  if (!isWeek && b.mean != null) facts.push('mean ' + num(b.mean, b.unit) + ' ' + b.unit + ' over ' + b.mean_days + ' days');
  if (b.unknown_rows > 0) facts.push(b.unknown_rows + ' rows without a value');
  const target = isWeek && b.target != null;
  return el('section', { class: 'card', data: { week: b.key } },
    el('div', { class: 'card-head' }, el('h3', null, b.label || b.key), infoLink(b.info, b.label || b.key),
      el('span', { class: 'right' }, num(b.consumed, b.unit) + (target ? ' / ' + num(b.target, b.unit) : '') + ' ' + b.unit, target && b.target_estimated ? el('span', { class: 'tag' }, 'estimated') : null)),
    target ? bar(b.consumed, b.target, b.pace_now, b.kind !== 'floor' && b.consumed > b.target) : null,
    facts.length ? el('p', { class: 'dim small' }, facts.join(' · ')) : null,
    el('div', { class: 'strip' }, (b.days || []).map((c) => cell(c, b.unit, today))));
}

function strengthCard(s, isWeek, today) {
  const p = isWeek ? s.week : s.last7;
  if (!p) return null;
  return el('section', { class: 'card', data: { week: 'strength' } },
    el('div', { class: 'card-head' }, el('h3', null, 'Strength'), infoLink(s.info, 'strength'),
      el('span', { class: 'right' }, p.sessions + (isWeek && s.sessions_target ? ' / ' + s.sessions_target : '') + ' sessions')),
    el('p', { class: 'dim small' }, p.sets + ' sets, ' + num(p.reps) + ' reps' + (s.session_min_sets ? '. A session is a day with ' + s.session_min_sets + ' sets or more.' : '')),
    el('div', { class: 'strip' }, (p.days || []).map((d) => el('div', { class: 'cell' + (d.session ? ' r-met' : '') + (d.date === today ? ' today' : '') },
      el('span', { class: 'wd' }, dateLabel(d.date, { day: undefined, month: undefined }).slice(0, 2)),
      el('span', { class: 'mk' }, d.session ? 'ok' : '-'),
      el('span', { class: 'val' }, d.sets > 0 ? String(d.sets) : '')))));
}

export function renderWeek(ctx, box) {
  clear(box);
  const st = ctx.state;
  const w = st.week;
  const isWeek = st.weekTab !== 'last7';
  const tab = (key, label) => el('button', { class: 'seg' + ((key === 'last7') !== isWeek ? ' on' : ''), type: 'button', 'aria-pressed': String((key === 'last7') !== isWeek), data: { tab: key },
    on: { click: () => { st.weekTab = key; ctx.render(); } } }, label);
  box.append(el('div', { class: 'daynav' }, el('h1', null, 'Week'), el('div', { class: 'segs' }, tab('week', 'This week'), tab('last7', 'Last 7 days'))));
  if (!w) {
    box.append(el('p', { class: 'empty' }, st.error.week || 'Loading the week.'));
    return;
  }
  const p = isWeek ? w.week : w.last7;
  box.append(el('p', { class: 'range', id: 'week-range' }, dateLabel(p.from) + ' to ' + dateLabel(p.to),
    p.incomplete_days > 0 ? el('span', { class: 'dim' }, '  ' + p.incomplete_days + ' days without a full record' + (p.consumed_is_partial ? '; the sums are partial' : '')) : null));
  const grid = el('div', { class: 'grid', id: 'week-grid' }, (p.budgets || []).map((b) => budgetCard(b, w.today, isWeek)));
  if (w.strength) grid.append(strengthCard(w.strength, isWeek, w.today));
  box.append(grid);
  box.append(el('p', { class: 'dim small' }, 'Marks: ok = met, x = missed, ! = over, ? = no full record, .. = today, still open. ',
    (w.missing || []).includes('strava_stale') ? 'Strava data is old; rest-day targets are used. ' : '',
    (w.missing || []).includes('complete_day_rule_unset') ? 'The rule for a full day is not set; no past day has a result. ' : '',
    'Data as of ' + clock(w.data_as_of || w.as_of) + '.'));
}
