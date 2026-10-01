package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeAgentd is a loopback stand-in for agentd's generic API: POST
// /sessions, POST /sessions/{id}/media (multipart file + text), DELETE
// /sessions/{id}. The tests go through the real AgentdClient.
type fakeAgentd struct {
	mu       sync.Mutex
	answer   func(task string) string
	hook     func() // runs inside the task turn, before the answer
	block    chan struct{}
	sessions int
	deleted  int
	texts    []string
	files    []int
	badAuth  int
}

func (f *fakeAgentd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer agent-token" {
		f.mu.Lock()
		f.badAuth++
		f.mu.Unlock()
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/sessions":
		f.mu.Lock()
		f.sessions++
		id := fmt.Sprintf("s%d", f.sessions)
		f.mu.Unlock()
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "harness": "fake", "workspace": "main"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/media"):
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(file)
		text := r.FormValue("text")
		f.mu.Lock()
		f.texts = append(f.texts, text)
		f.files = append(f.files, len(b))
		answer, hook, block := f.answer, f.hook, f.block
		f.mu.Unlock()
		result := "READY"
		if !strings.Contains(text, "Reply only: READY") {
			if block != nil {
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}
			if hook != nil {
				hook()
			}
			result = answer(text)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "artifact": "/x", "result": result})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/sessions/"):
		f.mu.Lock()
		f.deleted++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		w.WriteHeader(404)
	}
}

type recalHarness struct {
	*harness
	agent *fakeAgentd
}

func newRecalHarness(t *testing.T, mut ...func(*Options)) *recalHarness {
	fa := &fakeAgentd{}
	srv := httptest.NewServer(fa)
	t.Cleanup(srv.Close)
	all := append([]func(*Options){func(o *Options) {
		o.Recal = RecalOptions{Enabled: true, Agent: &AgentdClient{Base: srv.URL, Token: "agent-token", Client: loopbackClient()},
			Timeout: 5 * time.Second, MinInterval: time.Nanosecond, Tick: time.Hour}
	}}, mut...)
	return &recalHarness{harness: newHarness(t, all...), agent: fa}
}

func photoItem(name string, portion, kcal, protein, satFat float64, needsFraction bool) string {
	return fmt.Sprintf(`{"item":%q,"kind":"food","staple_key":null,"portion_g":%g,"portion_basis":"photo_estimate","kcal":%g,"protein_g":%g,"carbs_g":40,"net_carbs_g":null,"fat_g":10,"sat_fat_g":%g,"fiber_g":3,"needs_fraction":%v,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}`,
		name, portion, kcal, protein, satFat, needsFraction)
}

func photoMeal(items ...string) string {
	return `{"intent":"log","items":[` + strings.Join(items, ",") + `],"text":"","widgets":[]}`
}

// logPhoto logs a photo meal (n photos) and returns the response.
func (h *recalHarness) logPhoto(cid, model string, n int) LogResponse {
	h.t.Helper()
	h.model.mu.Lock()
	h.model.fn = func(ModelInput) string { return model }
	h.model.mu.Unlock()
	var files []filePart
	for i := 0; i < n; i++ {
		files = append(files, filePart{"image", fmt.Sprintf("p%d.jpg", i), "image/jpeg", testJPEG(300+i, 200)})
	}
	body, ct := multipartBody(h.t, map[string]string{"client_id": cid}, files)
	r := h.do("POST", "/fuel/log", body, ct)
	if r.Code != 200 {
		h.t.Fatalf("log %d %s", r.Code, r.Body)
	}
	return decode[LogResponse](h.t, r)
}

func opinion(matches string, name string, portion, kcal, protein, satFat float64, conf, reason string) string {
	m := "null"
	if matches != "" {
		m = fmt.Sprintf("%q", matches)
	}
	nb, _ := json.Marshal(name)
	rb, _ := json.Marshal(reason)
	return fmt.Sprintf(`{"matches":%s,"item":%s,"portion_g":%g,"kcal":%g,"protein_g":%g,"carbs_g":60,"net_carbs_g":null,"fat_g":14,"sat_fat_g":%g,"fiber_g":4,"confidence":%q,"evidence":"visual","reason":%s}`,
		m, nb, portion, kcal, protein, satFat, conf, rb)
}

func answerOf(items ...string) string {
	return "Here is my estimate.\n```json\n{\"items\":[" + strings.Join(items, ",") + "]}\n```"
}

func (h *recalHarness) say(answer string) {
	h.agent.mu.Lock()
	h.agent.answer = func(string) string { return answer }
	h.agent.mu.Unlock()
}

func (h *recalHarness) run() bool {
	h.clk.Add(time.Second) // the worker's minimum interval runs on the service clock
	return h.svc.recalRunOnce(context.Background())
}

type entryWire struct {
	Status        string         `json:"status"`
	Items         []ItemState    `json:"items"`
	Snapshot      Snapshot       `json:"snapshot"`
	Recalibration *Recalibration `json:"recalibration"`
}

func (h *recalHarness) entry(id string) entryWire {
	h.t.Helper()
	return decode[entryWire](h.t, h.do("GET", "/fuel/entry/"+id, nil, ""))
}

func (h *recalHarness) revert(cid, itemID string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"client_id": cid, "item_id": itemID})
	return h.do("POST", "/fuel/recalibration/revert", bytes.NewReader(b), "application/json")
}

func (h *recalHarness) coachLines() []string {
	var out []string
	page, _ := h.svc.feed.Page(0, 100)
	for _, it := range page {
		if it.Role == "coach" && it.Text != nil && strings.HasPrefix(it.Key, "c:") {
			out = append(out, *it.Text)
		}
	}
	return out
}

func (h *recalHarness) corrRows(reason string) []map[string]any {
	var out []map[string]any
	for _, r := range h.vars.rows("var-food") {
		if r["reason"] == reason {
			out = append(out, r)
		}
	}
	return out
}

func (h *recalHarness) dayTyped() []DayItem {
	h.t.Helper()
	return decode[struct {
		Items []DayItem `json:"items"`
	}](h.t, h.do("GET", "/fuel/day", nil, "")).Items
}

func kcalOf(s Snapshot) float64 { return macro(s, "kcal").Consumed }

