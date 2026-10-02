package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	logYesterdaySentence = "Can you log for yesterday that I drank 2x glasses of champagne, 1x of white wine, 1x Negroni sbagliato"
	moveSentence         = "Alcohol was supposed to be all logged for yesterday"
)

func drinkItem(name string, ml, kcal, alcohol float64) string {
	return fmt.Sprintf(`{"item":%q,"kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"stated","kcal":%g,"protein_g":0,"carbs_g":2,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":%g,"caffeine_mg":null,"alcohol_g":%g}`, name, kcal, ml, alcohol)
}

func alcoholLog(day string, extra ...string) string {
	items := []string{drinkItem("champagne", 300, 240, 28.4), drinkItem("white wine", 150, 120, 14.2), drinkItem("Negroni sbagliato", 120, 150, 15)}
	items = append(items, extra...)
	return fmt.Sprintf(`{"intent":"log","day":%s,"time":null,"items":[%s],"text":"","widgets":[]}`, day, strings.Join(items, ","))
}

func moveOut(day string, names ...string) string {
	targets := "[]"
	if len(names) > 0 {
		b, _ := json.Marshal(names)
		targets = fmt.Sprintf(`[{"ref":"last","names":%s,"which":null,"volume_ml":null,"portion_g":null}]`, b)
	}
	return fmt.Sprintf(`{"intent":"move","day":%s,"time":null,"items":[],"corrections":[],"targets":%s,"text":"","widgets":[]}`, day, targets)
}

func dayNames(d dayResp) string {
	var n []string
	for _, it := range d.Items {
		n = append(n, it["item"].(string))
	}
	return strings.Join(n, "|")
}

func TestLogForYesterdayExactSentence(t *testing.T) {
	h := newHarness(t)
	var now time.Time
	h.model.fn = func(in ModelInput) string { now = in.Now; return alcoholLog(`"yesterday"`) }
	r := h.logText("m0000001-0001", logYesterdaySentence)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if now.Format("2006-01-02 15:04") != "2026-10-01 12:00" || !strings.Contains(systemFor(ModelInput{Now: now}), "Now: Thursday 2026-10-01 12:00") || !strings.Contains(systemFor(ModelInput{Now: now}), "Yesterday was Wednesday 2026-09-30") {
		t.Fatalf("model clock %v", now)
	}
	rows := h.vars.rows("var-food")
	if len(rows) != 3 {
		t.Fatalf("rows %d", len(rows))
	}
	for _, row := range rows {
		if row["_record_date"] != "2026-09-30" || row["eaten_at"] != "2026-09-30T12:00:00+02:00" {
			t.Fatalf("row on %v at %v", row["_record_date"], row["eaten_at"])
		}
	}
	if resp.Snapshot.Date != "2026-09-30" || intake(resp.Snapshot, "alcohol_g").Consumed != 57.6 {
		t.Fatalf("snapshot %s alcohol %v", resp.Snapshot.Date, intake(resp.Snapshot, "alcohol_g").Consumed)
	}
	if resp.Blocks[0].Text != "Logged for Wed 30 Sep: champagne, white wine and Negroni sbagliato." {
		t.Fatalf("text %q", resp.Blocks[0].Text)
	}
	for _, b := range resp.Blocks {
		if b.Widget != "" && b.Date != "2026-09-30" {
			t.Fatalf("widget for %s", b.Date)
		}
	}
	if len(h.day("").Items) != 0 || len(h.day("?date=2026-09-30").Items) != 3 {
		t.Fatalf("today %d yesterday %d", len(h.day("").Items), len(h.day("?date=2026-09-30").Items))
	}
	// The week total counts it once (Wed and Thu are the same ISO week).
	today := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if intake(today, "alcohol_g").Consumed != 0 || intake(today, "alcohol_g_week").Consumed != 57.6 {
		t.Fatalf("today alcohol %v week %v", intake(today, "alcohol_g").Consumed, intake(today, "alcohol_g_week").Consumed)
	}
}

