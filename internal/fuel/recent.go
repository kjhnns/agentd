package fuel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RecentItem is one row of GET /fuel/recent (spec section 15).
type RecentItem struct {
	Key      string   `json:"key"`
	Item     string   `json:"item"`
	Kind     string   `json:"kind"` // food | drink | supplement
	PortionG *float64 `json:"portion_g"`
	VolumeML *float64 `json:"volume_ml"`
	// Current effective amounts, scaled like the macros (null = unknown).
	CaffeineMG  *float64  `json:"caffeine_mg"`
	AlcoholG    *float64  `json:"alcohol_g"`
	Macros      Macros    `json:"macros"`
	LastEatenAt time.Time `json:"last_eaten_at"`
	Times       int       `json:"times"`
	PhotoID     *string   `json:"photo_id"`
	Source      string    `json:"source"`
}

const recentDays = 35

// normName is the grouping form of a food name: lowercase, trimmed,
// inner whitespace collapsed.
func normName(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// recentKey is a stable hash of kind, normalized name and the portion
// rounded to 5 g; when the portion is unknown, the volume rounded to 5 ml
// (drinks), else "none".
func recentKey(kind, name string, portion, volume *float64) string {
	p := "none"
	switch {
	case portion != nil:
		p = strconv.FormatInt(int64(math.Round(*portion/5)*5), 10)
	case volume != nil:
		p = "ml" + strconv.FormatInt(int64(math.Round(*volume/5)*5), 10)
	}
	h := sha256.Sum256([]byte(kind + "|" + normName(name) + "|" + p))
	return hex.EncodeToString(h[:8])
}

// amountsAfter returns the item's current portion and volume: the after
// values of its newest correction that carries them (Fuel fix / fraction
// rows and agentd food-log fix rows: share_after, portion_g_after,
// volume_ml_after); without such metadata, the ratio of effective to
// original macros (corrections from other writers); else the original.
func amountsAfter(g *group, eff, origM Macros) (portion, volume *float64) {
	d := g.orig.Data
	var p0, v0 *float64
	if p, ok := d["portion_g"].(float64); ok && p > 0 {
		p0 = &p
	}
	if v, ok := d["volume_ml"].(float64); ok && v > 0 {
		v0 = &v
	}
	if _, _, ok := recalBase(g); ok {
		// The recalibrated or revised values are the base of every later share.
		origM, p0, v0 = groupBase(g)
	}
	share := -1.0
	var pAfter, vAfter *float64
	corrections := 0
	for _, r := range g.rows { // oldest first: the newest wins
		if _, isCorr := r.Data["corrects"]; !isCorr {
			continue
		}
		corrections++
		if sh, ok := r.Data["share_after"].(float64); ok && sh > 0 {
			share, pAfter, vAfter = sh, nil, nil
			if p, ok := r.Data["portion_g_after"].(float64); ok {
				pAfter = &p
			}
			if v, ok := r.Data["volume_ml_after"].(float64); ok {
				vAfter = &v
			}
			continue
		}
		pa, hp := r.Data["portion_g_after"].(float64)
		va, hv := r.Data["volume_ml_after"].(float64)
		if hp || hv {
			share = -1
			pAfter, vAfter = nil, nil
			if hp {
				pAfter = &pa
			}
			if hv {
				vAfter = &va
			}
			continue
		}
		share, pAfter, vAfter = -2, nil, nil // metadata-free: fall back
	}
	scale := func(x *float64, f float64) *float64 {
		if x == nil {
			return nil
		}
		v := round1(*x * f)
		return &v
	}
	switch {
	case corrections == 0:
		return p0, v0
	case share >= 0 || pAfter != nil || vAfter != nil:
		portion, volume = pAfter, vAfter
		if portion == nil && share > 0 {
			portion = scale(p0, share)
		}
		if volume == nil && share > 0 {
			volume = scale(v0, share)
		}
		if portion == nil && share < 0 && vAfter != nil && v0 != nil && *v0 > 0 {
			portion = scale(p0, *vAfter / *v0)
		}
		if volume == nil && share < 0 && pAfter != nil && p0 != nil && *p0 > 0 {
			volume = scale(v0, *pAfter / *p0)
		}
		return portion, volume
	}
	f := 1.0
	for _, k := range []string{"kcal", "protein_g", "carbs_g", "fat_g", "volume_ml"} {
		o, e := origM.Get(k), eff.Get(k)
		if o.OK && o.V > 0 && e.OK {
			f = float64(e.V) / float64(o.V)
			break
		}
	}
	return scale(p0, f), scale(v0, f)
}

// loadWindow loads every day of the recent window that was never read (and
// re-reads today when stale). It reports whether today is fresh and all
// days are loaded.
func (s *Service) loadWindow(ctx context.Context) bool {
	today := s.today()
	// Today and yesterday must be fresh (60 s / 2 min, spec 5); older days
	// at most an hour old (the background loop rolls through the window
	// every 10 min, so this rarely reads synchronously). Any failed read
	// fails the request: a stale list must not be copied by a relog.
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := true
	for i := 0; i < recentDays; i++ {
		d := dateAdd(today, -i)
		maxAge := time.Hour
		switch i {
		case 0:
			maxAge = 60 * time.Second
		case 1:
			maxAge = 2 * time.Minute
		}
		if s.cache.Age(d) < maxAge {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.cache.RefreshDay(ctx, d); err != nil {
				log.Printf("fuel: load %s: %s", d, errClass(err))
				mu.Lock()
				ok = false
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return ok
}

// refreshWindowSlice re-reads the window days that are older than 30 min,
// at most n per call (the background loop's rolling refresh).
func (s *Service) refreshWindowSlice(ctx context.Context, n int) {
	today := s.today()
	type aged struct {
		d   string
		age time.Duration
	}
	var days []aged
	for i := 2; i < recentDays; i++ {
		d := dateAdd(today, -i)
		if a := s.cache.Age(d); a >= 30*time.Minute {
			days = append(days, aged{d, a})
		}
	}
	// The OLDEST reads first, so the whole window rotates.
	sort.Slice(days, func(a, b int) bool { return days[a].age > days[b].age })
	for i := 0; i < len(days) && i < n; i++ {
		if err := s.cache.RefreshDay(ctx, days[i].d); err != nil {
			log.Printf("fuel: refresh %s: %s", days[i].d, errClass(err))
		}
	}
}

var errWindow = errf(http.StatusBadGateway, "upstream_failed", true, "could not read the recent food log; retry")

// recentAll builds the full recent list (no limit) from every Food log row
// in the window: ACTIVE contributions only (undone items excluded), each
// with its effective macros and its portion scaled by the eaten fraction.
func (s *Service) recentAll() []RecentItem {
	today := s.today()
	var dayRows [][]Value
	s.cache.View(func() {
		for i := 0; i < recentDays; i++ {
			r, _, _ := s.cache.rowsLocked(dateAdd(today, -i))
			dayRows = append(dayRows, r)
		}
	})
	type occ struct {
		name    string
		kind    string
		volume  *float64
		portion *float64
		macros  Macros
		at      time.Time
		source  string
		photos  []string
	}
	byKey := map[string][]occ{}
	for _, rows := range dayRows {
		for _, g := range groupRows(rows) {
			if g.orig.Data == nil || g.c.undone {
				continue // a lone correction, or an undone item
			}
			d := g.orig.Data
			name, _ := d["item"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			if n := baseName(g); n != "" {
				name = n
			}
			eff := g.c.effective()
			origM := macrosFromData(d)
			origM.normalizeNetCarbs()
			portion, volume := amountsAfter(g, eff, origM)
			kind := "food"
			if k, _ := d["kind"].(string); k == "drink" || k == "supplement" {
				kind = k
			}
			at := g.orig.CreatedAt
			if ts, _ := d["eaten_at"].(string); ts != "" {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					at = t
				}
			}
			src := "other"
			switch d["source"] {
			case "fuel":
				src = "fuel"
			case "agentd":
				src = "agentd"
			}
			var photos []string
			if ref, _ := d["photo_ref"].(string); ref != "" {
				photos = strings.Split(ref, ",")
			}
			k := recentKey(kind, name, portion, volume)
			byKey[k] = append(byKey[k], occ{name: strings.TrimSpace(name), kind: kind, volume: volume, portion: portion,
				macros: eff, at: at.UTC(), source: src, photos: photos})
		}
	}
	out := make([]RecentItem, 0, len(byKey))
	for k, os := range byKey {
		sort.SliceStable(os, func(a, b int) bool { return os[a].at.After(os[b].at) })
		last := os[0]
		it := RecentItem{Key: k, Item: last.name, Kind: last.kind, PortionG: last.portion, VolumeML: last.volume, Macros: last.macros,
			CaffeineMG: last.macros.CaffeineMG.ptr(), AlcoholG: last.macros.AlcoholG.ptr(),
			LastEatenAt: last.at, Times: len(os), Source: last.source}
		for _, o := range os {
			for _, p := range o.photos {
				if s.photos.path(p) != "" {
					id := p
					it.PhotoID = &id
					break
				}
			}
			if it.PhotoID != nil {
				break
			}
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].LastEatenAt.Equal(out[b].LastEatenAt) {
			return out[a].LastEatenAt.After(out[b].LastEatenAt)
		}
		return out[a].Key < out[b].Key
	})
	return out
}

func (s *Service) handleRecent(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "limit must be 1..100"))
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.o.Budget)
	defer cancel()
	if !s.loadWindow(ctx) {
		writeErr(w, errWindow)
		return
	}
	items := s.recentAll()
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

var relogScales = []float64{0.5, 1, 1.5, 2}

// handleRelog logs a recent item again: no model call, ONE row with the
// item's macros times scale (spec section 15). Idempotent by client_id; the
// answer has the shape of POST /fuel/log.
func (s *Service) handleRelog(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if e := s.acquireSlot(r.Context()); e != nil {
		writeErr(w, e)
		return
	}
	defer func() { <-s.slots }()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.o.ReadDeadline))
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		ClientID  string   `json:"client_id"`
		Key       string   `json:"key"`
		Scale     *float64 `json:"scale"`
		LocalTime string   `json:"local_time"`
	}
	err := decodeStrict(r.Body, &body)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		if maxBytesErr(err) {
			writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", false, "body too large"))
			return
		}
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "bad JSON body"))
		return
	}
	if !clientIDRe.MatchString(body.ClientID) || body.Key == "" {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "client_id and key are required"))
		return
	}
	scale := 1.0
	if body.Scale != nil {
		ok := false
		for _, a := range relogScales {
			ok = ok || *body.Scale == a
		}
		if !ok {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "scale must be 0.5, 1, 1.5 or 2"))
			return
		}
		scale = *body.Scale
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	now := s.o.Now()
	hash := fmt.Sprintf("relog|%s|%g|%s", body.Key, scale, strings.TrimSpace(body.LocalTime))
	claim, ce := s.claim(ctx, body.ClientID, "log", hash)
	if ce != nil {
		writeErr(w, ce)
		return
	}
	if claim.replay != nil {
		s.replayLog(ctx, w, *claim.replay)
		return
	}
	defer claim.release()

	targets, terr := s.loadTargets()
	if terr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	eatenAt := now
	if body.LocalTime != "" {
		t, err := time.Parse(time.RFC3339, body.LocalTime)
		if err != nil {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "local_time must be RFC3339 with an offset"))
			return
		}
		if now.Sub(t) > 48*time.Hour || t.Sub(now) > 5*time.Minute {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "local_time more than 48 h in the past or 5 min in the future"))
			return
		}
		eatenAt = t
	}
	date := eatenAt.In(targets.loc).Format("2006-01-02")

	if !s.loadWindow(ctx) {
		writeErr(w, errWindow)
		return
	}
	var src *RecentItem
	for _, it := range s.recentAll() {
		if it.Key == body.Key {
			it := it
			src = &it
			break
		}
	}
	if src == nil {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "unknown recent item"))
		return
	}
	for _, k := range []string{"kcal", "protein_g", "carbs_g", "fat_g", "sat_fat_g"} {
		if !src.Macros.Get(k).OK {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "this item has unknown macros and cannot be logged again; describe it instead"))
			return
		}
	}
	if e := s.rateCheck(now); e != nil {
		writeErr(w, e)
		return
	}

	entry := Entry{ID: newID("en_"), ClientID: body.ClientID, Date: date, EatenAt: eatenAt, CreatedAt: now,
		Intent: "log", PhotoIDs: []string{}, ReqHash: hash, UserText: "again: " + src.Item}
	m := src.Macros.Scale(scale) // required macros are known (checked above)
	it := Item{ID: newID("it_"), EntryID: entry.ID, Date: date, Name: src.Item, Basis: "repeat", Orig: m, EatenAt: eatenAt, Kind: src.Kind}
	if src.VolumeML != nil {
		m.VolumeML = known(toTenth(*src.VolumeML * scale)) // the current volume, scaled
		it.Orig = m
	}
	if src.PortionG != nil {
		p := round1(*src.PortionG * scale)
		it.PortionG = &p
	}
	entry.ItemIDs = []string{it.ID}
	opID := newID("op_")
	op := Op{ID: opID, Kind: "original", EntryID: entry.ID, ItemID: it.ID, RowItemID: it.ID, Date: date,
		Data: originalRowData(it, opID, ""), Macros: m, CreatedAt: now, State: OpPending,
		Attempts: 1, LastTry: s.o.Now()}

	tWrite := time.Now()
	if err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: []Item{it}, Ops: []Op{op}}); err != nil {
		switch {
		case err == ErrDeadline:
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		default:
			writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request"))
		}
		return
	}
	if err := s.idem.Put(idemRec{ClientID: body.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs}); err != nil {
		log.Printf("fuel: idem reservation for %s failed (the journal identity still holds)", entry.ID)
	}
	s.runOps(ctx, []Op{op})
	writeD := time.Since(tWrite)

	ready := s.renderReady(ctx, date)
	resp, status := s.buildLogResponse(entry)
	if resp.renderErr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	resp.LatencyMs = map[string]int{"upload": 0, "asr": 0, "model": 0, "write": ms(writeD), "total": ms(time.Since(t0))}
	if ready {
		s.appendEntryFeed(entry, resp.Blocks)
	}
	code := http.StatusOK
	switch status {
	case StatusPending:
		code = http.StatusAccepted
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected the row; nothing more is written"))
		return
	}
	b, _ := jsonMarshal(resp)
	if status == StatusDone && ready {
		s.coachEvent(entry, resp.Items, resp.Snapshot)
		_ = s.idem.Put(idemRec{ClientID: body.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs, Status: code, Response: b})
	}
	s.logLine("relog", entry.ID, 1, resp.LatencyMs, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}