func TestRecalAgree(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000001-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 2)
	it := resp.Items[0]
	if it.Recalibration == nil || it.Recalibration.State != "pending" {
		t.Fatalf("log answer must show the pending second opinion: %+v", it.Recalibration)
	}
	// 330 kcal: more than 20 kcal off but under 20 % and under 40 kcal.
	h.say(answerOf(opinion(it.ItemID, "pasta", 210, 330, 12, 2.5, "high", "portion looks right")))
	if !h.run() {
		t.Fatal("no job ran")
	}
	if n := len(h.vars.rows("var-food")); n != 1 {
		t.Fatalf("an agreeing opinion wrote rows: %d", n)
	}
	e := h.entry(resp.EntryID)
	if e.Recalibration == nil || e.Recalibration.State != "agreed" || e.Items[0].Recalibration.State != "agreed" {
		t.Fatalf("entry recalibration %+v", e.Recalibration)
	}
	if e.Recalibration.By != "agentd:fake" {
		t.Fatalf("tag %q", e.Recalibration.By)
	}
	if c := h.coachLines(); len(c) != 1 || c[0] != "Second opinion agrees." {
		t.Fatalf("coach lines %v", c)
	}
	// Two photos: two media turns in ONE session, the first only READY, the
	// second carries the adversarial task and the fast estimate; the session
	// is deleted.
	a := h.agent
	if a.sessions != 1 || a.deleted != 1 || len(a.texts) != 2 || a.badAuth != 0 {
		t.Fatalf("sessions %d deleted %d turns %d badAuth %d", a.sessions, a.deleted, len(a.texts), a.badAuth)
	}
	if !strings.Contains(a.texts[0], "Photo 1 of 2") || !strings.Contains(a.texts[1], "find the error, do not confirm") ||
		!strings.Contains(a.texts[1], it.ItemID) || a.files[0] == 0 || a.files[1] == 0 {
		t.Fatalf("turn texts %q", a.texts)
	}
	if h.run() {
		t.Fatal("the job ran twice")
	}
}

func TestRecalApplyRowBaseAndSurfaces(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000002-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false), photoItem("salad", 100, 40, 2, 0.2, false)), 1)
	pasta, salad := resp.Items[0], resp.Items[1]
	rev := resp.Snapshot.Revision
	h.say(answerOf(
		opinion(pasta.ItemID, "pasta", 300, 480, 16, 2.4, "medium", "plate fills the bowl."),
		opinion(salad.ItemID, "salad", 100, 45, 2, 0.2, "high", "fine")))
	h.run()
	rows := h.corrRows("recalibrate")
	if len(rows) != 1 {
		t.Fatalf("recalibrate rows %d", len(rows))
	}
	r := rows[0]
	// Deltas: 480-300, 16-10, 60-40 carbs, 14-10 fat, 2.4-2 sat fat, 4-3 fibre.
	for k, want := range map[string]float64{"kcal": 180, "protein_g": 6, "carbs_g": 20, "fat_g": 4, "sat_fat_g": 0.4, "fiber_g": 1, "net_carbs_g": 19, "portion_g_after": 300, "share_after": 1} {
		if got, _ := r[k].(float64); got != want {
			t.Fatalf("row %s = %v want %v (%v)", k, r[k], want, r)
		}
	}
	if r["corrects"] != pasta.ItemID || r["source"] != "fuel" || r["item"] != "correction: pasta" {
		t.Fatalf("row identity %v", r)
	}
	meta, _ := r["recalibrated"].(map[string]any)
	from, _ := meta["from"].(map[string]any)
	to, _ := meta["to"].(map[string]any)
	if from["kcal"] != 300.0 || to["kcal"] != 480.0 || to["portion_g"] != 300.0 || meta["confidence"] != "medium" || meta["by"] != "agentd:fake" {
		t.Fatalf("recalibrated meta %v", meta)
	}
	e := h.entry(resp.EntryID)
	if e.Snapshot.Revision != rev+1 || kcalOf(e.Snapshot) != 520 {
		t.Fatalf("revision %d (was %d) kcal %v", e.Snapshot.Revision, rev, kcalOf(e.Snapshot))
	}
	p := e.Items[0]
	if p.Recalibration.State != "applied" || !p.Recalibration.CanRevert || *p.Recalibration.Deltas["kcal"] != 180 ||
		p.Effective.Kcal.float() != 480 || p.Macros.Kcal.float() != 480 || *p.PortionG != 300 {
		t.Fatalf("item after apply %+v recal %+v", p, p.Recalibration)
	}
	if e.Items[1].Recalibration.State != "agreed" || e.Recalibration.State != "applied" {
		t.Fatalf("salad %+v entry %+v", e.Items[1].Recalibration, e.Recalibration)
	}
	want := "Second opinion: pasta 300 g, not 200 g (plate fills the bowl): +180 kcal, +6 g protein. Applied. The rest holds."
	if c := h.coachLines(); len(c) != 1 || c[0] != want {
		t.Fatalf("coach %q", c)
	}
	// The day and recent lists show the recalibrated values.
	for _, d := range h.dayTyped() {
		if d.Item == "pasta" && (*d.PortionG != 300 || d.Macros.Kcal.float() != 480 || d.Recalibration == nil || d.Recalibration.State != "applied") {
			t.Fatalf("day item %+v", d)
		}
	}
	// The feed's item cards carry the recalibration too.
	fr := h.do("GET", "/fuel/feed", nil, "")
	if !strings.Contains(fr.Body.String(), `"recalibration":{"state":"applied"`) || !strings.Contains(fr.Body.String(), `"role":"coach"`) {
		t.Fatalf("feed %s", fr.Body)
	}
}

func TestRecalThresholds(t *testing.T) {
	m := func(kcal, protein, sat float64) Macros {
		return Macros{Kcal: known(toTenth(kcal)), Protein: known(toTenth(protein)), SatFat: known(toTenth(sat))}
	}
	cases := []struct {
		cur, to Macros
		want    bool
	}{
		{m(300, 10, 2), m(339, 10, 2), false},   // 13 %, 39 kcal
		{m(300, 10, 2), m(361, 10, 2), true},    // more than 20 % and more than 40
		{m(100, 10, 2), m(135, 10, 2), false},   // 35 %, but only 35 kcal
		{m(1000, 10, 2), m(1150, 10, 2), false}, // 150 kcal, but only 15 %
		{m(300, 10, 2), m(300, 15.1, 2), true},
		{m(300, 10, 2), m(300, 15, 2), false},
		{m(300, 10, 2), m(300, 10, 4.1), true},
		{m(300, 10, 2), m(300, 10, 0), false},
		{m(300, 10, 2.4), m(300, 10, 4.4), false}, // exactly 2 g (floats would say 2.0000000000000004)
		{m(300, 10, 2.4), m(300, 10, 4.5), true},
		{m(200, 10, 2), m(240, 10, 2), false}, // exactly 40 kcal and 20 %
		{m(200, 10, 2), m(240.1, 10, 2), true},
		{m(250, 10, 2), m(300, 10, 2), false}, // 50 kcal is exactly 20 %
		{m(300, 10, 2), m(200, 10, 2), true},
	}
	for i, c := range cases {
		if got := recalDiffers(c.cur, c.to); got != c.want {
			t.Fatalf("case %d: %v", i, got)
		}
	}
}

