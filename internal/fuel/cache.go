package fuel

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// dayCache is one date's Food log rows plus whether a strength habit value
// (Push ups / Pull ups) exists that day.
type dayCache struct {
	rows     []Value
	strength bool
	// nums are the non-json values of the date, of every variable (spec
	// 18.6): the strength sets are computed from them at each snapshot, so a
	// changed set_variables list needs no new read.
	nums    []numVal
	fp      string // fingerprint of the food rows of the last server read
	fetched time.Time
	ok      bool // at least one successful read
}

// Cache holds the last 35 days of Food log rows and the Body composition
// history (spec section 5 [C8], section 14 [C8]). Reads never walk all
// history per request.
type Cache struct {
	mu       sync.Mutex
	vars     Variables
	ids      VarIDs
	days     map[string]*dayCache
	gen      map[string]uint64 // completed refreshes per date
	body     []Value
	bodyAt   time.Time
	rec      map[string][]Value // record variables: "bp", "symptom" (spec 18.4)
	recOwn   []ownRecord        // own record rows merged on 201
	journal  *Journal
	now      func() time.Time
	readTO   time.Duration
	bodyTO   time.Duration // the one-shot history read (startup, 20 min loop)
	refreshM sync.Map      // date -> *sync.Mutex (one refresh per day at a time)
	publish  sync.Locker   // the service render lock (stateMu), taken to publish
	// onChange is called (outside the cache lock) when a refresh brings
	// other food rows for a date that was loaded before, or other Body
	// composition rows ("body").
	onChange func(date string)
	bodyFP   string
}

func newCache(v Variables, ids VarIDs, j *Journal, now func() time.Time) *Cache {
	return &Cache{vars: v, ids: ids, days: map[string]*dayCache{}, gen: map[string]uint64{}, journal: j, now: now, readTO: 5 * time.Second, bodyTO: 20 * time.Second}
}

// ownRecord is an own record row merged on 201, kept so a refresh that does
// not hold it yet re-adds it (written less than 60 s ago).
type ownRecord struct {
	vr string
	v  Value
}

// daySlot is a one-slot semaphore per date: waiting for it honours the
// caller's context, so a queue of refreshes cannot outlive a request budget.
func (c *Cache) daySlot(date string) chan struct{} {
	m, _ := c.refreshM.LoadOrStore(date, make(chan struct{}, 1))
	return m.(chan struct{})
}

// RefreshDay REPLACES the cached day from a server read, then re-adds own done
// rows missing from that read only if they were written less than 60 s ago
// (eventual consistency). External edits and deletions win.
func (c *Cache) RefreshDay(ctx context.Context, date string) error {
	c.mu.Lock()
	asked := c.gen[date]
	c.mu.Unlock()
	slot := c.daySlot(date)
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slot }()
	// Coalesce: a refresh that STARTED after we asked and completed while we
	// waited is as fresh as ours (generation counter, not the clock).
	c.mu.Lock()
	coalesced := c.gen[date] >= asked+2
	c.mu.Unlock()
	if coalesced {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, c.readTO)
	defer cancel()
	vals, err := c.vars.ByDate(rctx, date)
	if err != nil {
		return err
	}
	dc := &dayCache{fetched: c.now(), ok: true}
	seen := map[string]bool{}
	for _, v := range vals {
		switch v.VariableID {
		case c.ids.Food:
			if v.RecordDate == "" || v.RecordDate == date {
				dc.rows = append(dc.rows, v)
				seen[v.ID] = true
			}
		case c.ids.PushUps, c.ids.PullUps:
			if v.VariableID != "" && v.Raw != "" && v.Raw != "0" {
				dc.strength = true
			}
		}
		if v.Data == nil && v.VariableID != "" && (v.RecordDate == "" || v.RecordDate == date) {
			dc.nums = append(dc.nums, numVal{VarID: v.VariableID, ID: v.ID, Raw: v.Raw})
		}
	}
	// Collect own recent rows and publish UNDER the cache lock: a done op is
	// journaled inside the same lock (commit), so a row written after the
	// server read is either in the journal now or merged after we release.
	// The publish also takes the service's render lock (exclusively, same
	// order as markDone), so a response never mixes two views of a day.
	if c.publish != nil {
		c.publish.Lock()
		defer c.publish.Unlock()
	}
	dc.fp = rowsFingerprint(dc.rows)
	changed := false
	c.mu.Lock()
	if old := c.days[date]; old != nil && old.ok && old.fp != dc.fp {
		changed = true
	}
	if c.journal != nil {
		for _, op := range c.journal.DoneRowsFor(date, c.now().Add(-60*time.Second)) {
			if !seen[op.ValueID] {
				dc.rows = append(dc.rows, opValue(op, c.ids.Food))
			}
		}
	}
	c.days[date] = dc
	c.gen[date]++
	c.mu.Unlock()
	if changed && c.onChange != nil {
		c.onChange(date)
	}
	return nil
}