func TestMoveSentenceMovesTheAlcoholOfTheNewestEntry(t *testing.T) {
	h := newHarness(t)
	peanuts := `{"item":"peanuts","kind":"food","staple_key":null,"portion_g":30,"portion_basis":"stated","kcal":170,"protein_g":7,"carbs_g":5,"net_carbs_g":null,"fat_g":14,"sat_fat_g":2,"fiber_g":2,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}`
	h.model.fn = func(ModelInput) string { return alcoholLog("null", peanuts) }
	h.logText("m0000002-0001", "2 champagne, a white wine, a Negroni sbagliato and peanuts")
	before := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`, "champagne", "white wine", "Negroni sbagliato") }
	r := h.logText("m0000002-0002", moveSentence)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if resp.Intent != "move" || resp.Blocks[0].Text != "Moved champagne, white wine and Negroni sbagliato to Wed 30 Sep." {
		t.Fatalf("intent %s text %q", resp.Intent, resp.Blocks[0].Text)
	}
	if resp.Snapshot.Date != "2026-09-30" || len(resp.Items) != 3 {
		t.Fatalf("snapshot %s items %d", resp.Snapshot.Date, len(resp.Items))
	}
	h.clk.Add(2 * time.Minute)
	td, yd := h.day(""), h.day("?date=2026-09-30")
	if dayNames(td) != "peanuts" || len(yd.Items) != 3 {
		t.Fatalf("today %q yesterday %q", dayNames(td), dayNames(yd))
	}
	for _, it := range yd.Items {
		if it["eaten_at"] != "2026-09-30T10:00:00Z" { // the same clock time, a day earlier
			t.Fatalf("eaten_at %v", it["eaten_at"])
		}
	}
	after := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if intake(after, "alcohol_g").Consumed != 0 || intake(after, "alcohol_g_week").Consumed != intake(before, "alcohol_g_week").Consumed {
		t.Fatalf("alcohol today %v, week %v -> %v", intake(after, "alcohol_g").Consumed, intake(before, "alcohol_g_week").Consumed, intake(after, "alcohol_g_week").Consumed)
	}
	if macro(after, "kcal").Consumed != 170 || macro(yd.Snapshot, "kcal").Consumed != 510 {
		t.Fatalf("kcal today %v yesterday %v", macro(after, "kcal").Consumed, macro(yd.Snapshot, "kcal").Consumed)
	}
	// Idempotent by client_id: no new rows, the same entry.
	n := len(h.vars.rows("var-food"))
	rep := decode[LogResponse](t, h.logText("m0000002-0002", moveSentence))
	if rep.EntryID != resp.EntryID || len(h.vars.rows("var-food")) != n {
		t.Fatal("move repeat not idempotent")
	}
	// The rows: an undo on today and a new row on yesterday per item.
	var undos, news int
	for _, row := range h.vars.rows("var-food") {
		if row["reason"] == "undo" && row["moved_to"] == "2026-09-30" && row["_record_date"] == "2026-10-01" {
			undos++
		}
		if row["moved_from"] != nil && row["_record_date"] == "2026-09-30" {
			news++
		}
	}
	if undos != 3 || news != 3 {
		t.Fatalf("undos %d new rows %d", undos, news)
	}
}

func TestMoveWithoutTargetsMovesTheWholeNewestEntry(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return alcoholLog("null") }
	h.logText("m0000003-0001", "drinks")
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return moveOut(`"2026-09-29"`) }
	resp := decode[LogResponse](t, h.logText("m0000003-0002", "the last entry was for Tuesday"))
	if resp.Blocks[0].Text != "Moved champagne, white wine and Negroni sbagliato to Tue 29 Sep." {
		t.Fatalf("%q", resp.Blocks[0].Text)
	}
	h.clk.Add(2 * time.Minute)
	if len(h.day("").Items) != 0 || len(h.day("?date=2026-09-29").Items) != 3 {
		t.Fatal("days wrong after the move")
	}
	// Moving again to the same day: nothing to do.
	h.model.fn = func(ModelInput) string { return moveOut(`"2026-09-29"`) }
	n := len(h.vars.rows("var-food"))
	r2 := decode[LogResponse](t, h.logText("m0000003-0003", "move it to Tuesday"))
	if len(h.vars.rows("var-food")) != n {
		t.Fatalf("moved again: %q", r2.Blocks[0].Text)
	}
}

func TestDayBoundaries(t *testing.T) {
	h := newHarness(t)
	one := func(day, clock string) string {
		return fmt.Sprintf(`{"intent":"log","day":%s,"time":%s,"items":[%s],"text":"","widgets":[]}`, day, clock, drinkItem("beer", 330, 140, 13))
	}
	cases := []struct {
		day, clock, wantDate, wantAt, refuse string
	}{
		{`"2026-08-28"`, "null", "2026-08-28", "2026-08-28T12:00:00+02:00", ""}, // today - 34
		{`"2026-08-27"`, "null", "", "", "too far back"},
		{`"2026-10-02"`, "null", "", "", "in the future"},
		{`"yesterday"`, `"22:30"`, "2026-09-30", "2026-09-30T22:30:00+02:00", ""},
		{`"today"`, `"08:15"`, "2026-10-01", "2026-10-01T08:15:00+02:00", ""},
		{"null", "null", "2026-10-01", "2026-10-01T10:00:00Z", ""},
		{`"today"`, `"23:00"`, "2026-10-01", "2026-10-01T10:00:00Z", ""}, // a future time today is ignored
	}
	for i, c := range cases {
		c := c
		h.model.fn = func(ModelInput) string { return one(c.day, c.clock) }
		n := len(h.vars.rows("var-food"))
		r := h.logText(fmt.Sprintf("m0000004-%04d", i), "a beer")
		if r.Code != 200 {
			t.Fatalf("case %d: %d %s", i, r.Code, r.Body)
		}
		resp := decode[LogResponse](t, r)
		rows := h.vars.rows("var-food")
		if c.refuse != "" {
			if len(rows) != n || !strings.Contains(resp.Blocks[0].Text, c.refuse) || !strings.Contains(resp.Blocks[0].Text, "Nothing was logged.") {
				t.Fatalf("case %d: rows %d->%d text %q", i, n, len(rows), resp.Blocks[0].Text)
			}
			continue
		}
		last := rows[len(rows)-1]
		if last["_record_date"] != c.wantDate || last["eaten_at"] != c.wantAt {
			t.Fatalf("case %d: %v at %v", i, last["_record_date"], last["eaten_at"])
		}
	}
	for _, bad := range []string{
		`{"intent":"log","day":"tomorrow","time":null,"items":[],"text":"","widgets":[]}`,
		`{"intent":"log","day":null,"time":"25:00","items":[],"text":"","widgets":[]}`,
		`{"intent":"move","day":null,"time":null,"items":[],"corrections":[],"targets":[],"text":"","widgets":[]}`,
	} {
		if _, err := validateOutputFor(json.RawMessage(bad), true); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	// A move to a refused day writes nothing and says why.
	h.model.fn = func(ModelInput) string { return moveOut(`"2026-07-01"`) }
	n := len(h.vars.rows("var-food"))
	r := decode[LogResponse](t, h.logText("m0000004-0100", "that was in July"))
	if len(h.vars.rows("var-food")) != n || !strings.Contains(r.Blocks[0].Text, "too far back") || !strings.Contains(r.Blocks[0].Text, "Nothing was moved.") {
		t.Fatalf("%q", r.Blocks[0].Text)
	}
}

// sums returns the kcal known sum per record date of the active rows.
func kcalByDate(h *harness) map[string]float64 {
	out := map[string]float64{}
	for _, r := range h.vars.rows("var-food") {
		out[r["_record_date"].(string)] += r["kcal"].(float64)
	}
	return out
}

func TestHalfDoneMoveRecoversAfterACrash(t *testing.T) {
	for _, storeFirst := range []bool{false, true} {
		h := newHarness(t)
		h.model.fn = func(ModelInput) string { return alcoholLog("null") }
		h.logText("m0000005-0001", "drinks")
		h.clk.Add(time.Minute)
		// Every POST of the move is uncertain; with storeFirst the rows land
		// anyway (the answer was lost), otherwise they do not.
		h.vars.mu.Lock()
		h.vars.onPost = func(int, map[string]any) (bool, int) { return storeFirst, 500 }
		h.vars.mu.Unlock()
		h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
		if r := h.logText("m0000005-0002", moveSentence); r.Code != 202 {
			t.Fatalf("storeFirst=%v: %d %s", storeFirst, r.Code, r.Body)
		}
		// Never both and never neither while it is half done: the old rows
		// still count on today (the undo waits for the new row).
		for _, op := range h.svc.journal.Ops() {
			if op.PairOp != "" && op.State != OpPending {
				t.Fatalf("undo half posted before its new row was done: %s", op.State)
			}
		}
		// During the transition exactly ONE side counts (the source), also
		// when the new rows already landed: day lists, snapshots, the week.
		h.clk.Add(2 * time.Minute)
		tdS := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
		ydS := decode[Snapshot](t, h.do("GET", "/fuel/snapshot?date=2026-09-30", nil, ""))
		if macro(tdS, "kcal").Consumed != 510 || macro(ydS, "kcal").Consumed != 0 || intake(tdS, "alcohol_g_week").Consumed != 57.6 {
			t.Fatalf("storeFirst=%v transition: today %v yesterday %v week %v", storeFirst, macro(tdS, "kcal").Consumed, macro(ydS, "kcal").Consumed, intake(tdS, "alcohol_g_week").Consumed)
		}
		if len(h.day("").Items) != 3 || len(h.day("?date=2026-09-30").Items) != 0 {
			t.Fatalf("storeFirst=%v transition day lists wrong", storeFirst)
		}
		h.vars.mu.Lock()
		h.vars.onPost = nil
		h.vars.mu.Unlock()
		h.restart() // the crash
		for i := 0; i < 3; i++ {
			h.clk.Add(61 * time.Second)
			h.svc.reconcileOnce(context.Background())
		}
		got := kcalByDate(h)
		if toTenth(got["2026-10-01"]) != 0 || got["2026-09-30"] != 510 {
			t.Fatalf("storeFirst=%v: kcal by date %v", storeFirst, got)
		}
		h.clk.Add(2 * time.Minute)
		if len(h.day("").Items) != 0 || len(h.day("?date=2026-09-30").Items) != 3 {
			t.Fatalf("storeFirst=%v: days wrong after recovery", storeFirst)
		}
		rep := decode[LogResponse](t, h.logText("m0000005-0002", moveSentence))
		if rep.Blocks[0].Text != "Moved champagne, white wine and Negroni sbagliato to Wed 30 Sep." {
			t.Fatalf("replay text %q", rep.Blocks[0].Text)
		}
	}
}

func TestMoveRejections(t *testing.T) {
	// The new row is rejected: the item stays on its old day, no undo.
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return alcoholLog("null") }
	h.logText("m0000006-0001", "drinks")
	h.clk.Add(time.Minute)
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["moved_from"] != nil && d["item"] == "white wine" {
			return false, 400
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	r1 := h.logText("m0000006-0002", moveSentence)
	resp1 := decode[LogResponse](t, r1)
	if r1.Code != 200 || resp1.Intent != "move" || resp1.Blocks[0].Text != "Moved champagne and Negroni sbagliato to Wed 30 Sep. Could not move white wine; it stays where it was." {
		t.Fatalf("partial move: %d %q", r1.Code, resp1.Blocks[0].Text)
	}
	if rep := decode[LogResponse](t, h.logText("m0000006-0002", moveSentence)); rep.Blocks[0].Text != resp1.Blocks[0].Text {
		t.Fatalf("replay %q", rep.Blocks[0].Text)
	}
	h.svc.reconcileOnce(context.Background())
	h.clk.Add(2 * time.Minute)
	if dayNames(h.day("")) != "white wine" || len(h.day("?date=2026-09-30").Items) != 2 {
		t.Fatalf("today %q yesterday %q", dayNames(h.day("")), dayNames(h.day("?date=2026-09-30")))
	}

	// The undo is rejected after the new row was written: the new row is
	// cancelled, the item stays on its old day only.
	h2 := newHarness(t)
	h2.model.fn = func(ModelInput) string { return alcoholLog("null") }
	h2.logText("m0000006-0101", "drinks")
	h2.clk.Add(time.Minute)
	h2.vars.mu.Lock()
	h2.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" && d["item"] == "correction: champagne" {
			return false, 400
		}
		return true, 201
	}
	h2.vars.mu.Unlock()
	h2.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	r2 := h2.logText("m0000006-0102", moveSentence)
	if resp2 := decode[LogResponse](t, r2); r2.Code != 200 || !strings.Contains(resp2.Blocks[0].Text, "Could not move champagne; it stays where it was.") || !strings.Contains(resp2.Blocks[0].Text, "Moved white wine and Negroni sbagliato to Wed 30 Sep.") {
		t.Fatalf("undo rejected: %d %q", r2.Code, resp2.Blocks[0].Text)
	}
	h2.svc.reconcileOnce(context.Background())
	h2.clk.Add(2 * time.Minute)
	if dayNames(h2.day("")) != "champagne" {
		t.Fatalf("today %q", dayNames(h2.day("")))
	}
	yd := h2.day("?date=2026-09-30")
	if len(yd.Items) != 2 || macro(yd.Snapshot, "kcal").Consumed != 270 {
		t.Fatalf("yesterday %q kcal %v", dayNames(yd), macro(yd.Snapshot, "kcal").Consumed)
	}
}

func TestAlcoholGuidanceInThePrompt(t *testing.T) {
	for _, want := range []string{"alcohol_g = volume_ml x ABV x 0.789", "300 ml = 28 g", "never a flat amount per drink", `"Log for yesterday that I drank X" is a log with day "yesterday"`} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("prompt lacks %q", want)
		}
	}
}

func TestMoveCompensationSurvivesACrashAfterTheFailedUndo(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("champagne", 300, 240, 28.4))
	}
	h.logText("m0000007-0001", "champagne")
	h.clk.Add(time.Minute)
	// The undo stays uncertain (not stored); the new row is written.
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return false, 500
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m0000007-0002", "that was yesterday")
	var undo Op
	for _, op := range h.svc.journal.Ops() {
		if op.PairOp != "" {
			undo = op
		}
	}
	// The crash: the undo is journaled FAILED and nothing else.
	h.svc.Close()
	_ = appendJSONLine(h.opts.StateDir+"/journal.jsonl", journalRec{T: "state", OpID: undo.ID, State: OpFailed, At: h.clk.Now()})
	h.vars.mu.Lock()
	h.vars.onPost = nil
	h.vars.mu.Unlock()
	h.start()
	h.svc.reconcileOnce(context.Background())
	h.svc.reconcileOnce(context.Background())
	got := kcalByDate(h)
	if got["2026-10-01"] != 240 || toTenth(got["2026-09-30"]) != 0 {
		t.Fatalf("kcal by date %v: the failed move left both or neither", got)
	}
	comps := 0
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "compensation" {
			comps++
		}
	}
	if comps != 1 {
		t.Fatalf("compensations %d (must be exactly one, also after a second pass)", comps)
	}
}

func TestMoveUndoIsNeverFailedByTheDeadline(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("champagne", 300, 240, 28.4))
	}
	h.logText("m0000008-0001", "champagne")
	h.clk.Add(time.Minute)
	// The undo IS stored but answers 500; then reads fail for over a day.
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return true, 500
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m0000008-0002", "that was yesterday")
	h.vars.failReads.Store(true)
	h.clk.Add(25 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	h.vars.failReads.Store(false)
	h.vars.mu.Lock()
	h.vars.onPost = nil
	h.vars.mu.Unlock()
	h.clk.Add(2 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	got := kcalByDate(h)
	if toTenth(got["2026-10-01"]) != 0 || got["2026-09-30"] != 240 {
		t.Fatalf("kcal by date %v: on neither or both days", got)
	}
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "compensation" || op.State == OpFailed {
			t.Fatalf("op %s %s %s", op.ID, op.Reason, op.State)
		}
	}
}

func TestMoveDestinationIsPendingUntilSettled(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("champagne", 300, 240, 28.4))
	}
	h.logText("m0000009-0001", "champagne")
	h.clk.Add(time.Minute)
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return false, 500
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	resp := decode[LogResponse](t, h.logText("m0000009-0002", "that was yesterday"))
	if !strings.HasPrefix(resp.Blocks[0].Text, "Still moving champagne to Wed 30 Sep.") {
		t.Fatalf("%q", resp.Blocks[0].Text)
	}
	dest := resp.Items[0].ItemID
	if r := h.fix("m0000009-0003", dest, map[string]any{"share": 0.5}); r.Code != 409 || errCode(t, r) != "pending" {
		t.Fatalf("fix on the destination: %d %s", r.Code, r.Body)
	}
	if r := h.mutate("undo", "m0000009-0004", dest, nil); r.Code != 409 {
		t.Fatalf("undo on the destination: %d", r.Code)
	}
}

func TestMoveNamesConstrainIDs(t *testing.T) {
	h := newHarness(t)
	water := drinkItem("water", 500, 0, 0)
	water = strings.Replace(water, `"alcohol_g":0`, `"alcohol_g":null`, 1)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s,%s],"text":"","widgets":[]}`, drinkItem("champagne", 300, 240, 28.4), water)
	}
	l := decode[LogResponse](t, h.logText("m0000010-0001", "champagne and water"))
	h.clk.Add(time.Minute)
	champagneID := l.Items[0].ItemID
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"move","day":"yesterday","time":null,"items":[],"corrections":[],"targets":[{"ref":%q,"names":["water"],"which":null,"volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`, champagneID)
	}
	r := decode[LogResponse](t, h.logText("m0000010-0002", "the water was yesterday"))
	if r.Blocks[0].Text != "Moved water to Wed 30 Sep." {
		t.Fatalf("%q", r.Blocks[0].Text)
	}
	h.clk.Add(2 * time.Minute)
	if dayNames(h.day("")) != "champagne" {
		t.Fatalf("today %q", dayNames(h.day("")))
	}
}