func TestRecalLowConfidenceAndMissedItem(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000003-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	id := resp.Items[0].ItemID
	h.say(answerOf(
		opinion(id, "pasta", 300, 480, 16, 2.4, "low", "blurry"),
		opinion("", "olive oil", 10, 88, 0, 1.4, "medium", "sheen on the pasta")))
	h.run()
	if n := len(h.vars.rows("var-food")); n != 1 {
		t.Fatalf("rows %d: low confidence and missed items must write nothing", n)
	}
	e := h.entry(resp.EntryID)
	if e.Items[0].Recalibration.State != "suggested" || e.Recalibration.State != "suggested" || len(e.Recalibration.Suggestions) != 1 {
		t.Fatalf("recalibration %+v", e.Recalibration)
	}
	if kcalOf(e.Snapshot) != 300 {
		t.Fatalf("kcal %v", kcalOf(e.Snapshot))
	}
	c := h.coachLines()
	if len(c) != 1 || !strings.Contains(c[0], "Low confidence, not applied.") || !strings.Contains(c[0], "Suggests a missed item: olive oil, about 10 g. Add it?") {
		t.Fatalf("coach %q", c)
	}
}

func TestRecalInvalidAnswersWriteNothing(t *testing.T) {
	h := newRecalHarness(t)
	n := 0
	bad := func(name, answer string, mk func(a, b string) string) {
		n++
		resp := h.logPhoto(fmt.Sprintf("r0000004-%04d", n), photoMeal(photoItem("pasta", 200, 300, 10, 2, false), photoItem("salad", 100, 40, 2, 0.2, false)), 1)
		if mk != nil {
			answer = mk(resp.Items[0].ItemID, resp.Items[1].ItemID)
		}
		h.say(answer)
		before := len(h.vars.rows("var-food"))
		if !h.run() {
			t.Fatalf("%s: no job", name)
		}
		if len(h.vars.rows("var-food")) != before {
			t.Fatalf("%s: rows written", name)
		}
		e := h.entry(resp.EntryID)
		if e.Recalibration == nil || e.Recalibration.State != "failed" || !strings.Contains(e.Recalibration.Summary, "invalid answer") || e.Items[0].Recalibration.State != "failed" {
			t.Fatalf("%s: %+v", name, e.Recalibration)
		}
	}
	good := func(id string) string { return opinion(id, "x", 300, 480, 16, 2.4, "high", "r") }
	bad("prose", "The pasta looks like 300 g to me.", nil)
	bad("not json", "{items: nope}", nil)
	bad("unknown item id", "", func(a, b string) string { return answerOf(good(a), good("it_nope")) })
	bad("duplicate match", "", func(a, b string) string { return answerOf(good(a), good(a)) })
	bad("an item without opinion", "", func(a, b string) string { return answerOf(good(a)) })
	bad("extra key", "", func(a, b string) string {
		return answerOf(good(a), strings.Replace(good(b), `"item"`, `"run":"rm -rf","item"`, 1))
	})
	bad("missing key", "", func(a, b string) string {
		return answerOf(good(a), strings.Replace(good(b), `"fat_g":14,`, ``, 1))
	})
	bad("kcal out of bounds", "", func(a, b string) string { return answerOf(good(a), opinion(b, "x", 300, 9000, 16, 2, "high", "r")) })
	bad("negative protein", "", func(a, b string) string { return answerOf(good(a), opinion(b, "x", 300, 400, -3, 2, "high", "r")) })
	bad("bad confidence", "", func(a, b string) string { return answerOf(good(a), opinion(b, "x", 300, 400, 3, 2, "certain", "r")) })
	bad("string number", "", func(a, b string) string {
		return answerOf(good(a), strings.Replace(good(b), `"kcal":480`, `"kcal":"480"`, 1))
	})
	bad("too many", "", func(a, b string) string {
		items := []string{good(a), good(b)}
		for i := 0; i < 11; i++ {
			items = append(items, good(""))
		}
		return answerOf(items...)
	})
	bad("reason null", "", func(a, b string) string {
		return answerOf(good(a), strings.Replace(good(b), `"reason":"r"`, `"reason":null`, 1))
	})
	bad("top-level extra", "", func(a, b string) string {
		return `{"items":[` + good(a) + `,` + good(b) + `],"apply":true}`
	})
	if c := h.coachLines(); len(c) != 0 {
		t.Fatalf("a failed second opinion is silent in the feed: %v", c)
	}
	// The reason text is sanitized and capped.
	resp := h.logPhoto("r0000004-9999", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pasta", 300, 480, 16, 2.4, "high", "line one\nline two\u0007 \u202e\u009b"+strings.Repeat("\u00e9", 600))))
	h.run()
	r := h.entry(resp.EntryID).Items[0].Recalibration
	if r.State != "applied" || strings.ContainsAny(r.Reason, "\n\u0007\u202e\u009b") || len([]rune(r.Reason)) != 240 || !utf8.ValidString(r.Reason) {
		t.Fatalf("reason %q", r.Reason)
	}
}

func TestRecalTimeout(t *testing.T) {
	h := newRecalHarness(t, func(o *Options) { o.Recal.Timeout = 80 * time.Millisecond })
	resp := h.logPhoto("r0000005-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.agent.block = make(chan struct{})
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pasta", 300, 480, 16, 2.4, "high", "r")))
	h.run()
	e := h.entry(resp.EntryID)
	if e.Recalibration.State != "failed" || !strings.Contains(e.Recalibration.Summary, "timeout") {
		t.Fatalf("%+v", e.Recalibration)
	}
	if len(h.vars.rows("var-food")) != 1 {
		t.Fatal("rows written after a timeout")
	}
	h.agent.mu.Lock()
	del := h.agent.deleted
	h.agent.mu.Unlock()
	if del != 1 {
		t.Fatalf("the session of a timed-out job must be deleted: %d", del)
	}
	if h.run() {
		t.Fatal("a failed job must not run again")
	}
}

func TestRecalUserEditsWin(t *testing.T) {
	for _, edit := range []string{"fix", "undo", "fraction", "external"} {
		t.Run(edit, func(t *testing.T) {
			h := newRecalHarness(t)
			resp := h.logPhoto("r0000006-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, edit == "fraction")), 1)
			id := resp.Items[0].ItemID
			h.say(answerOf(opinion(id, "pasta", 300, 480, 16, 2.4, "high", "r")))
			// The user (or another writer) changes the item while the agent works.
			h.agent.hook = func() {
				switch edit {
				case "fix":
					if r := h.fix("r0000006-0002", id, map[string]any{"portion_g": 100}); r.Code != 200 {
						t.Errorf("fix %d %s", r.Code, r.Body)
					}
				case "undo":
					if r := h.mutate("undo", "r0000006-0002", id, nil); r.Code != 200 {
						t.Errorf("undo %d %s", r.Code, r.Body)
					}
				case "fraction":
					if r := h.mutate("fraction", "r0000006-0002", id, f64(1)); r.Code != 200 {
						t.Errorf("fraction %d %s", r.Code, r.Body)
					}
				case "external":
					h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: pasta", "source": "agentd", "corrects": id,
						"reason": "fix", "op_id": "ext-1", "kcal": -150, "protein_g": -5, "carbs_g": -20, "fat_g": -5, "sat_fat_g": -1, "fiber_g": nil})
				}
			}
			h.run()
			if n := len(h.corrRows("recalibrate")); n != 0 {
				t.Fatalf("recalibrated an item that was changed meanwhile (%d rows)", n)
			}
			e := h.entry(resp.EntryID)
			if e.Items[0].Recalibration.State != "skipped" || e.Items[0].Recalibration.CanRevert {
				t.Fatalf("%+v", e.Items[0].Recalibration)
			}
			if c := h.coachLines(); len(c) != 1 || !strings.Contains(c[0], "was not applied: the item changed meanwhile") {
				t.Fatalf("coach %q", c)
			}
		})
	}
}

func TestRecalRestartMidJobAndAfterTheRow(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000007-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	id := resp.Items[0].ItemID
	h.say(answerOf(opinion(id, "pasta", 300, 480, 16, 2.4, "high", "r")))
	h.agent.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.svc.recalRunOnce(ctx); close(done) }()
	for { // until the job is journaled as running
		if j, ok := h.svc.recal.get(resp.EntryID); ok && j.State == RecalRunning {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel() // the process stops mid-job
	<-done
	if j, _ := h.svc.recal.get(resp.EntryID); j.State != RecalRunning {
		t.Fatalf("an interrupted job must stay running in the store: %s", j.State)
	}
	h.agent.mu.Lock()
	h.agent.block = nil
	h.agent.mu.Unlock()
	h.restart()
	if j, _ := h.svc.recal.get(resp.EntryID); j.State != RecalQueued || j.Attempts != 1 {
		t.Fatalf("after restart: %+v", j)
	}
	h.run()
	if n := len(h.corrRows("recalibrate")); n != 1 {
		t.Fatalf("rows %d", n)
	}
	// A crash AFTER the row was journaled but before the job result: the
	// job runs again and must adopt the row, never write a second one.
	j, _ := h.svc.recal.get(resp.EntryID)
	j.State, j.Items, j.Attempts = RecalRunning, nil, 1
	if err := h.svc.recal.put(j); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.run()
	if n := len(h.corrRows("recalibrate")); n != 1 {
		t.Fatalf("a re-run wrote a second recalibration: %d rows", n)
	}
	e := h.entry(resp.EntryID)
	if r := e.Items[0].Recalibration; r.State != "applied" || !r.CanRevert || *r.Deltas["kcal"] != 180 || kcalOf(e.Snapshot) != 480 {
		t.Fatalf("%+v kcal %v", r, kcalOf(e.Snapshot))
	}
	if c := h.coachLines(); len(c) != 1 {
		t.Fatalf("coach lines %q", c)
	}
	// Interrupted twice: failed, never a third run.
	j.State, j.Attempts = RecalRunning, 2
	_ = h.svc.recal.put(j)
	h.restart()
	if j, _ := h.svc.recal.get(resp.EntryID); j.State != RecalFailed {
		t.Fatalf("%+v", j)
	}
}

func TestRecalRevert(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000008-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false), photoItem("salad", 100, 40, 2, 0.2, false)), 1)
	pasta, salad := resp.Items[0].ItemID, resp.Items[1].ItemID
	if r := h.revert("r0000008-0002", pasta); r.Code != 409 {
		t.Fatalf("revert before the second opinion: %d %s", r.Code, r.Body)
	}
	h.say(answerOf(opinion(pasta, "pasta", 300, 480, 16, 2.4, "high", "r"), opinion(salad, "salad", 100, 40, 2, 0.2, "high", "ok")))
	h.run()
	if r := h.revert("r0000008-0003", salad); r.Code != 409 || errCode(t, r) != "not_recalibrated" {
		t.Fatalf("revert of an agreed item: %d %s", r.Code, r.Body)
	}
	r := h.revert("r0000008-0004", pasta)
	if r.Code != 200 {
		t.Fatalf("revert %d %s", r.Code, r.Body)
	}
	m := decode[MutationResponse](t, r)
	if kcalOf(m.Snapshot) != 340 || m.Item.Effective.Kcal.float() != 300 || *m.Item.PortionG != 200 || m.Item.Macros.Kcal.float() != 300 ||
		m.Item.Recalibration.State != "reverted" || m.Item.Recalibration.CanRevert || !strings.HasPrefix(m.Blocks[0].Text, "Reverted the second opinion on pasta.") {
		t.Fatalf("after revert %+v %+v %q", m.Item, m.Item.Recalibration, m.Blocks[0].Text)
	}
	rows := h.corrRows("recalibrate_revert")
	if len(rows) != 1 || rows[0]["kcal"] != -180.0 || rows[0]["portion_g_after"] != 200.0 || rows[0]["corrects"] != pasta {
		t.Fatalf("revert rows %v", rows)
	}
	// Idempotent replay; a new request is refused; one row in total.
	if r2 := h.revert("r0000008-0004", pasta); r2.Code != 200 || r2.Body.String() != r.Body.String() {
		t.Fatalf("replay %d", r2.Code)
	}
	if r3 := h.revert("r0000008-0005", pasta); r3.Code != 409 || errCode(t, r3) != "already_reverted" {
		t.Fatalf("second revert %d %s", r3.Code, r3.Body)
	}
	if len(h.corrRows("recalibrate_revert")) != 1 {
		t.Fatal("more than one revert row")
	}
	for _, d := range h.dayTyped() {
		if d.Item == "pasta" && (*d.PortionG != 200 || d.Macros.Kcal.float() != 300) {
			t.Fatalf("day item after revert %+v", d)
		}
	}
	// After a revert the ORIGINAL is the base again.
	fx := decode[MutationResponse](t, h.fix("r0000008-0006", pasta, map[string]any{"portion_g": 100}))
	if fx.Item.Effective.Kcal.float() != 150 {
		t.Fatalf("fix after revert %v", fx.Item.Effective.Kcal.float())
	}
	// Survives a restart.
	h.restart()
	if e := h.entry(resp.EntryID); e.Items[0].Recalibration.State != "reverted" || kcalOf(e.Snapshot) != 190 {
		t.Fatalf("after restart %+v kcal %v", e.Items[0].Recalibration, kcalOf(e.Snapshot))
	}
}