// rowsFingerprint identifies the content of a server read (ids and data).
func rowsFingerprint(rows []Value) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, r.ID+"\x00"+r.Raw)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

// commit runs fn under the cache lock (journal a done op + merge its row).
func (c *Cache) commit(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

// View runs fn under the cache lock, for a consistent rows + revision pair.
func (c *Cache) View(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

func opValue(op Op, varID string) Value {
	return Value{ID: op.ValueID, VariableID: varID, RecordDate: op.Date, CreatedAt: op.DoneAt, Data: op.Data}
}

// mergeLocked adds an own row after its 201, by value id, so a refresh never
// double counts or drops it. The caller holds c.mu.
func (c *Cache) mergeLocked(op Op) {
	dc := c.days[op.Date]
	if dc == nil {
		dc = &dayCache{}
		c.days[op.Date] = dc
	}
	for _, r := range dc.rows {
		if r.ID == op.ValueID {
			return
		}
	}
	dc.rows = append(dc.rows, opValue(op, c.ids.Food))
}

// Rows returns a copy of a day's rows and the time of its last server read.
func (c *Cache) Rows(date string) ([]Value, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rowsLocked(date)
}

// rowsLocked is the day as it COUNTS (spec 15.6). The destination row of a
// move (`moved_from`) counts only once the SOURCE item is undone in the rows
// of its own day; until then the source counts and the destination group
// (its original, duplicates and every correction of it, by item id or value
// id) is left out. The rule reads only rows, so both sides switch in the
// same instant: when the undo row becomes known.
func (c *Cache) rowsLocked(date string) ([]Value, time.Time, bool) {
	rows, fetched, ok := c.rawRowsLocked(date)
	moved := false
	for _, r := range rows {
		if _, has := r.Data["moved_from"]; has {
			moved = true
			break
		}
	}
	if !moved {
		return rows, fetched, ok
	}
	srcGroups := map[string][]*group{} // source date -> groups
	// Rows of the groups that do not count, by value id and by op_id (so
	// their duplicate copies go too). Every other row stays as it is,
	// duplicates included: later grouping still resolves a correction that
	// names a duplicate copy.
	dropID, dropOp := map[string]bool{}, map[string]bool{}
	for _, g := range groupRows(rows) {
		if c.destinationCounts(g, srcGroups) {
			continue
		}
		for _, r := range g.rows {
			dropID[r.ID] = true
			if op, _ := r.Data["op_id"].(string); op != "" {
				dropOp[op] = true
			}
		}
	}
	if len(dropID) == 0 {
		return rows, fetched, ok
	}
	out := rows[:0]
	for _, r := range rows {
		op, _ := r.Data["op_id"].(string)
		if dropID[r.ID] || (op != "" && dropOp[op]) {
			continue
		}
		out = append(out, r)
	}
	return out, fetched, ok
}

// destinationCounts: a group that is not a move destination always counts;
// a destination counts when its source is undone, deleted, or on a day that
// is not cached (nothing to compare with).
func (c *Cache) destinationCounts(g *group, srcGroups map[string][]*group) bool {
	if g.orig.Data == nil {
		return true
	}
	from, _ := g.orig.Data["moved_from"].(string)
	srcDate, _ := g.orig.Data["moved_from_date"].(string)
	if from == "" || srcDate == "" {
		return true
	}
	gs, seen := srcGroups[srcDate]
	if !seen {
		raw, _, loaded := c.rawRowsLocked(srcDate)
		if !loaded {
			srcGroups[srcDate] = nil
			return true
		}
		gs = groupRows(raw)
		if gs == nil {
			gs = []*group{}
		}
		srcGroups[srcDate] = gs
	}
	if gs == nil {
		return true
	}
	for _, sg := range gs {
		if sg.key == "i:"+from || sg.key == "v:"+from {
			return sg.c.undone
		}
	}
	return true // the source row is gone
}

// RowsRaw returns every cached row of a day (reconciliation and
// compensation look for rows by op_id, hidden or not).
func (c *Cache) RowsRaw(date string) ([]Value, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rawRowsLocked(date)
}

func (c *Cache) rawRowsLocked(date string) ([]Value, time.Time, bool) {
	dc := c.days[date]
	if dc == nil {
		return nil, time.Time{}, false
	}
	return append([]Value(nil), dc.rows...), dc.fetched, dc.ok
}

// Strength reports whether the day has a Push ups or Pull ups value.
func (c *Cache) Strength(date string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.strengthLocked(date)
}

func (c *Cache) strengthLocked(date string) bool {
	dc := c.days[date]
	return dc != nil && dc.strength
}

// Loaded reports whether a day was read from the server at least once.
func (c *Cache) Loaded(date string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	dc := c.days[date]
	return dc != nil && dc.ok
}

// Age of a day's last read (a large value when never read).
func (c *Cache) Age(date string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	dc := c.days[date]
	if dc == nil || !dc.ok {
		return 1 << 62
	}
	return c.now().Sub(dc.fetched)
}

// RefreshBody replaces the Body composition history from one includeJson read.
func (c *Cache) RefreshBody(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, c.bodyTO)
	defer cancel()
	vals, err := c.vars.All(rctx)
	if err != nil {
		return err
	}
	var body []Value
	rec := map[string][]Value{}
	seen := map[string]bool{}
	from := c.now().AddDate(0, 0, -181).Format("2006-01-02") // the record window: 180 days
	for _, v := range vals {
		if v.Data == nil {
			continue
		}
		switch {
		case v.VariableID == c.ids.Body:
			body = append(body, v)
			seen[v.ID] = true
		case c.ids.BP != "" && v.VariableID == c.ids.BP && v.RecordDate >= from:
			rec["bp"] = append(rec["bp"], v)
			seen[v.ID] = true
		case c.ids.Symptom != "" && v.VariableID == c.ids.Symptom && v.RecordDate >= from:
			rec["symptom"] = append(rec["symptom"], v)
			seen[v.ID] = true
		}
	}
	c.mu.Lock()
	// The read REPLACES the cached rows; own rows written less than 60 s ago
	// that it does not hold yet are re-added (section 14 [C8]).
	var keep []ownRecord
	for _, o := range c.recOwn {
		if c.now().Sub(o.v.CreatedAt) > 60*time.Second {
			continue
		}
		keep = append(keep, o)
		if seen[o.v.ID] {
			continue
		}
		if o.vr == "body" {
			body = append(body, o.v)
		} else {
			rec[o.vr] = append(rec[o.vr], o.v)
		}
	}
	sort.SliceStable(body, func(a, b int) bool { return body[a].RecordDate < body[b].RecordDate })
	c.recOwn = keep
	fp := rowsFingerprint(body)
	changed := !c.bodyAt.IsZero() && fp != c.bodyFP
	c.body, c.bodyAt, c.rec, c.bodyFP = body, c.now(), rec, fp
	c.mu.Unlock()
	if changed && c.onChange != nil {
		c.onChange("body")
	}
	return nil
}

// RecordsReadAt is the time of the last full read of the record variables.
func (c *Cache) RecordsReadAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodyAt
}

