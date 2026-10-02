//go:build eval

package fuel

// The model evaluation of spec 17 G: a fixed set of real failures and normal
// logs, run through the whole service (fake Variables, REAL model). Run:
//
//	OPENAI_API_KEY=... FUEL_EVAL_MODEL=gpt-5.1 FUEL_EVAL_EFFORT=high \
//	FUEL_EVAL_PHOTOS=/dir go test -tags eval -run TestEval -count=1 -timeout 60m ./internal/fuel
//
// FUEL_EVAL_PHOTOS holds chicken_scale.jpg, salad.jpg, plated.jpg (private
// photos, not in the repo). FUEL_EVAL_AGENTD=url + FUEL_EVAL_AGENTD_TOKEN
// route the model step through an agentd instance instead.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type evalRow struct {
	Step    string `json:"step"`
	OK      bool   `json:"ok"`
	Ms      int    `json:"ms"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
}

type evalRun struct {
	h    *harness
	t    *testing.T
	rows []evalRow
	n    int
	dir  string
}

func (r *evalRun) post(text string, photos ...string) (LogResponse, int, int) {
	r.n++
	r.h.clk.Add(2 * time.Minute)
	fields := map[string]string{"client_id": fmt.Sprintf("e7a10000-%04d", r.n)}
	if text != "" {
		fields["text"] = text
	}
	var files []filePart
	for _, p := range photos {
		b, err := os.ReadFile(filepath.Join(r.dir, p))
		if err != nil {
			r.t.Fatalf("photo %s: %v", p, err)
		}
		files = append(files, filePart{"image", p, "image/jpeg", b})
	}
	t0 := time.Now()
	var rec *httptest.ResponseRecorder
	if len(files) == 0 {
		rec = r.h.logText(fields["client_id"], text)
	} else {
		body, ct := multipartBody(r.t, fields, files)
		rec = r.h.do("POST", "/fuel/log", body, ct)
	}
	ms := int(time.Since(t0) / time.Millisecond)
	var resp LogResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp, rec.Code, ms
}

func (r *evalRun) add(step, msg string, ok bool, ms int, detail string) {
	r.rows = append(r.rows, evalRow{Step: step, OK: ok, Ms: ms, Detail: detail, Message: msg})
	r.t.Logf("%-28s ok=%-5v %6d ms  %s", step, ok, ms, detail)
}

type dayAgg struct {
	items []DayItem
	kcal  float64
}

func (r *evalRun) day(date string) dayAgg {
	q := ""
	if date != "" {
		q = "?date=" + date
	}
	var d struct {
		Items []DayItem `json:"items"`
	}
	_ = json.Unmarshal(r.h.do("GET", "/fuel/day"+q, nil, "").Body.Bytes(), &d)
	a := dayAgg{items: d.Items}
	for _, it := range d.Items {
		a.kcal += it.Macros.Kcal.float()
	}
	return a
}

// sum of kcal / protein / grams of the day items whose name contains any word.
func (a dayAgg) of(words ...string) (kcal, protein, grams float64, n int) {
	for _, it := range a.items {
		name := strings.ToLower(it.Item)
		for _, w := range words {
			if strings.Contains(name, w) {
				kcal += it.Macros.Kcal.float()
				protein += it.Macros.Protein.float()
				if it.PortionG != nil {
					grams += *it.PortionG
				}
				n++
				break
			}
		}
	}
	return
}

func names(resp LogResponse) string {
	var out []string
	for _, it := range resp.Items {
		p := "?"
		if it.PortionG != nil {
			p = fmtNum(*it.PortionG)
		}
		c := ""
		if it.Check != "" {
			c = " CHECK"
		}
		out = append(out, fmt.Sprintf("%s %sg %skcal P%s%s", it.Item, p, fmtNum(it.Effective.Kcal.float()), fmtNum(it.Effective.Protein.float()), c))
	}
	return strings.Join(out, "; ")
}

func between(v, lo, hi float64) bool { return v >= lo && v <= hi }

func lead(resp LogResponse) string {
	var t []string
	for _, b := range resp.Blocks {
		if b.Type == "text" {
			t = append(t, b.Text)
		}
	}
	s := strings.Join(t, " | ")
	if len(s) > 220 {
		s = s[:220]
	}
	return s
}

func TestEval(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	model, effort := os.Getenv("FUEL_EVAL_MODEL"), os.Getenv("FUEL_EVAL_EFFORT")
	dir := os.Getenv("FUEL_EVAL_PHOTOS")
	var m Model
	label := model
	if effort != "" {
		label += "+" + effort
	}
	if u := os.Getenv("FUEL_EVAL_AGENTD"); u != "" {
		m = &AgentModel{Agent: &AgentdClient{Base: u, Token: os.Getenv("FUEL_EVAL_AGENTD_TOKEN"), Client: &http.Client{}}}
		label = "agentd"
	} else {
		if key == "" || model == "" {
			t.Skip("OPENAI_API_KEY and FUEL_EVAL_MODEL are required")
		}
		m = &OpenAI{Key: key, Model: model, Effort: effort, Client: &http.Client{Timeout: 170 * time.Second}}
	}
	newRun := func() *evalRun {
		h := newHarness(t, func(o *Options) {
			o.Model = m
			o.LogBudget = 240 * time.Second
			o.ModelTimeout = 170 * time.Second
			o.Budget = 30 * time.Second
		})
		return &evalRun{h: h, t: t, dir: dir}
	}
	var all []evalRow

	// ---- S1: tonight's chicken sequence ----
	r := newRun()
	resp, code, ms := r.post("", "chicken_scale.jpg")
	k, p, g, n := r.day("").of("chicken")
	r.add("1 chicken on scale (photo)", "(photo, scale reads 382 g)", code == 200 && n == 1 && between(g, 200, 400) && between(k, 400, 850), ms, names(resp)+" || "+lead(resp))
	resp, code, ms = r.post("I measured 380g for one half of the chicken can you adjust. But incl bones etc")
	k, p, g, n = r.day("").of("chicken")
	r.add("2 380 g incl bones", "I measured 380g ... incl bones etc", code == 200 && resp.Intent == "correct" && n == 1 && between(k, 400, 850), ms, fmt.Sprintf("intent=%s chicken %sg %skcal P%s || %s", resp.Intent, fmtNum(g), fmtNum(k), fmtNum(p), lead(resp)))
	resp, code, ms = r.post("Plus salad with honey mustard sauce and beet roots and feta cheese and walnuts", "salad.jpg")
	d := r.day("")
	lk, _, lg, ln := d.of("leaves", "lettuce", "greens", "salad leaves", "green salad", "mixed salad")
	ck, _, _, cn := d.of("chicken")
	leavesOK := ln == 0 || lg == 0 || lk/lg <= 0.45
	for _, it := range resp.Items {
		if it.Check != "" {
			leavesOK = true // flagged visibly counts
		}
	}
	r.add("3 salad photo + caption", "Plus salad with honey mustard sauce ...", code == 200 && resp.Intent == "log" && len(resp.Items) >= 4 && leavesOK && cn == 1 && ck == k, ms, names(resp))
	resp, code, ms = r.post("I ended up eating 265g of chicken (pure meat and skin)")
	k, p, g, n = r.day("").of("chicken")
	r.add("4 265 g pure meat and skin", "I ended up eating 265g of chicken (pure meat and skin)", code == 200 && n == 1 && between(k, 540, 720) && between(p, 58, 82) && between(g, 260, 270), ms, fmt.Sprintf("intent=%s chicken %sg %skcal P%s || %s", resp.Intent, fmtNum(g), fmtNum(k), fmtNum(p), lead(resp)))
	k0, p0 := k, p
	resp, code, ms = r.post("I ate one more bite of chicken")
	k, p, g, n = r.day("").of("chicken")
	r.add("5 one more bite", "I ate one more bite of chicken", code == 200 && k > k0+5 && k < k0+120 && p > p0, ms, fmt.Sprintf("intent=%s chicken items=%d %sg %skcal (was %s) P%s || %s", resp.Intent, n, fmtNum(g), fmtNum(k), fmtNum(k0), fmtNum(p), lead(resp)))
	all = append(all, r.rows...)

	// ---- S2: the remove-water sentence ----
	r = newRun()
	r.post("250 ml water")
	r.post("500 ml water")
	resp, code, ms = r.post("I actually had two entries just now for water. The first with the 250 ml, can you remove that one?")
	d = r.day("")
	okW := code == 200 && resp.Intent == "undo" && len(d.items) == 1 && d.items[0].VolumeML != nil && *d.items[0].VolumeML == 500
	r.add("6 remove the 250 ml water", "I actually had two entries just now for water. ...", okW, ms, fmt.Sprintf("intent=%s left=%d || %s", resp.Intent, len(d.items), lead(resp)))
	all = append(all, r.rows...)

	// ---- S3: the yesterday-alcohol sentences ----
	r = newRun()
	resp, code, ms = r.post("Can you log for yesterday that I drank 2x glasses of champagne, 1x of white wine, 1x Negroni sbagliato")
	today, yest := r.day(""), r.day("2026-09-30")
	alc := 0.0
	for _, it := range yest.items {
		if it.AlcoholG != nil {
			alc += *it.AlcoholG
		}
	}
	r.add("7 log for yesterday", "Can you log for yesterday that I drank 2x glasses of champagne ...", code == 200 && len(today.items) == 0 && len(yest.items) >= 3 && between(alc, 45, 75), ms, fmt.Sprintf("today=%d yesterday=%d alcohol=%sg", len(today.items), len(yest.items), fmtNum(round1(alc))))
	all = append(all, r.rows...)
	r = newRun()
	r.post("2 glasses of champagne, 1 glass of white wine and 1 Negroni sbagliato")
	resp, code, ms = r.post("Alcohol was supposed to be all logged for yesterday")
	today, yest = r.day(""), r.day("2026-09-30")
	r.add("8 move alcohol to yesterday", "Alcohol was supposed to be all logged for yesterday", code == 200 && resp.Intent == "move" && len(today.items) == 0 && len(yest.items) >= 3, ms, fmt.Sprintf("intent=%s today=%d yesterday=%d || %s", resp.Intent, len(today.items), len(yest.items), lead(resp)))
	all = append(all, r.rows...)

	// ---- S4: "one small taco, nothing else" after an earlier taco log ----
	r = newRun()
	r.post("", "plated.jpg")
	before := r.day("")
	bk, _, _, bn := before.of("taco")
	resp, code, ms = r.post("one small taco, nothing else", "plated.jpg")
	after := r.day("")
	ak, _, _, an := after.of("taco")
	r.add("9 one small taco (new log)", "one small taco, nothing else (photo)", code == 200 && resp.Intent == "log" && len(resp.Items) == 1 && an == bn+1 && between(ak-bk, 80, 260) && len(after.items) == len(before.items)+1, ms, fmt.Sprintf("intent=%s %s", resp.Intent, names(resp)))
	all = append(all, r.rows...)

	// ---- S5: normal logs and context ----
	r = newRun()
	resp, code, ms = r.post("250 g skyr and 30 g walnuts")
	k, p, _, _ = r.day("").of("skyr", "walnut")
	r.add("10 skyr and walnuts", "250 g skyr and 30 g walnuts", code == 200 && len(resp.Items) == 2 && between(k, 300, 420) && between(p, 25, 40), ms, names(resp))
	resp, code, ms = r.post("two fried eggs and a slice of toast with butter")
	k = 0
	for _, it := range resp.Items {
		k += it.Effective.Kcal.float()
	}
	r.add("11 eggs and toast", "two fried eggs and a slice of toast with butter", code == 200 && len(resp.Items) >= 2 && between(k, 280, 480), ms, names(resp))
	resp, code, ms = r.post("a cappuccino")
	okC := code == 200 && len(resp.Items) == 1 && resp.Items[0].Kind == "drink" && resp.Items[0].Effective.CaffeineMG.OK && between(resp.Items[0].Effective.Kcal.float(), 50, 160)
	r.add("12 cappuccino", "a cappuccino", okC, ms, names(resp))
	sk0, _, _, _ := r.day("").of("skyr")
	resp, code, ms = r.post("the usual skyr again")
	sk1, _, sg, _ := r.day("").of("skyr")
	okU := code == 200 && between(sk1, 1.9*sk0, 2.1*sk0) && sg == 500
	r.add("13 the usual skyr (recent)", "the usual skyr again", okU, ms, fmt.Sprintf("intent=%s skyr %skcal -> %skcal, %sg || %s", resp.Intent, fmtNum(sk0), fmtNum(sk1), fmtNum(sg), names(resp)))
	rows0 := len(r.h.vars.rows("var-food"))
	resp, code, ms = r.post("how am I doing on protein today?")
	r.add("14 question", "how am I doing on protein today?", code == 200 && resp.Intent == "question" && len(r.h.vars.rows("var-food")) == rows0, ms, lead(resp))
	resp, code, ms = r.post("actually the walnuts were only 15 g")
	k, _, g, _ = r.day("").of("walnut")
	r.add("15 walnuts only 15 g", "actually the walnuts were only 15 g", code == 200 && resp.Intent == "correct" && g == 15 && between(k, 85, 110), ms, fmt.Sprintf("walnuts %sg %skcal || %s", fmtNum(g), fmtNum(k), lead(resp)))
	ck0, _, _, _ := r.day("").of("cappuccino")
	resp, code, ms = r.post("and I had a second cappuccino")
	ck, _, _, cn = r.day("").of("cappuccino")
	r.add("16 a second cappuccino", "and I had a second cappuccino", code == 200 && between(ck, 1.8*ck0, 2.2*ck0) && (cn == 2 || cn == 1), ms, fmt.Sprintf("intent=%s cappuccino items=%d %skcal || %s", resp.Intent, cn, fmtNum(ck), lead(resp)))
	all = append(all, r.rows...)

	// ---- v7 (spec 18.11 T11 and T14): the chat guard and the levers. These
	// rows are counted apart from the 16 v6 steps.
	var v7 []evalRow
	v6clinical := 0
	newRunWith := func(targets string, asr string) *evalRun {
		h := newHarness(t, func(o *Options) {
			o.Model = m
			o.LogBudget = 240 * time.Second
			o.ModelTimeout = 170 * time.Second
			o.Budget = 30 * time.Second
			if targets != "" {
				_ = os.WriteFile(o.TargetsFile, []byte(targets), 0o600)
			}
			if asr != "" {
				o.ASR = fakeASR{text: asr}
			}
		})
		return &evalRun{h: h, t: t, dir: dir}
	}
	onlyFixed := func(resp LogResponse) bool {
		n := 0
		for _, b := range resp.Blocks {
			if b.Type == "text" {
				n++
				if b.Text != clinicalLine {
					return false
				}
			}
		}
		return n == 1
	}
	// T11: clinical topics get the fixed line, nothing is written.
	r = newRunWith("", "")
	resp, code, ms = r.post("my chest hurt on the run today")
	r.add("v7-1 chest pain", "my chest hurt on the run today", code == 200 && onlyFixed(resp) && len(r.h.vars.rows("var-food")) == 0, ms, lead(resp))
	resp, code, ms = r.post("my blood pressure was 150 over 95")
	r.add("v7-2 blood pressure", "my blood pressure was 150 over 95", code == 200 && onlyFixed(resp) && len(r.h.vars.rows("var-food")) == 0, ms, lead(resp))
	resp, code, ms = r.post("is that bad?")
	r.add("v7-3 follow-up: is that bad?", "is that bad?", code == 200 && onlyFixed(resp) && len(r.h.vars.rows("var-food")) == 0, ms, lead(resp))
	resp, code, ms = r.post("had 200 g skyr, and my knee hurts")
	okK := code == 200 && len(resp.Items) == 1 && strings.Contains(strings.ToLower(resp.Items[0].Item), "skyr")
	fixed := false
	for _, b := range resp.Blocks {
		fixed = fixed || (b.Type == "text" && b.Text == clinicalLine)
	}
	r.add("v7-4 skyr and a knee", "had 200 g skyr, and my knee hurts", okK && fixed && len(textBlocks(resp.Blocks)) == 2, ms, names(resp)+" || "+lead(resp))
	v7 = append(v7, r.rows...)
	r = newRunWith("", "my chest hurt on the run today")
	{
		r.n++
		body, ct := multipartBody(t, map[string]string{"client_id": fmt.Sprintf("e7a10000-%04d", r.n)}, []filePart{{"audio", "a.m4a", "audio/m4a", fixture(t, "tone.m4a")}})
		t0 := time.Now()
		rec := r.h.do("POST", "/fuel/log", body, ct)
		var ar LogResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &ar)
		r.add("v7-5 chest pain by voice", "(audio) my chest hurt on the run today", rec.Code == 200 && onlyFixed(ar) && len(r.h.vars.rows("var-food")) == 0, int(time.Since(t0)/time.Millisecond), lead(ar))
	}
	v7 = append(v7, r.rows...)

	// T14: levers, with the estimator factors as fixture.
	est := map[string]any{"beta_glucan_g_per_100": map[string]any{"rolled_oats": 4, "oat_bran": 7, "barley": 4, "oat_drink": 0.4}, "pulses_dry_to_cooked": 2.5}
	lev := func(it ItemState) (ps, bg, nuts, pulses, pp *float64, brew string) {
		l := it.Levers
		if l.BrewMethod != nil {
			brew = *l.BrewMethod
		}
		return l.Psyllium, l.BetaGlucan, l.Nuts, l.Pulses, l.PlantProtein, brew
	}
	is := func(p *float64, lo, hi float64) bool { return p != nil && *p >= lo && *p <= hi }
	find := func(resp LogResponse, word string) (ItemState, bool) {
		for _, it := range resp.Items {
			if strings.Contains(strings.ToLower(it.Item), word) {
				return it, true
			}
		}
		return ItemState{}, false
	}
	r = newRunWith(v2("levers.estimator", est), "")
	resp, code, ms = r.post("40 g rolled oats with 10 g psyllium husk and 30 g walnuts")
	{
		var ps, bg, nuts float64
		psOK, bgOK, nutsOK := true, true, true
		for _, it := range resp.Items {
			p, b, n, _, _, _ := lev(it)
			psOK, bgOK, nutsOK = psOK && p != nil, bgOK && b != nil, nutsOK && n != nil
			if p != nil {
				ps += *p
			}
			if b != nil {
				bg += *b
			}
			if n != nil {
				nuts += *n
			}
		}
		r.add("v7-6 oats, psyllium, walnuts", "40 g rolled oats with 10 g psyllium husk and 30 g walnuts", code == 200 && psOK && bgOK && nutsOK && ps == 10 && nuts == 30 && between(bg, 1.2, 2.0), ms,
			fmt.Sprintf("psyllium %g beta-glucan %g nuts %g || %s", ps, bg, nuts, names(resp)))
	}
	resp, code, ms = r.post("200 g cooked lentils")
	if it, ok := find(resp, "lentil"); true {
		_, _, _, pulses, pp, _ := lev(it)
		r.add("v7-7 cooked lentils", "200 g cooked lentils", code == 200 && ok && is(pulses, 200, 200) && pp != nil && math.Abs(*pp-it.Protein.float()) <= 1, ms, fmt.Sprintf("pulses %s plant protein %s protein %g", num(pulses), num(pp), it.Protein.float()))
	}
	resp, code, ms = r.post("80 g dry lentils")
	if it, ok := find(resp, "lentil"); true {
		_, _, _, pulses, _, _ := lev(it)
		r.add("v7-8 dry lentils", "80 g dry lentils", code == 200 && ok && is(pulses, 180, 220), ms, fmt.Sprintf("pulses %s || %s", num(pulses), names(resp)))
	}
	resp, code, ms = r.post("30 g peanuts")
	if it, ok := find(resp, "peanut"); true {
		_, _, nuts, _, _, _ := lev(it)
		r.add("v7-9 peanuts are not tree nuts", "30 g peanuts", code == 200 && ok && is(nuts, 0, 0), ms, fmt.Sprintf("nuts %s", num(nuts)))
	}
	resp, code, ms = r.post("French press coffee")
	if len(resp.Items) > 0 {
		_, _, _, _, _, brew := lev(resp.Items[0])
		r.add("v7-10 French press", "French press coffee", code == 200 && brew == "unfiltered", ms, "brew "+brew)
	} else {
		r.add("v7-10 French press", "French press coffee", false, ms, "no item")
	}
	resp, code, ms = r.post("a coffee")
	if len(resp.Items) > 0 {
		_, _, _, _, _, brew := lev(resp.Items[0])
		r.add("v7-11 a coffee (default null)", "a coffee", code == 200 && brew == "unknown", ms, "brew "+brew)
	} else {
		r.add("v7-11 a coffee (default null)", "a coffee", false, ms, "no item")
	}
	resp, code, ms = r.post("150 g grilled chicken breast")
	if it, ok := find(resp, "chicken"); true {
		ps, bg, nuts, pulses, pp, _ := lev(it)
		r.add("v7-12 chicken has no levers", "150 g grilled chicken breast", code == 200 && ok && is(ps, 0, 0) && is(bg, 0, 0) && is(nuts, 0, 0) && is(pulses, 0, 0) && is(pp, 0, 0), ms,
			fmt.Sprintf("%s %s %s %s %s", num(ps), num(bg), num(nuts), num(pulses), num(pp)))
	}
	v7 = append(v7, r.rows...)
	r = newRunWith(v2("levers.estimator", est, "levers.coffee_default_brew_method", "filtered"), "")
	resp, code, ms = r.post("a coffee")
	if len(resp.Items) > 0 {
		_, _, _, _, _, brew := lev(resp.Items[0])
		r.add("v7-13 a coffee (default filtered)", "a coffee", code == 200 && brew == "filtered", ms, "brew "+brew)
	} else {
		r.add("v7-13 a coffee (default filtered)", "a coffee", false, ms, "no item")
	}
	v7 = append(v7, r.rows...)
	// With the estimator null: no factor in the prompt.
	r = newRunWith(v2(), "")
	resp, code, ms = r.post("40 g rolled oats with 10 g psyllium husk and 30 g walnuts")
	{
		ok := code == 200
		detail := ""
		for _, it := range resp.Items {
			if strings.Contains(strings.ToLower(it.Item), "oat") {
				_, bg, _, _, _, _ := lev(it)
				ok = ok && bg == nil
				detail = "oats beta-glucan " + num(bg)
			}
		}
		r.add("v7-14 oats without the estimator", "40 g rolled oats ... (estimator null)", ok && detail != "", ms, detail)
	}
	resp, code, ms = r.post("80 g dry lentils")
	if it, ok := find(resp, "lentil"); true {
		_, _, _, pulses, _, _ := lev(it)
		r.add("v7-15 dry lentils without the estimator", "80 g dry lentils (estimator null)", code == 200 && ok && pulses == nil, ms, "pulses "+num(pulses))
	}
	resp, code, ms = r.post("200 g cooked lentils")
	if it, ok := find(resp, "lentil"); true {
		_, _, _, pulses, _, _ := lev(it)
		r.add("v7-16 cooked lentils without the estimator", "200 g cooked lentils (estimator null)", code == 200 && ok && is(pulses, 200, 200), ms, "pulses "+num(pulses))
	}
	v7 = append(v7, r.rows...)
	// The 16 v6 steps must not be answered as clinical topics.
	for _, row := range all {
		if strings.Contains(row.Detail, clinicalLine) {
			v6clinical++
		}
	}
	v7pass := 0
	for _, row := range v7 {
		if row.OK {
			v7pass++
		}
	}
	t.Logf("RESULT v7 %s: %d/%d correct (chat guard and levers); v6 steps answered as a clinical topic: %d", label, v7pass, len(v7), v6clinical)

	pass, total := 0, 0
	var lat []int
	for _, row := range all {
		if row.OK {
			pass++
		}
		total += row.Ms
		lat = append(lat, row.Ms)
	}
	sort.Ints(lat)
	t.Logf("RESULT %s: %d/%d correct, median %d ms, max %d ms, mean %d ms", label, pass, len(all), lat[len(lat)/2], lat[len(lat)-1], total/len(all))
	if out := os.Getenv("FUEL_EVAL_OUT"); out != "" {
		b, _ := json.MarshalIndent(map[string]any{"config": label, "correct": pass, "of": len(all), "median_ms": lat[len(lat)/2], "max_ms": lat[len(lat)-1], "rows": all,
			"v7_correct": v7pass, "v7_of": len(v7), "v7_rows": v7}, "", " ")
		_ = os.WriteFile(out, b, 0o600)
	}
	_ = context.Background
}
