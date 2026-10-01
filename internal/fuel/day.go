package fuel

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DayItem is one ACTIVE item of a day, from any writer (spec section 15.4).
type DayItem struct {
	RowKey     string    `json:"row_key"` // item_id (Fuel) or "v:<value_id>"
	Item       string    `json:"item"`
	Kind       string    `json:"kind"`
	PortionG   *float64  `json:"portion_g"`
	VolumeML   *float64  `json:"volume_ml"`
	Macros     Macros    `json:"macros"`
	CaffeineMG *float64  `json:"caffeine_mg"`
	AlcoholG   *float64  `json:"alcohol_g"`
	EatenAt    time.Time `json:"eaten_at"`
	Source     string    `json:"source"`
	PhotoID    *string   `json:"photo_id"`  // the first stored photo (compatibility)
	PhotoIDs   []string  `json:"photo_ids"` // every stored photo of the entry
	Actions    []string  `json:"actions"`
	RecentKey  string    `json:"recent_key"`
	// Recalibration is the second opinion on a Fuel photo item (spec 16).
	Recalibration *Recalibration `json:"recalibration,omitempty"`

	// For resolution (not on the wire).
	origPortion *float64
	origVolume  *float64
}

// rowKeyOf is the stable id of a contribution group: the Fuel item_id, else
// "v:<value id>" of the original row.
func rowKeyOf(g *group) string {
	if strings.HasPrefix(g.key, "i:") {
		return strings.TrimPrefix(g.key, "i:")
	}
	return g.key // "v:<value id>"
}

// dayItems lists the active items of one date from the cached rows, newest
// first. Undone items (by any writer) do not appear; corrected items show
// their current amounts.
func (s *Service) dayItems(date string) []DayItem {
	rows, _, _ := s.cache.Rows(date)
	var out []DayItem
	for _, g := range groupRows(rows) {
		if g.orig.Data == nil || g.c.undone {
			continue
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
		it := DayItem{RowKey: rowKeyOf(g), Item: strings.TrimSpace(name), Kind: kind, PortionG: portion, VolumeML: volume,
			Macros: eff, CaffeineMG: eff.CaffeineMG.ptr(), AlcoholG: eff.AlcoholG.ptr(), EatenAt: at.UTC(), Source: src,
			Actions: []string{"delete", "repeat", "fix"}, RecentKey: recentKey(kind, name, portion, volume)}
		if p, ok := d["portion_g"].(float64); ok && p > 0 {
			it.origPortion = &p
		}
		if v, ok := d["volume_ml"].(float64); ok && v > 0 {
			it.origVolume = &v
		}
		if src == "fuel" {
			if ji, ok := s.journal.Item(it.RowKey); ok {
				it.Recalibration = s.itemRecal(ji)
			}
		}
		it.PhotoIDs = []string{}
		if ref, _ := d["photo_ref"].(string); ref != "" {
			for _, p := range strings.Split(ref, ",") {
				if s.photos.path(p) != "" {
					it.PhotoIDs = append(it.PhotoIDs, p)
				}
			}
			if len(it.PhotoIDs) > 0 {
				id := it.PhotoIDs[0]
				it.PhotoID = &id
			}
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].EatenAt.Equal(out[b].EatenAt) {
			return out[a].EatenAt.After(out[b].EatenAt)
		}
		return out[a].RowKey < out[b].RowKey
	})
	return out
}

func (s *Service) handleDay(w http.ResponseWriter, r *http.Request) {
	today := s.today()
	date := r.URL.Query().Get("date")
	if date == "" {
		date = today
	}
	if _, err := time.Parse("2006-01-02", date); err != nil || date > today || date < dateAdd(today, -34) {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "date must be today or one of the 34 days before it"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.o.Budget)
	defer cancel()
	if !s.freshen(ctx, date) || !s.ensureHistory(ctx, date) {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the food log; retry"))
		return
	}
	// Items and snapshot from one view of the day.
	s.stateMu.RLock()
	items := s.dayItems(date)
	snap, err := s.snapshotFor(date)
	s.stateMu.RUnlock()
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	if items == nil {
		items = []DayItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"date": date, "items": items, "snapshot": snap})
}

// findRow locates an external row ("v:<value id>") in the cached window and
// returns its date. Fuel item ids are looked up in the journal instead.
func (s *Service) findRow(rowKey string) (string, bool) {
	id := strings.TrimPrefix(rowKey, "v:")
	today := s.today()
	for i := 0; i < recentDays; i++ {
		d := dateAdd(today, -i)
		rows, _, _ := s.cache.Rows(d)
		for _, r := range rows {
			if r.ID == id {
				return d, true
			}
		}
	}
	return "", false
}

// externalItem builds the synthetic Item that stands for an external row
// (written by another writer), so the undo/fix machinery can journal and
// lock it like a Fuel item. Its ID is the row key "v:<value id>".
func externalItem(rowKey, date string, rows []Value) (Item, bool) {
	g := itemGroup(rows, rowKey)
	if g == nil || g.orig.Data == nil {
		return Item{}, false
	}
	d := g.orig.Data
	name, _ := d["item"].(string)
	it := Item{ID: rowKey, Date: date, Name: strings.TrimSpace(name), Basis: "external", EatenAt: g.orig.CreatedAt}
	if ts, _ := d["eaten_at"].(string); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			it.EatenAt = t
		}
	}
	if k, _ := d["kind"].(string); k != "" {
		it.Kind = k
	}
	if p, ok := d["portion_g"].(float64); ok && p > 0 {
		it.PortionG = &p
	}
	m := macrosFromData(d)
	m.normalizeNetCarbs()
	it.Orig = m
	return it, true
}

func isExternalKey(k string) bool { return strings.HasPrefix(k, "v:") }