func TestResolveDayClockRules(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Zurich")
	// DST ends 2026-10-25 (25 h day): noon is 12:00 local, not 11:00.
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	y := "yesterday"
	date, at, refuse := resolveDay(&y, nil, now, loc, "2026-10-26", now)
	if refuse != "" || date != "2026-10-25" || at.In(loc).Format("15:04") != "12:00" {
		t.Fatalf("%s %s %q", date, at.In(loc).Format("15:04"), refuse)
	}
	// DST starts 2026-03-29 (23 h day).
	d := "2026-03-29"
	now = time.Date(2026, 3, 30, 9, 0, 0, 0, time.UTC)
	clock := "18:30"
	_, at, _ = resolveDay(&d, &clock, now, loc, "2026-03-30", now)
	if at.In(loc).Format("2006-01-02 15:04") != "2026-03-29 18:30" {
		t.Fatalf("%s", at.In(loc).Format("2006-01-02 15:04"))
	}
	if got := localClock("2026-10-25", 12*60, loc).In(loc).Format("15:04"); got != "12:00" {
		t.Fatal(got)
	}
	// Explicit today with a request instant from yesterday: today's time.
	now = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	base := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC)
	td, future := "today", "23:00"
	date, at, _ = resolveDay(&td, &future, now, loc, "2026-09-30", base)
	if date != "2026-10-01" || !at.Equal(now) {
		t.Fatalf("%s %v", date, at)
	}
	// A stated time 4 minutes ahead is in the future: ignored.
	near := "12:04"
	_, at, _ = resolveDay(&td, &near, now, loc, "2026-10-01", now)
	if !at.Equal(now) {
		t.Fatalf("near-future time accepted: %v", at)
	}
	exact := "12:00"
	_, at, _ = resolveDay(&td, &exact, now, loc, "2026-10-01", now)
	if at.In(loc).Format("15:04") != "12:00" {
		t.Fatal("the current minute refused")
	}
}

