// The Dashboard (GET /fuel/snapshot, contract 18.7): the budgets of the
// framework, levers, coffee, strength and the maintenance calibration, each
// with its info link to the framework page (18.8).

import { el, clear, num, clock, bar, kindWord, isOver, infoLink, dateLabel } from './util.js';

function card(title, right, info, ...kids) {
  return el('section', { class: 'card' }, el('div', { class: 'card-head' }, el('h3', null, title), info, right ? el('span', { class: 'right' }, right) : null), kids);
}

function budgetRow(b) {
  const none = b.target === null || b.target === undefined;
  return el('div', { class: 'brow' + (isOver(b) ? ' over' : ''), data: { budget: b.key } },
    el('div', { class: 'brow-top' },
      el('span', { class: 'label' }, b.label || b.key), b.provisional ? el('span', { class: 'tag' }, 'Provisional') : null, infoLink(b.info, b.label || b.key),
      el('span', { class: 'right' }, el('b', null, num(b.consumed, b.unit)), none ? ' ' + b.unit : ' / ' + num(b.target, b.unit) + ' ' + b.unit)),
    none ? null : bar(b.consumed, b.target, b.pace_target_now, isOver(b)),
    el('div', { class: 'dim small' }, none ? (b.key === 'carbs_g' ? 'No carbohydrate floor on this day.' : 'No target.') : kindWord(b.kind) + (b.basis ? ', ' + b.basis : '') + (b.unknown_rows > 0 ? ', ' + b.unknown_rows + ' rows without a value' : '')));
}

function coffeeWords(c) {
  const parts = ['filtered', 'unfiltered', 'espresso', 'instant', 'unknown'].filter((k) => c && c[k] > 0).map((k) => c[k] + ' ' + k);
  return parts.length ? parts.join(', ') : 'none';
}

function calibration(e) {
  const c = e && e.calibration;
  if (!c || c.state === 'off') return null;
  const lines = [];
  if (c.state === 'collecting') lines.push('Calibrating maintenance: ' + c.days + ' complete days in a row, ' + (c.days_needed != null ? c.days_needed : '?') + ' needed. Weigh-ins ' + c.weigh_days + '.');
  if (c.state === 'blocked') lines.push('Calibration is blocked. ' + c.days + ' complete days, weigh-ins ' + c.weigh_days + '.');
  if (c.state === 'candidate' && c.candidate) {
    lines.push('Estimate ' + num(c.candidate.kcal, 'kcal') + ' kcal (plus or minus ' + num(c.candidate.uncertainty_kcal, 'kcal') + ', from the weight trend only). Under-logging makes it too low.');
    lines.push('Tell joe_pa "adopt" to use it. Interval ' + c.candidate.interval[0] + ' to ' + c.candidate.interval[1] + '.');
  }
  for (const b of c.blocked_by || []) lines.push(b);
  if (c.run_km_per_week != null) lines.push('Running in the interval: ' + num(c.run_km_per_week, 'km') + ' km a week.');
  if (e.drift) lines.push('The maintenance estimate moved. Review.');
  const food = infoLink(c.food_record_info, 'the food record');
  return card('Maintenance calibration', c.state, infoLink(e.calibration_info, 'maintenance energy'),
    lines.map((l) => el('p', null, l)),
    food ? el('p', { class: 'dim small' }, 'The 2-week food record ', food) : null);
}