// mergeRecord adds an own record row after its 201, by value id.
func (c *Cache) mergeRecord(vr string, v Value) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := c.body
	if vr != "body" {
		rows = c.rec[vr]
	}
	for _, r := range rows {
		if r.ID == v.ID {
			return
		}
	}
	c.recOwn = append(c.recOwn, ownRecord{vr, v})
	if vr == "body" {
		c.body = append(append([]Value(nil), c.body...), v)
		return
	}
	if c.rec == nil {
		c.rec = map[string][]Value{}
	}
	c.rec[vr] = append(c.rec[vr], v)
}

// RecordRows returns the cached rows of the record variables by var
// ("bp", "symptom", and "body" for the waist rows).
func (c *Cache) RecordRows() map[string][]Value {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]Value{"body": append([]Value(nil), c.body...)}
	for k, v := range c.rec {
		out[k] = append([]Value(nil), v...)
	}
	return out
}

// numsLocked returns the non-json values of a date (the caller holds c.mu).
func (c *Cache) numsLocked(date string) []numVal {
	if dc := c.days[date]; dc != nil {
		return dc.nums
	}
	return nil
}

// BodyLoaded reports whether the Body composition history was read once.
func (c *Cache) BodyLoaded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.bodyAt.IsZero()
}

func (c *Cache) Body() ([]Value, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body, c.bodyAt
}