func champagneToday(h *harness, cid string) LogResponse {
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("champagne", 300, 240, 28.4))
	}
	return decode[LogResponse](h.t, h.logText(cid, "champagne"))
}

func setPost(h *harness, fn func(int, map[string]any) (bool, int)) {
	h.vars.mu.Lock()
	h.vars.onPost = fn
	h.vars.mu.Unlock()
}

func moveUndoOp(h *harness) Op {
	for _, op := range h.svc.journal.Ops() {
		if op.PairOp != "" {
			return op
		}
	}
	h.t.Fatal("no move undo op")
	return Op{}
}

func TestRejectedRetryAfterAnUncertainUndoStaysOpen(t *testing.T) {
	h := newHarness(t)
	champagneToday(h, "m1000001-0001")
	h.clk.Add(time.Minute)
	n := 0
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		if d["reason"] != "undo" {
			return true, 201
		}
		n++
		if n == 1 {
			return false, 500 // uncertain (in reality it may still land)
		}
		return false, 400 // the retry is rejected
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m1000001-0002", "that was yesterday")
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	u := moveUndoOp(h)
	if terminal(u.State) {
		t.Fatalf("undo %s after a rejected RETRY", u.State)
	}
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "compensation" {
			t.Fatal("the destination was cancelled although the first undo attempt may land")
		}
	}
	// The first attempt lands late.
	h.vars.add("var-food", u.Date, u.Data)
	setPost(h, nil)
	h.clk.Add(2 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	got := kcalByDate(h)
	if toTenth(got["2026-10-01"]) != 0 || got["2026-09-30"] != 240 {
		t.Fatalf("kcal by date %v", got)
	}
	if u = moveUndoOp(h); u.State != OpDone {
		t.Fatalf("undo %s", u.State)
	}
}