func TestRecalRevertRefusedAfterALaterChange(t *testing.T) {
	for _, edit := range []string{"fix", "undo", "external", "fraction1"} {
		t.Run(edit, func(t *testing.T) {
			h := newRecalHarness(t)
			resp := h.logPhoto("r0000009-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, true)), 1)
			id := resp.Items[0].ItemID
			h.say(answerOf(opinion(id, "pasta", 300, 480, 16, 2.4, "high", "r")))
			h.run()
			want := "changed_after"
			switch edit {
			case "fix":
				h.fix("r0000009-0002", id, map[string]any{"portion_g": 150})
			case "fraction1":
				// "I ate all of it": a choice without a row is an edit too.
				if r := h.mutate("fraction", "r0000009-0002", id, f64(1)); r.Code != 200 {
					t.Fatalf("fraction %d", r.Code)
				}
			case "undo":
				h.mutate("undo", "r0000009-0002", id, nil)
				want = "already_undone"
			case "external":
				h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: pasta", "source": "agentd", "corrects": id,
					"reason": "fix", "op_id": "ext-1", "kcal": -100, "protein_g": -3, "carbs_g": -10, "fat_g": -2, "sat_fat_g": -0.5, "fiber_g": nil})
			}
			r := h.revert("r0000009-0003", id)
			if r.Code != 409 || errCode(t, r) != want {
				t.Fatalf("%d %s", r.Code, r.Body)
			}
			if len(h.corrRows("recalibrate_revert")) != 0 {
				t.Fatal("a refused revert wrote a row")
			}
			if e := h.entry(resp.EntryID); e.Items[0].Recalibration.CanRevert {
				t.Fatalf("can_revert after %s", edit)
			}
		})
	}
}