export function renderDashboard(ctx, box) {
  clear(box);
  const st = ctx.state;
  const s = st.snapshot;
  box.append(el('div', { class: 'daynav' }, el('h1', null, 'Dashboard'), s ? el('span', { class: 'dim' }, dateLabel(s.date)) : null));
  if (!s) {
    box.append(el('p', { class: 'empty' }, st.error.snapshot || 'Loading the numbers.'));
    return;
  }
  const grid = el('div', { class: 'grid', id: 'dash-grid' });
  const score = s.budget_score ? s.budget_score.hit + ' of ' + s.budget_score.of : '';
  const cls = s.day_class && s.day_class !== 'unknown' ? s.day_class + ' day' : '';
  grid.append(card('Budgets', [score, cls].filter(Boolean).join(' · '), null, (s.budgets || []).map(budgetRow),
    s.budget_next_action && s.budget_next_action.text ? el('p', { class: 'next' }, 'Next: ' + s.budget_next_action.text) : null));

  const e = s.energy;
  if (e) {
    const words = { provisional: 'Provisional', formula: 'Formula', legacy: 'Fixed' };
    grid.append(card('Energy', words[e.state] || e.state, infoLink(e.info, 'energy'),
      el('p', null, 'Target today ' + num(e.target, 'kcal') + ' kcal.' + (e.run_km != null ? ' Run ' + num(e.run_km, 'km') + ' km.' : '')),
      e.state === 'provisional' ? el('p', { class: 'dim small' }, 'Provisional: no maintenance value is adopted yet.') : null,
      e.maintenance_kcal != null ? el('p', { class: 'dim small' }, 'Maintenance ' + num(e.maintenance_kcal, 'kcal') + ' kcal (' + (e.maintenance_source || 'set') + ')' +
        (e.run_adjust_kcal != null ? ', run ' + (e.run_adjust_kcal >= 0 ? '+' : '') + num(e.run_adjust_kcal, 'kcal') : '') + (e.deficit_kcal != null ? ', deficit ' + num(e.deficit_kcal, 'kcal') : '') + '.') : null));
    const cal = calibration(e);
    if (cal) grid.append(cal);
  }

  if ((s.levers || []).length) {
    grid.append(card('Levers', null, null,
      s.levers.map((l) => el('div', { class: 'mini', data: { lever: l.key } },
        el('span', { class: 'label' }, l.label, ' ', infoLink(l.info, l.label)),
        el('span', { class: 'right' }, num(l.consumed, l.unit) + ' ' + l.unit + (l.reference != null ? ' (reference ' + num(l.reference, l.unit) + ')' : '')),
        el('span', { class: 'dim small sub' }, (l.avg7 != null ? '7-day mean ' + num(l.avg7, l.unit) + ' ' + l.unit + ' over ' + l.covered_days_7 + ' days' : 'no 7-day mean yet') + (l.untagged_rows > 0 ? ', ' + l.untagged_rows + ' untagged rows' : '')))),
      el('p', { class: 'dim small' }, 'A lever is an amount next to a reference dose. It is never pass or fail.')));
  }

  if (s.coffee) {
    grid.append(card('Coffee by brew method', null, infoLink(s.coffee.info, 'coffee brewing'),
      el('div', { class: 'mini' }, el('span', { class: 'label' }, 'Today'), el('span', { class: 'right' }, coffeeWords(s.coffee.today))),
      el('div', { class: 'mini' }, el('span', { class: 'label' }, 'Last 7 days'), el('span', { class: 'right' }, coffeeWords(s.coffee.last7))),
      s.coffee.default_method ? el('p', { class: 'dim small' }, 'Default method: ' + s.coffee.default_method + '.') : null));
  }

  const sg = s.strength;
  if (sg) {
    const wk = sg.week;
    grid.append(card('Strength this week', sg.sessions + ' / ' + sg.sessions_target + ' sessions', infoLink(sg.info, 'strength'),
      wk ? el('p', { class: 'dim small' }, wk.sets + ' sets, ' + num(wk.reps) + ' reps' + (sg.session_min_sets ? '. A session is a day with ' + sg.session_min_sets + ' sets or more.' : '')) : null,
      wk ? (wk.by_variable || []).filter((v) => v.sets > 0).map((v) => el('div', { class: 'mini' }, el('span', { class: 'label' }, v.name), el('span', { class: 'right' }, v.sets + ' sets, ' + num(v.reps) + ' reps'))) : null,
      (sg.missing_variables || []).length ? el('p', { class: 'dim small' }, 'Missing variables: ' + sg.missing_variables.join(', ') + '.') : null,
      sg.clinician && sg.clinician.text ? el('p', { class: 'dim small' }, sg.clinician.text) : null,
      s.week ? el('div', { class: 'mini' }, el('span', { class: 'label' }, 'Runs'), el('span', { class: 'right' }, s.week.runs + ', ' + num(s.week.run_km, 'km') + ' km')) : null,
      el('p', null, el('a', { href: '#/week', class: 'link' }, 'Week budget and seven days'))));
  }
  box.append(grid);
  box.append(el('p', { class: 'dim small' },
    (s.missing || []).includes('strava_stale') ? 'Strava data is old; rest-day targets are used. ' : '',
    (s.missing || []).filter((m) => m !== 'strava_stale').map((m) => 'Missing: ' + m.replace(/_/g, ' ') + '. ').join(''),
    'Data as of ' + clock(s.data_as_of || s.as_of) + '.'));
}