func TestStoredButUnacknowledgedUndoSwitchesBothSidesAtOnce(t *testing.T) {
	h := newHarness(t)
	champagneToday(h, "m1000002-0001")
	h.clk.Add(time.Minute)
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return true, 500 // stored, the answer was lost
		}
		return true, 201
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m1000002-0002", "that was yesterday")
	// No reconciliation yet; an ordinary refresh reads the stored undo.
	h.clk.Add(2 * time.Minute)
	td := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	yd := decode[Snapshot](t, h.do("GET", "/fuel/snapshot?date=2026-09-30", nil, ""))
	if macro(td, "kcal").Consumed+macro(yd, "kcal").Consumed != 240 || intake(td, "alcohol_g_week").Consumed != 28.4 {
		t.Fatalf("today %v yesterday %v week %v: not exactly one side", macro(td, "kcal").Consumed, macro(yd, "kcal").Consumed, intake(td, "alcohol_g_week").Consumed)
	}
	if len(h.day("").Items)+len(h.day("?date=2026-09-30").Items) != 1 {
		t.Fatal("day lists show both or neither")
	}
	if n := len(h.recent("").Items); n != 1 {
		t.Fatalf("recent shows %d", n)
	}
}

func TestCorrectionsOfAHiddenDestinationByValueIDDoNotCount(t *testing.T) {
	h := newHarness(t)
	champagneToday(h, "m1000003-0001")
	h.clk.Add(time.Minute)
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return false, 500 // the undo is not there: the destination is hidden
		}
		return true, 201
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m1000003-0002", "that was yesterday")
	var destValue string
	for _, r := range h.vars.rows("var-food") {
		if r["moved_from"] != nil {
			destValue = r["_id"].(string)
		}
	}
	// The food-log fixes the (hidden) destination by its VALUE id.
	h.vars.add("var-food", "2026-09-30", map[string]any{"item": "correction: champagne", "kcal": -120.0, "protein_g": 0.0, "carbs_g": -1.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0, "volume_ml": -150.0, "alcohol_g": -14.2, "source": "agentd", "reason": "fix", "corrects": destValue, "share_after": 0.5, "op_id": "op_fix_ext"})
	h.clk.Add(2 * time.Minute)
	yd := decode[Snapshot](t, h.do("GET", "/fuel/snapshot?date=2026-09-30", nil, ""))
	if macro(yd, "kcal").Consumed != 0 || intake(yd, "alcohol_g").Consumed != 0 {
		t.Fatalf("an orphan correction counted: kcal %v alcohol %v", macro(yd, "kcal").Consumed, intake(yd, "alcohol_g").Consumed)
	}
}