func TestFixFractionUndoAfterRecalibrateUseTheNewBase(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000010-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false), photoItem("pizza", 400, 800, 30, 10, true)), 1)
	pasta, pizza := resp.Items[0].ItemID, resp.Items[1].ItemID
	h.say(answerOf(opinion(pasta, "pasta", 300, 480, 16, 2.4, "high", "r"), opinion(pizza, "pizza", 500, 1000, 40, 12, "high", "r")))
	h.run()
	// Grams against the recalibrated 300 g / 480 kcal: 150 g = 240 kcal.
	fx := decode[MutationResponse](t, h.fix("r0000010-0002", pasta, map[string]any{"portion_g": 150}))
	if fx.Item.Effective.Kcal.float() != 240 || fx.Item.Effective.Protein.float() != 8 {
		t.Fatalf("fix after recalibrate: %v kcal %v protein", fx.Item.Effective.Kcal.float(), fx.Item.Effective.Protein.float())
	}
	h.clk.Add(time.Second)
	// A share is a share of the recalibrated base: 2 x = 960 kcal, 600 g.
	fx = decode[MutationResponse](t, h.fix("r0000010-0003", pasta, map[string]any{"share": 2}))
	if fx.Item.Effective.Kcal.float() != 960 {
		t.Fatalf("share after recalibrate: %v", fx.Item.Effective.Kcal.float())
	}
	for _, d := range h.dayTyped() {
		if d.Item == "pasta" && *d.PortionG != 600 {
			t.Fatalf("portion after share 2: %v", *d.PortionG)
		}
	}
	h.clk.Add(time.Second)
	// The eaten fraction of the recalibrated pizza: half of 1000.
	fr := decode[MutationResponse](t, h.mutate("fraction", "r0000010-0004", pizza, f64(0.5)))
	if fr.Item.Effective.Kcal.float() != 500 || fr.Item.Macros.Kcal.float() != 1000 {
		t.Fatalf("fraction after recalibrate: %v of %v", fr.Item.Effective.Kcal.float(), fr.Item.Macros.Kcal.float())
	}
	for _, d := range h.dayTyped() {
		if d.Item == "pizza" && *d.PortionG != 250 {
			t.Fatalf("pizza portion %v", *d.PortionG)
		}
	}
	// Undo negates the CURRENT contribution: the day ends without the pizza.
	un := decode[MutationResponse](t, h.mutate("undo", "r0000010-0005", pizza, nil))
	if kcalOf(un.Snapshot) != 960 || un.Item.Effective.Kcal.float() != 0 {
		t.Fatalf("undo after recalibrate: day %v item %v", kcalOf(un.Snapshot), un.Item.Effective.Kcal.float())
	}
}

