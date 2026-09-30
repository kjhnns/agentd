package fuel

import (
	"context"
	"sort"
	"sync"
	"time"
)

// dayCache is one date's Food log rows plus whether a strength habit value
// (Push ups / Pull ups) exists that day.
type dayCache struct {
	rows     []Value
	strength bool
	fetched  time.Time
	ok       bool // at least one successful read
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
	journal  *Journal
	now      func() time.Time
	readTO   time.Duration
	bodyTO   time.Duration // the one-shot history read (startup, 20 min loop)
	refreshM sync.Map      // date -> *sync.Mutex (one refresh per day at a time)
	publish  sync.Locker   // the service render lock (stateMu), taken to publish
}

func newCache(v Variables, ids VarIDs, j *Journal, now func() time.Time) *Cache {
	return &Cache{vars: v, ids: ids, days: map[string]*dayCache{}, gen: map[string]uint64{}, journal: j, now: now, readTO: 5 * time.Second, bodyTO: 20 * time.Second}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.journal != nil {
		for _, op := range c.journal.DoneRowsFor(date, c.now().Add(-60*time.Second)) {
			if !seen[op.ValueID] {
				dc.rows = append(dc.rows, opValue(op, c.ids.Food))
			}
		}
	}
	c.days[date] = dc
	c.gen[date]++
	return nil
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

func (c *Cache) rowsLocked(date string) ([]Value, time.Time, bool) {
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
	for _, v := range vals {
		if v.VariableID == c.ids.Body && v.Data != nil {
			body = append(body, v)
		}
	}
	sort.Slice(body, func(a, b int) bool { return body[a].RecordDate < body[b].RecordDate })
	c.mu.Lock()
	c.body, c.bodyAt = body, c.now()
	c.mu.Unlock()
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
	sorted := append([]Value(nil), rows...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if !sorted[a].CreatedAt.Equal(sorted[b].CreatedAt) {
			return sorted[a].CreatedAt.Before(sorted[b].CreatedAt)
		}
		return sorted[a].ID < sorted[b].ID
	})
	seen := map[string]bool{}
	var out []Value
	for _, r := range sorted {
		if r.Data == nil {
			continue
		}
		if op, _ := r.Data["op_id"].(string); op != "" {
			if seen[op] {
				continue
			}
			seen[op] = true
		}
		out = append(out, r)
	}
	return out
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
	for _, k := range MacroKeys {
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
		*f = c.value(MacroKeys[i])
	}
	return m
}

// cancel is the correction that brings the known sum of every field to 0; a
// field no row ever knew stays null (null stays null).
func (c *contrib) cancel() Macros {
	var m Macros
	for i, f := range m.fields() {
		k := MacroKeys[i]
		if c.known[k] {
			*f = known(-c.sum[k])
		}
	}
	return m
}

// residual reports whether any known sum is non-zero.
func (c *contrib) residual() bool {
	for _, k := range MacroKeys {
		if c.sum[k] != 0 {
			return true
		}
	}
	return false
}

func groupKey(r Value) string {
	if s, _ := r.Data["corrects"].(string); s != "" {
		return "i:" + s
	}
	if s, _ := r.Data["item_id"].(string); s != "" {
		return "i:" + s
	}
	return "v:" + r.ID
}

func totalsFromRows(rows []Value) DayTotals {
	rows = dedupeRows(rows)
	groups := map[string]*contrib{}
	var order []string
	for _, r := range rows {
		key := groupKey(r)
		g := groups[key]
		if g == nil {
			g = newContrib()
			groups[key] = g
			order = append(order, key)
		}
		g.add(r)
	}
	t := DayTotals{Sum: map[string]int64{}, Unknown: map[string]int{}, Rows: len(rows)}
	for _, k := range order {
		g := groups[k]
		for _, key := range MacroKeys {
			if v := g.value(key); v.OK {
				t.Sum[key] += v.V
			} else {
				t.Unknown[key]++
			}
		}
	}
	return t
}

// itemContrib reduces the authoritative (deduplicated) rows of one item: its
// original row and every correction of it. orig is the original row (zero
// Value when absent); ok is false when no row of the item is present.
func itemContrib(rows []Value, itemID string) (*contrib, Value, bool) {
	c := newContrib()
	var orig Value
	for _, r := range dedupeRows(rows) {
		if groupKey(r) != "i:"+itemID {
			continue
		}
		if _, isCorr := r.Data["corrects"]; !isCorr {
			orig = r
		}
		c.add(r)
	}
	return c, orig, c.rows > 0
}