func TestConcurrentRepairWritesOneCompensation(t *testing.T) {
	h := newHarness(t)
	champagneToday(h, "m1000004-0001")
	h.clk.Add(time.Minute)
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return false, 500
		}
		return true, 201
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	r := decode[LogResponse](t, h.logText("m1000004-0002", "that was yesterday"))
	u := moveUndoOp(h)
	setPost(h, nil)
	// The failed-undo record alone (as after a crash): the move is NOT done.
	_ = h.svc.journal.Append(journalRec{T: "state", OpID: u.ID, State: OpFailed, At: h.clk.Now()})
	e, _ := h.svc.journal.Entry(r.EntryID)
	if st := h.svc.entryStatus(e); st != StatusPending {
		t.Fatalf("status %s before the cancellation exists", st)
	}
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() { h.svc.repairMoves(); done <- struct{}{} }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	comps := 0
	for _, row := range h.vars.rows("var-food") {
		if row["reason"] == "compensation" {
			comps++
		}
	}
	if comps != 1 {
		t.Fatalf("compensation rows %d", comps)
	}
	if st := h.svc.entryStatus(e); st != StatusDone {
		t.Fatalf("status %s after the cancellation", st)
	}
	got := kcalByDate(h)
	if got["2026-10-01"] != 240 || toTenth(got["2026-09-30"]) != 0 {
		t.Fatalf("kcal by date %v", got)
	}
}