func TestRelogOfARecalibratedItem(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000011-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pasta", 300, 480, 16, 2.4, "high", "r")))
	h.run()
	rec := h.recent("")
	if len(rec.Items) != 1 || *rec.Items[0].PortionG != 300 || rec.Items[0].Macros.Kcal.float() != 480 {
		t.Fatalf("recent %+v", rec.Items)
	}
	h.clk.Add(time.Minute)
	r := h.relog("r0000011-0002", rec.Items[0].Key, nil)
	if r.Code != 200 {
		t.Fatalf("relog %d %s", r.Code, r.Body)
	}
	lr := decode[LogResponse](t, r)
	if lr.Items[0].Macros.Kcal.float() != 480 || *lr.Items[0].PortionG != 300 || lr.Items[0].Recalibration != nil || kcalOf(lr.Snapshot) != 960 {
		t.Fatalf("relog item %+v kcal %v", lr.Items[0], kcalOf(lr.Snapshot))
	}
	// A relog has no photo of its own: no second job.
	if h.run() {
		t.Fatal("a relog was recalibrated")
	}
}

func TestRecalKillSwitchIntervalAndScope(t *testing.T) {
	kill := filepath.Join(t.TempDir(), "recalibrate.off")
	h := newRecalHarness(t, func(o *Options) { o.Recal.KillFile = kill; o.Recal.MinInterval = time.Minute })
	// A text log gets no job.
	h.model.fn = func(ModelInput) string { return skyrWalnuts }
	h.logText("r0000012-0001", "skyr and walnuts")
	if h.run() {
		t.Fatal("a text log was recalibrated")
	}
	a := h.logPhoto("r0000012-0002", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.clk.Add(time.Second)
	b := h.logPhoto("r0000012-0003", photoMeal(photoItem("rice", 200, 260, 5, 0.2, false)), 1)
	h.agent.answer = func(task string) string {
		id := a.Items[0].ItemID
		if strings.Contains(task, b.Items[0].ItemID) {
			id = b.Items[0].ItemID
		}
		return answerOf(opinion(id, "x", 200, 300, 10, 2, "high", "ok"))
	}
	if err := os.WriteFile(kill, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if h.run() || h.agent.sessions != 0 {
		t.Fatal("a job ran with the kill file present")
	}
	if e := h.entry(a.EntryID); e.Recalibration.State != "pending" {
		t.Fatalf("%+v", e.Recalibration)
	}
	_ = os.Remove(kill)
	if !h.run() {
		t.Fatal("no job after the kill file was removed")
	}
	// Oldest first, and not more often than the minimum interval.
	if e := h.entry(a.EntryID); e.Recalibration.State != "agreed" {
		t.Fatalf("first job %+v", e.Recalibration)
	}
	if h.run() {
		t.Fatal("the second job ran inside the minimum interval")
	}
	h.clk.Add(61 * time.Second)
	if !h.run() || h.entry(b.EntryID).Recalibration.State != "agreed" {
		t.Fatal("the second job did not run after the interval")
	}
}

func TestRecalImplausibleSwingIsOnlySuggested(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000016-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false), photoItem("rice", 150, 200, 4, 0.2, false)), 1)
	a, b := resp.Items[0].ItemID, resp.Items[1].ItemID
	// 300 -> 950 kcal (more than 3 x) and 200 -> 60 kcal (less than a third).
	h.say(answerOf(opinion(a, "pasta", 600, 950, 30, 3, "high", "huge"), opinion(b, "rice", 40, 60, 1, 0.1, "high", "tiny")))
	h.run()
	if n := len(h.vars.rows("var-food")); n != 2 {
		t.Fatalf("rows %d: a swing beyond 3 x must not be applied", n)
	}
	e := h.entry(resp.EntryID)
	for _, it := range e.Items {
		if it.Recalibration.State != "suggested" || !strings.Contains(it.Recalibration.Summary, "Too far from the first estimate, not applied.") {
			t.Fatalf("%+v", it.Recalibration)
		}
	}
}

func TestRecalQueueSkipsPendingAndExpires(t *testing.T) {
	h := newRecalHarness(t)
	old := h.logPhoto("r0000017-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.clk.Add(25 * time.Hour) // for example the kill switch was on for a day
	fresh := h.logPhoto("r0000017-0002", photoMeal(photoItem("rice", 150, 200, 4, 0.2, false)), 1)
	h.say(answerOf(opinion(fresh.Items[0].ItemID, "rice", 150, 200, 4, 0.2, "high", "ok")))
	if !h.run() {
		t.Fatal("the fresh job must run")
	}
	if j, _ := h.svc.recal.get(old.EntryID); j.State != RecalFailed || j.Error != "expired" {
		t.Fatalf("old job %+v", j)
	}
	if j, _ := h.svc.recal.get(fresh.EntryID); j.State != RecalDone || h.agent.sessions != 1 {
		t.Fatalf("fresh job %+v sessions %d", j, h.agent.sessions)
	}
}

func TestRecalDisabledWritesNoJob(t *testing.T) {
	h := newRecalHarness(t, func(o *Options) { o.Recal.Enabled = false })
	resp := h.logPhoto("r0000013-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	if resp.Items[0].Recalibration != nil || h.run() || h.agent.sessions != 0 {
		t.Fatal("recalibration ran while disabled")
	}
	if e := h.entry(resp.EntryID); e.Recalibration != nil {
		t.Fatalf("%+v", e.Recalibration)
	}
	// Reverting on a service without recalibration is a clean 409.
	if r := h.revert("r0000013-0002", resp.Items[0].ItemID); r.Code != 409 {
		t.Fatalf("%d", r.Code)
	}
}

func TestRecalWaitsForThePendingEntryAndFailsWithIt(t *testing.T) {
	h := newRecalHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) { return true, 500 } // stored, answer lost
	h.model.fn = func(ModelInput) string { return photoMeal(photoItem("pasta", 200, 300, 10, 2, false)) }
	body, ct := multipartBody(t, map[string]string{"client_id": "r0000014-0001"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(300, 200)}})
	r := h.do("POST", "/fuel/log", body, ct)
	if r.Code != 202 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if h.run() || h.agent.sessions != 0 {
		t.Fatal("the job ran before the fast path's rows were settled")
	}
	h.vars.onPost = nil
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	resp := decode[LogResponse](t, r)
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pasta", 300, 480, 16, 2.4, "high", "r")))
	if !h.run() || len(h.corrRows("recalibrate")) != 1 {
		t.Fatal("the job did not run once the entry was done")
	}
}