// ---- totals ----

// dedupeRows keeps the first value per op_id (spec 14 [C1]): Variables has no
// server-side dedup, so a duplicate row is made harmless, not impossible.
func dedupeRows(rows []Value) []Value {
	out, _ := dedupeRowsMap(rows)
	return out
}

// dedupeRowsMap is dedupeRows plus a map from every dropped duplicate's
// value id to the kept row's value id (a correction may name a copy).
func dedupeRowsMap(rows []Value) ([]Value, map[string]string) {
	dup := map[string]string{}
	sorted := append([]Value(nil), rows...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if !sorted[a].CreatedAt.Equal(sorted[b].CreatedAt) {
			return sorted[a].CreatedAt.Before(sorted[b].CreatedAt)
		}
		return sorted[a].ID < sorted[b].ID
	})
	seen := map[string]string{} // op_id -> kept value id
	var out []Value
	for _, r := range sorted {
		if r.Data == nil {
			continue
		}
		if op, _ := r.Data["op_id"].(string); op != "" {
			if kept, ok := seen[op]; ok {
				dup[r.ID] = kept
				continue
			}
			seen[op] = r.ID
		}
		out = append(out, r)
	}
	return out, dup
}

// DayTotals is the consumed sum per macro and the unknown-contribution count.
type DayTotals struct {
	Sum     map[string]int64 // tenths
	Unknown map[string]int
	Rows    int
}

// totalsFromRows groups rows into contributions (an original row plus all its
// corrections; an external row alone), then sums known fields. A contribution
// with a null field counts in unknown_rows; an undone item is a known 0.
// contrib is one contribution (an original row plus all its corrections, or
// an external row alone) reduced per field: the sum of the KNOWN values, and
// whether any row had the field null / every row had it null. The one
// reducer behind totals, effective state, undo and compensation.
type contrib struct {
	sum     map[string]int64
	anyNull map[string]bool
	known   map[string]bool // at least one row had the field known
	undone  bool            // a row with reason undo or compensation exists
	rows    int
}

func newContrib() *contrib {
	return &contrib{sum: map[string]int64{}, anyNull: map[string]bool{}, known: map[string]bool{}}
}

func (c *contrib) add(r Value) {
	m := macrosFromData(r.Data)
	if _, isCorr := r.Data["corrects"]; !isCorr {
		m.normalizeNetCarbs()
	}
	if reason, _ := r.Data["reason"].(string); reason == "undo" || reason == "compensation" {
		c.undone = true
	}
	c.rows++
	for _, k := range AllKeys {
		if v := m.Get(k); v.OK {
			c.sum[k] += v.V
			c.known[k] = true
		} else {
			c.anyNull[k] = true
		}
	}
}