func TestDayLabelWhenTheModelDayDiffersFromTheRequestDay(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":"today","time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("beer", 330, 140, 13))
	}
	b, _ := json.Marshal(map[string]string{"client_id": "m1000005-0001", "text": "a beer just now", "local_time": "2026-09-30T23:30:00+02:00"})
	r := decode[LogResponse](t, h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"))
	if r.Snapshot.Date != "2026-10-01" || !strings.HasPrefix(r.Blocks[0].Text, "Logged for Thu 1 Oct: beer.") {
		t.Fatalf("date %s text %q", r.Snapshot.Date, r.Blocks[0].Text)
	}
}

func TestMoveWriterLeavesAnOpTheReconcilerAttempted(t *testing.T) {
	h := newHarness(t)
	champagneToday(h, "m2000001-0001")
	h.clk.Add(time.Minute)
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "undo" {
			return false, 500
		}
		return true, 201
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m2000001-0002", "that was yesterday")
	u := moveUndoOp(h)
	if u.State != OpUncertain || u.Attempts != 1 {
		t.Fatalf("undo %s attempts %d", u.State, u.Attempts)
	}
	// A stale writer resumes now and would be rejected: it must not post.
	setPost(h, func(int, map[string]any) (bool, int) { return false, 400 })
	posts := h.vars.posts
	h.svc.postMoveOp(u.ID)
	if h.vars.posts != posts {
		t.Fatal("the writer posted an op the reconciler owns")
	}
	if u = moveUndoOp(h); terminal(u.State) {
		t.Fatalf("undo %s", u.State)
	}
}

func TestSourceIsPendingWhileAFailedMoveIsBeingCancelled(t *testing.T) {
	h := newHarness(t)
	src := champagneToday(h, "m2000002-0001").Items[0]
	h.clk.Add(time.Minute)
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		switch d["reason"] {
		case "undo":
			return false, 400 // first attempt rejected: the move failed
		case "compensation":
			return false, 500 // its cancellation stays unresolved
		}
		return true, 201
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	r := decode[LogResponse](t, h.logText("m2000002-0002", "that was yesterday"))
	if r.Status != StatusPending {
		t.Fatalf("move status %s while its cancellation is unresolved", r.Status)
	}
	if rr := h.mutate("undo", "m2000002-0003", src.ItemID, nil); rr.Code != 409 || errCode(t, rr) != "pending" {
		t.Fatalf("undo of the source: %d %s", rr.Code, rr.Body)
	}
	if rr := h.fix("m2000002-0004", src.ItemID, map[string]any{"share": 0.5}); rr.Code != 409 {
		t.Fatalf("fix of the source: %d", rr.Code)
	}
	n := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return moveOut(`"2026-09-29"`) }
	m2 := decode[LogResponse](t, h.logText("m2000002-0005", "no, Tuesday"))
	if len(h.vars.rows("var-food")) != n || !strings.Contains(m2.Blocks[0].Text, "still being saved") {
		t.Fatalf("second move: rows %d->%d text %q", n, len(h.vars.rows("var-food")), m2.Blocks[0].Text)
	}
	// Exactly one side counts meanwhile: the source.
	h.clk.Add(2 * time.Minute)
	if len(h.day("").Items) != 1 || len(h.day("?date=2026-09-30").Items) != 0 {
		t.Fatal("days wrong while the cancellation is unresolved")
	}
}