func TestRecalNullFieldStaysNull(t *testing.T) {
	h := newRecalHarness(t)
	// The logged row has fiber null (pizzaPhoto) and no portion.
	resp := h.logPhoto("r0000015-0001", pizzaPhoto, 1)
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pizza", 420, 1100, 40, 15, "high", "r")))
	h.run()
	rows := h.corrRows("recalibrate")
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	if v, ok := rows[0]["fiber_g"]; !ok || v != nil {
		t.Fatalf("fiber_g of the correction must stay null: %v", v)
	}
	e := h.entry(resp.EntryID)
	if e.Items[0].Effective.Fiber.OK || e.Items[0].Effective.Kcal.float() != 1100 || *e.Items[0].PortionG != 420 {
		t.Fatalf("%+v", e.Items[0])
	}
}

func TestAgentdClientErrors(t *testing.T) {
	fa := &fakeAgentd{answer: func(string) string { return "x" }}
	srv := httptest.NewServer(fa)
	defer srv.Close()
	c := &AgentdClient{Base: srv.URL, Token: "wrong", Client: loopbackClient()}
	if _, _, err := c.Ask(context.Background(), "t", [][]byte{{1}}); err == nil || fa.badAuth != 1 {
		t.Fatalf("a 401 must be an error: %v", err)
	}
	c.Token = "agent-token"
	if _, _, err := c.Ask(context.Background(), "t", nil); err == nil {
		t.Fatal("no photos must be an error")
	}
	ans, tag, err := c.Ask(context.Background(), "task", [][]byte{{1}, {2}, {3}})
	if err != nil || ans != "x" || tag != "agentd:fake" || fa.sessions != 1 || fa.deleted != 1 || len(fa.texts) != 3 {
		t.Fatalf("%q %q %v %+v", ans, tag, err, fa.texts)
	}
}

func TestRecalConfigKeys(t *testing.T) {
	c, err := ParseDaemonConfig([]byte("recalibrate_enabled = true\nrecalibrate_agentd_url = \"http://127.0.0.1:8798\"\nrecalibrate_agentd_token = \"env:X\"\nrecalibrate_timeout = \"5m\"\nrecalibrate_min_interval = \"1m\"\nrecalibrate_kill_file = \"/tmp/off\"\n"))
	if err != nil || !c.RecalibrateEnabled || c.RecalibrateTimeout != "5m" || c.RecalibrateKillFile != "/tmp/off" {
		t.Fatalf("%+v %v", c, err)
	}
	if d := DefaultDaemonConfig(); d.RecalibrateEnabled || d.RecalibrateTimeout != "10m" {
		t.Fatalf("defaults %+v", d)
	}
	for _, bad := range []string{
		"recalibrate_enabled = true\n", // no token
		"recalibrate_enabled = true\nrecalibrate_agentd_token = \"t\"\nrecalibrate_agentd_url = \"ftp://x\"\n",
		"recalibrate_timeout = \"soon\"\n",
		"recalibrate_min_interval = \"-1s\"\n",
		"recalibrate_enabled = \"yes\"\n",
	} {
		if _, err := ParseDaemonConfig([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestRecalKeepsKnownIntakeAndFibre(t *testing.T) {
	h := newRecalHarness(t)
	// A photographed drink: volume and alcohol are known and stay known.
	beer := `{"item":"beer","kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"photo_estimate","kcal":140,"protein_g":1,"carbs_g":12,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":330,"caffeine_mg":null,"alcohol_g":13}`
	resp := h.logPhoto("r0000018-0001", photoMeal(beer, photoItem("pasta", 200, 300, 10, 2, false)), 1)
	b, p := resp.Items[0].ItemID, resp.Items[1].ItemID
	noFibre := strings.Replace(opinion(p, "pasta", 300, 480, 16, 2.4, "high", "r"), `"fiber_g":4`, `"fiber_g":null`, 1)
	h.say(answerOf(opinion(b, "beer", 500, 215, 2, 0, "high", "a half litre glass"), noFibre))
	h.run()
	if n := len(h.corrRows("recalibrate")); n != 2 {
		t.Fatalf("rows %d", n)
	}
	e := h.entry(resp.EntryID)
	eb, ep := e.Items[0].Effective, e.Items[1].Effective
	if !eb.VolumeML.OK || eb.VolumeML.float() != 330 || !eb.AlcoholG.OK || eb.AlcoholG.float() != 13 || eb.Kcal.float() != 215 {
		t.Fatalf("beer after recalibrate %+v", eb)
	}
	if !ep.Fiber.OK || ep.Fiber.float() != 3 || ep.Kcal.float() != 480 {
		t.Fatalf("pasta fibre must stay the known 3 g: %+v", ep)
	}
	for _, in := range e.Snapshot.Intake {
		if in.Key == "alcohol_g" && (in.Consumed != 13 || in.UnknownRows != 0) {
			t.Fatalf("alcohol intake %+v", in)
		}
	}
	// And after a revert.
	m := decode[MutationResponse](t, h.revert("r0000018-0002", b))
	if !m.Item.Effective.VolumeML.OK || m.Item.Effective.AlcoholG.float() != 13 || m.Item.Effective.Kcal.float() != 140 {
		t.Fatalf("beer after revert %+v", m.Item.Effective)
	}
}

func TestRecalRereadsEachItemUnderItsLock(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000019-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false), photoItem("rice", 150, 200, 4, 0.2, false)), 1)
	a, b := resp.Items[0].ItemID, resp.Items[1].ItemID
	h.say(answerOf(opinion(a, "pasta", 300, 480, 16, 2.4, "high", "r"), opinion(b, "rice", 300, 400, 8, 0.4, "high", "r")))
	// While the first item's row is posted, another writer corrects the second.
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["reason"] == "recalibrate" && data["corrects"] == a {
			go h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: rice", "source": "agentd", "corrects": b,
				"reason": "fix", "op_id": "ext-9", "kcal": -100, "protein_g": -2, "carbs_g": -20, "fat_g": -5, "sat_fat_g": -0.1, "fiber_g": nil})
			time.Sleep(20 * time.Millisecond)
		}
		return true, 201
	}
	h.run()
	rows := h.corrRows("recalibrate")
	if len(rows) != 1 || rows[0]["corrects"] != a {
		t.Fatalf("the second item was recalibrated over another writer's correction: %v", rows)
	}
	if e := h.entry(resp.EntryID); e.Items[1].Recalibration.State != "skipped" {
		t.Fatalf("%+v", e.Items[1].Recalibration)
	}
}