// value is the field's contribution: an undone item is KNOWN (its known sum;
// null counts as 0, spec 14 [C11]); otherwise any null makes it unknown.
func (c *contrib) value(k string) tenth {
	if c.anyNull[k] && !c.undone {
		return tenth{}
	}
	return known(c.sum[k])
}

// effective renders all fields.
func (c *contrib) effective() Macros {
	var m Macros
	for i, f := range m.fields() {
		*f = c.value(AllKeys[i])
	}
	return m
}

// cancel is the correction that brings the known sum of every field to 0; a
// field no row ever knew stays null (null stays null).
func (c *contrib) cancel() Macros {
	var m Macros
	for i, f := range m.fields() {
		k := AllKeys[i]
		if c.known[k] {
			*f = known(-c.sum[k])
		}
	}
	return m
}

// residual reports whether any known sum is non-zero.
func (c *contrib) residual() bool {
	for _, k := range AllKeys {
		if c.sum[k] != 0 {
			return true
		}
	}
	return false
}

// group is one contribution with its original row (zero Value when only
// corrections are present).
type group struct {
	key  string
	orig Value
	c    *contrib
	last Value   // the newest row of the group
	rows []Value // every row, oldest first
}

// groupRows dedupes rows and groups them into contributions: an original
// row plus every correction of it. A correction names its original by
// item_id (rows written by Fuel) or by value id (rows written by the agentd
// food-log script, `corrects: <value_id>`); both join the same group.
func groupRows(rows []Value) []*group {
	rows, dup := dedupeRowsMap(rows)
	byKey := map[string]*group{}
	alias := map[string]string{} // item_id or value id -> group key
	var order []*group
	get := func(key string) *group {
		g := byKey[key]
		if g == nil {
			g = &group{key: key, c: newContrib()}
			byKey[key] = g
			order = append(order, g)
		}
		return g
	}
	// Originals first, so a correction can find its original by either id.
	for _, r := range rows {
		if c, _ := r.Data["corrects"].(string); c != "" {
			continue
		}
		key := "v:" + r.ID
		if id, _ := r.Data["item_id"].(string); id != "" {
			key = "i:" + id
			alias[id] = key
		}
		alias[r.ID] = key
		g := get(key)
		g.orig = r
	}
	for _, r := range rows {
		key := ""
		if c, _ := r.Data["corrects"].(string); c != "" {
			if kept, ok := dup[c]; ok {
				c = kept // it names a duplicate copy of its original
			}
			if k, ok := alias[c]; ok {
				key = k
			} else {
				key = "i:" + c // its original is not in these rows
			}
		} else {
			key = alias[r.ID]
		}
		g := get(key)
		g.c.add(r)
		g.rows = append(g.rows, r)
		if g.last.Data == nil || !r.CreatedAt.Before(g.last.CreatedAt) {
			g.last = r
		}
	}
	return order
}

func totalsFromRows(rows []Value) DayTotals {
	t := DayTotals{Sum: map[string]int64{}, Unknown: map[string]int{}, Rows: len(dedupeRows(rows))}
	for _, g := range groupRows(rows) {
		for _, key := range AllKeys {
			if v := g.c.value(key); v.OK {
				t.Sum[key] += v.V
			} else {
				t.Unknown[key]++
			}
		}
	}
	return t
}

// itemGroup returns the contribution group of one item (nil when absent).
func itemGroup(rows []Value, itemID string) *group {
	key := "i:" + itemID
	if strings.HasPrefix(itemID, "v:") {
		key = itemID // an external row key is the group key itself
	}
	for _, g := range groupRows(rows) {
		if g.key == key {
			return g
		}
	}
	return nil
}

// itemContrib reduces the authoritative (deduplicated) rows of one item: its
// original row and every correction of it. orig is the original row (zero
// Value when absent); ok is false when no row of the item is present.
func itemContrib(rows []Value, itemID string) (*contrib, Value, bool) {
	if g := itemGroup(rows, itemID); g != nil {
		return g.c, g.orig, g.c.rows > 0
	}
	return newContrib(), Value{}, false
}