func TestDuplicateAliasesSurviveTheMoveFilter(t *testing.T) {
	h := newHarness(t)
	// A food-log row with a duplicate copy and a fix that names the COPY.
	rice := map[string]any{"item": "Rice", "portion_g": 200.0, "kcal": 240.0, "protein_g": 5.0, "carbs_g": 50.0, "fat_g": 1.0, "sat_fat_g": 0.2, "fiber_g": 1.0, "source": "agentd", "op_id": "op_ag_rice", "eaten_at": "2026-09-30T19:00:00+02:00"}
	h.vars.add("var-food", "2026-09-30", rice)
	h.vars.add("var-food", "2026-09-30", rice)
	rows := h.vars.rows("var-food")
	dup := rows[len(rows)-1]["_id"].(string)
	h.vars.add("var-food", "2026-09-30", map[string]any{"item": "correction: Rice", "kcal": -120.0, "protein_g": -2.5, "carbs_g": -25.0, "fat_g": -0.5, "sat_fat_g": -0.1, "fiber_g": -0.5, "source": "agentd", "reason": "fix", "corrects": dup, "share_after": 0.5, "portion_g_after": 100.0, "op_id": "op_fix_rice"})
	// A move destination lands on the same day.
	champagneToday(h, "m2000003-0001")
	h.clk.Add(2 * time.Minute)
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m2000003-0002", "that was yesterday")
	h.clk.Add(2 * time.Minute)
	yd := h.day("?date=2026-09-30")
	if macro(yd.Snapshot, "kcal").Consumed != 360 { // rice 120 + champagne 240
		t.Fatalf("kcal %v (the fix of the duplicate copy was orphaned)", macro(yd.Snapshot, "kcal").Consumed)
	}
	for _, it := range yd.Items {
		if it["item"] == "Rice" && it["portion_g"] != 100.0 {
			t.Fatalf("rice portion %v", it["portion_g"])
		}
	}
}

func TestMoveDestinationEverUncertainIsNeverFailed(t *testing.T) {
	h := newHarness(t)
	src := champagneToday(h, "m3000001-0001").Items[0]
	h.clk.Add(time.Minute)
	n := 0
	setPost(h, func(_ int, d map[string]any) (bool, int) {
		if d["moved_from"] == nil {
			return true, 201
		}
		n++
		if n == 1 {
			return false, 500 // uncertain: it may still land
		}
		return false, 400 // the retry is rejected
	})
	h.model.fn = func(ModelInput) string { return moveOut(`"yesterday"`) }
	h.logText("m3000001-0002", "that was yesterday")
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	h.clk.Add(25 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	for _, op := range h.svc.journal.Ops() {
		if op.State == OpFailed {
			t.Fatalf("op %s (%s) failed although its first attempt may land", op.ID, op.Kind)
		}
	}
	// The source stays pending: no second move, no undo.
	if r := h.mutate("undo", "m3000001-0003", src.ItemID, nil); r.Code != 409 {
		t.Fatalf("undo of the source: %d", r.Code)
	}
}

func TestUnnamedLastMovesTheNewestEntryNotTodaysItems(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("coffee", 200, 2, 0))
	}
	h.logText("m3000002-0001", "coffee")
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":"yesterday","time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("wine", 150, 120, 14.2))
	}
	h.logText("m3000002-0002", "wine yesterday")
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"move","day":"2026-09-29","time":null,"items":[],"corrections":[],"targets":[{"ref":"last","names":[],"which":"last","volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`
	}
	r := decode[LogResponse](t, h.logText("m3000002-0003", "that last one was Tuesday"))
	if r.Blocks[0].Text != "Moved wine to Tue 29 Sep." {
		t.Fatalf("%q", r.Blocks[0].Text)
	}
	h.clk.Add(2 * time.Minute)
	if dayNames(h.day("")) != "coffee" {
		t.Fatalf("today %q", dayNames(h.day("")))
	}
}

func TestUnnamedLastNeverFallsBackToOlderItems(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("coffee", 200, 2, 0))
	}
	h.logText("m4000001-0001", "coffee")
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"log","day":null,"time":null,"items":[%s],"text":"","widgets":[]}`, drinkItem("wine", 150, 120, 14.2))
	}
	h.logText("m4000001-0002", "wine")
	h.clk.Add(time.Minute)
	last := func(day, ml string) string {
		return fmt.Sprintf(`{"intent":"move","day":%s,"time":null,"items":[],"corrections":[],"targets":[{"ref":"last","names":[],"which":null,"volume_ml":%s,"portion_g":null}],"text":"","widgets":[]}`, day, ml)
	}
	// An amount that does not fit the newest entry: nothing moves.
	n := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return last(`"yesterday"`, "200") }
	r := decode[LogResponse](t, h.logText("m4000001-0003", "the 200 ml one was yesterday"))
	if len(h.vars.rows("var-food")) != n || !strings.Contains(r.Blocks[0].Text, "nothing in the last entry") {
		t.Fatalf("amount mismatch moved something: %q", r.Blocks[0].Text)
	}
	// Move the wine; then "the last entry" again must not take the coffee.
	h.model.fn = func(ModelInput) string { return last(`"yesterday"`, "null") }
	h.logText("m4000001-0004", "that was yesterday")
	h.clk.Add(time.Minute)
	n = len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return last(`"2026-09-29"`, "null") }
	r = decode[LogResponse](t, h.logText("m4000001-0005", "move the last entry to Tuesday"))
	if len(h.vars.rows("var-food")) != n {
		t.Fatalf("moved an older item: %q", r.Blocks[0].Text)
	}
	h.clk.Add(2 * time.Minute)
	if dayNames(h.day("")) != "coffee" {
		t.Fatalf("today %q", dayNames(h.day("")))
	}
}