func TestRecalRejectedWriteIsNotAnnouncedAsApplied(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000020-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pasta", 300, 480, 16, 2.4, "high", "r")))
	h.vars.onPost = func(n int, data map[string]any) (bool, int) { return false, 400 }
	h.run()
	h.vars.onPost = nil
	e := h.entry(resp.EntryID)
	r := e.Items[0].Recalibration
	if r.State != "failed" || r.CanRevert || e.Recalibration.State != "failed" || kcalOf(e.Snapshot) != 300 || e.Items[0].Macros.Kcal.float() != 300 {
		t.Fatalf("item %+v entry %+v kcal %v", r, e.Recalibration, kcalOf(e.Snapshot))
	}
	c := h.coachLines()
	if len(c) != 1 || strings.Contains(c[0], "Applied.") || !strings.Contains(c[0], "Could not be saved, not applied.") {
		t.Fatalf("coach %q", c)
	}
	if rr := h.revert("r0000020-0002", resp.Items[0].ItemID); rr.Code != 409 {
		t.Fatalf("revert of a rejected recalibration: %d", rr.Code)
	}
}

func TestRecalCrashAfterTheRowThenAgentFails(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000021-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	id := resp.Items[0].ItemID
	h.say(answerOf(opinion(id, "pasta", 300, 480, 16, 2.4, "high", "r")))
	h.run()
	// The job result is lost (crash); the user then fixes the item; the
	// second run's agent answer is garbage.
	j, _ := h.svc.recal.get(resp.EntryID)
	j.State, j.Items, j.Attempts = RecalRunning, nil, 2 // interrupted twice: failed at startup, no agent call
	_ = h.svc.recal.put(j)
	if err := os.Truncate(filepath.Join(h.opts.StateDir, "feed.jsonl"), 0); err != nil { // the line was lost too
		t.Fatal(err)
	}
	h.clk.Add(time.Second)
	h.fix("r0000021-0002", id, map[string]any{"portion_g": 150})
	h.say("no JSON today")
	h.restart()
	h.run()
	if j, _ := h.svc.recal.get(resp.EntryID); j.State != RecalFailed {
		t.Fatalf("%+v", j)
	}
	e := h.entry(resp.EntryID)
	r := e.Items[0].Recalibration
	if r.State != "applied" || r.CanRevert || *r.Deltas["kcal"] != 180 || r.To.Kcal != 480 || e.Recalibration.State != "applied" {
		t.Fatalf("the journaled recalibration must stay visible: %+v entry %+v", r, e.Recalibration)
	}
	// A written change is never silent, also when the job failed.
	if c := h.coachLines(); len(c) != 1 || !strings.HasSuffix(c[0], "Applied.") {
		t.Fatalf("coach %q", c)
	}
	if e.Items[0].Effective.Kcal.float() != 240 || len(h.corrRows("recalibrate")) != 1 {
		t.Fatalf("effective %v", e.Items[0].Effective.Kcal.float())
	}
}

func TestRecalExpiresBehindTheKillFile(t *testing.T) {
	kill := filepath.Join(t.TempDir(), "off")
	h := newRecalHarness(t, func(o *Options) { o.Recal.KillFile = kill })
	resp := h.logPhoto("r0000022-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	_ = os.WriteFile(kill, nil, 0o600)
	h.clk.Add(25 * time.Hour)
	if h.run() {
		t.Fatal("a job ran behind the kill file")
	}
	if j, _ := h.svc.recal.get(resp.EntryID); j.State != RecalFailed || j.Error != "expired" {
		t.Fatalf("%+v", j)
	}
}

func TestChatGramsCorrectionUsesTheRecalibratedPortion(t *testing.T) {
	h := newRecalHarness(t)
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return oneItem("Oats", 60, 220, 8, false) }
	h.logText("r0000023-0001", "oats")
	h.clk.Add(time.Minute)
	// The pizza is logged without a portion; the second opinion gives 420 g.
	resp := h.logPhoto("r0000023-0002", pizzaPhoto, 1)
	pizza := resp.Items[0].ItemID
	h.say(answerOf(opinion(pizza, "pizza", 420, 1100, 40, 15, "high", "r")))
	h.run()
	var seen []LastItem
	h.model.mu.Lock()
	h.model.fn = func(in ModelInput) string {
		seen = in.LastItems
		return correctOut(pizza, "210", "null", "null")
	}
	h.model.mu.Unlock()
	h.clk.Add(time.Minute)
	r := h.logText("r0000023-0003", "the pizza was only 210 g")
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	e := h.entry(resp.EntryID)
	if e.Items[0].Effective.Kcal.float() != 550 {
		t.Fatalf("pizza %v kcal (the correction went elsewhere: %s)", e.Items[0].Effective.Kcal.float(), r.Body)
	}
	ok := false
	for _, li := range seen {
		if li.ItemID == pizza {
			ok = li.PortionG != nil && *li.PortionG == 420 && len(li.Units) == 1 && li.Units[0] == "g"
		}
	}
	if !ok {
		t.Fatalf("LAST LOGGED ITEMS must show the recalibrated portion: %+v", seen)
	}
}

func TestRecalCoachLineWaitsForAnUncertainWrite(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("r0000024-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	h.say(answerOf(opinion(resp.Items[0].ItemID, "pasta", 300, 480, 16, 2.4, "high", "r")))
	h.vars.onPost = func(n int, data map[string]any) (bool, int) { return true, 500 } // stored, answer lost
	h.run()
	h.vars.onPost = nil
	if c := h.coachLines(); len(c) != 0 {
		t.Fatalf("a coach line before the write settled: %q", c)
	}
	if e := h.entry(resp.EntryID); e.Items[0].Recalibration.State != "pending" || e.Items[0].Recalibration.CanRevert || e.Recalibration.State != "pending" {
		t.Fatalf("%+v entry %+v", e.Items[0].Recalibration, e.Recalibration)
	}
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background()) // must not block on the worker
	h.run()
	c := h.coachLines()
	if len(c) != 1 || !strings.HasSuffix(c[0], "Applied.") {
		t.Fatalf("coach %q", c)
	}
	if e := h.entry(resp.EntryID); e.Items[0].Recalibration.State != "applied" || kcalOf(e.Snapshot) != 480 || len(h.corrRows("recalibrate")) != 1 {
		t.Fatalf("%+v", e.Items[0].Recalibration)
	}
}
