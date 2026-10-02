package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// T24 (spec section 22): the agent chat broker against a fake agentd. The
// fake agent "runs" a turn by calling a script with the turn message; the
// script writes through fueld's turn-bound routes like fuel-op does.

const opTok = "agent-op-token-0123456789"

type fakeChatAgentd struct {
	t         *testing.T
	mu        sync.Mutex
	srv       *httptest.Server
	sessions  map[string]bool
	creates   []map[string]string
	turns     []string // the turn messages that RAN
	media     []string // the texts of the media turns that ran
	files     int
	tried     int
	deletes   int
	interrupt int
	seq       int
	delay     time.Duration
	// script answers a turn message: the status and the result text.
	script func(msg string) (int, string)
}

var (
	capRE  = regexp.MustCompile(`Capability of this turn, for every write: (\S+)`)
	markRE = regexp.MustCompile(`FUEL-END (\S+)`)
)

func capOf(msg string) string  { return capRE.FindStringSubmatch(msg)[1] }
func markOf(msg string) string { return markRE.FindStringSubmatch(msg)[1] }

// end is an answer with the end line of the turn.
func end(msg, text string) string { return text + "\nFUEL-END " + markOf(msg) }

func newChatAgentd(t *testing.T) *fakeChatAgentd {
	f := &fakeChatAgentd{t: t, sessions: map[string]bool{}}
	f.script = func(msg string) (int, string) { return 200, end(msg, "Hello from the agent.") }
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+agentTok {
			http.Error(w, "unauthorized", 401)
			return
		}
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")[0]
		switch {
		case r.Method == "DELETE":
			f.mu.Lock()
			f.deletes++
			f.mu.Unlock()
		case r.Method == "POST" && r.URL.Path == "/sessions":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.seq++
			sid := fmt.Sprintf("sess-%d", f.seq)
			f.sessions[sid] = true
			f.creates = append(f.creates, body)
			f.mu.Unlock()
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": sid, "harness": "claude-code", "workspace": body["workspace"]})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/interrupt"):
			f.mu.Lock()
			f.interrupt++
			f.mu.Unlock()
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/media"):
			f.mu.Lock()
			ok := f.sessions[id]
			f.mu.Unlock()
			if !ok {
				http.Error(w, "session not found", 404)
				return
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, "no file", 400)
				return
			}
			b, _ := io.ReadAll(file)
			f.mu.Lock()
			f.files++
			n := f.files
			f.media = append(f.media, r.FormValue("text"))
			f.mu.Unlock()
			if len(b) == 0 {
				http.Error(w, "empty", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "routed", "id": id, "result": "FUEL-READY",
				"artifact": map[string]string{"path": fmt.Sprintf("/home/agentd/media/web/photo-%d.jpg", n)}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/input"):
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.tried++
			ok := f.sessions[id]
			d, script := f.delay, f.script
			if ok {
				f.turns = append(f.turns, body.Text)
			}
			f.mu.Unlock()
			if !ok {
				http.Error(w, "session not found", 404)
				return
			}
			if d > 0 {
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					return
				}
			}
			code, res := script(body.Text)
			if code != 200 {
				http.Error(w, "boom", code)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "sent", "id": id, "result": res})
		default:
			http.Error(w, "no", 404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChatAgentd) counts() (creates, turns, tried int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), len(f.turns), f.tried
}

func (f *fakeChatAgentd) lastTurn() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) == 0 {
		return ""
	}
	return f.turns[len(f.turns)-1]
}

// newChat is a harness with chat_backend "agent" and a fake agentd. The
// estimator model fails the test when it is called.
func newChat(t *testing.T, mut ...func(*Options)) (*harness, *fakeChatAgentd) {
	t.Helper()
	fa := newChatAgentd(t)
	ms := append([]func(*Options){func(o *Options) {
		o.Chat = ChatOptions{Backend: "agent", Workspace: "fuel-e2e", Model: "claude-opus-5-5", OpToken: opTok,
			Agent: &AgentdClient{Base: fa.srv.URL, Token: agentTok, Client: loopbackClient()}}
	}}, mut...)
	h := newHarness(t, ms...)
	h.model.fn = func(ModelInput) string {
		t.Error("the estimator model was called on the agent backend")
		return modelQuestion
	}
	return h, fa
}

// op is a fuel-op style call: the agent token, and the capability when set.
func (h *harness) op(method, path, capability string, body any) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+opTok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if capability != "" {
		req.Header.Set(turnHeader, capability)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func food(name string, g, kcal, protein, sat float64) map[string]any {
	return map[string]any{"item": name, "portion_g": g, "kcal": kcal, "protein_g": protein, "carbs_g": 2, "fat_g": 5, "sat_fat_g": sat, "fiber_g": 0}
}

var opSeq int

func opID() string { opSeq++; return fmt.Sprintf("0b000000-%04d", opSeq) }

// logsItems is a script that logs the items and answers text.
func logsItems(h *harness, text string, items ...map[string]any) func(string) (int, string) {
	return func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "items": items})
		if rec.Code != 200 {
			h.t.Errorf("items in the turn: %d %s", rec.Code, rec.Body)
		}
		return 200, end(msg, text)
	}
}

func chatEntries(h *harness) []Entry {
	var out []Entry
	for _, e := range h.svc.journal.Entries() {
		if e.Chat == chatAgent {
			out = append(out, e)
		}
	}
	return out
}

func widgetOf(blocks []Block) (Block, bool) {
	for _, b := range blocks {
		if b.Type == "widget" && b.Widget == "macros_today" {
			return b, true
		}
	}
	return Block{}, false
}

// A text log: the agent answers and writes through the turn-bound route. One
// entry, one user line, one reply line; the agent's text leads; the widget
// has what the turn added; no status line; no model call.
func TestChatTextLog(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged 3 scrambled eggs, about 200 g.", food("scrambled eggs", 200, 300, 20, 6))
	rec := h.logText(cid(), "3 scrambled eggs")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decode[LogResponse](t, rec)
	if r.Status != StatusDone || r.Intent != "log" || len(r.Items) != 1 || r.Items[0].Item != "scrambled eggs" {
		t.Fatalf("response %+v", r)
	}
	tb := textBlocks(r.Blocks)
	if len(tb) != 1 || tb[0] != "Logged 3 scrambled eggs, about 200 g." {
		t.Fatalf("text blocks: %q", tb)
	}
	wb, ok := widgetOf(r.Blocks)
	if !ok || len(r.Blocks) != 2 {
		t.Fatalf("blocks: %+v", r.Blocks)
	}
	if added := fmt.Sprint(wb.Data); !strings.Contains(added, "protein_g:0x") && !strings.Contains(added, "protein_g") {
		t.Errorf("widget data: %v", wb.Data)
	}
	rows := h.vars.rows("var-food")
	if len(rows) != 1 || rows[0]["entry_id"] != r.EntryID || rows[0]["source"] != "fuel" || rows[0]["_record_date"] != "2026-10-01" {
		t.Fatalf("rows %v", rows)
	}
	if es := chatEntries(h); len(es) != 1 || len(h.svc.journal.Entries()) != 1 || es[0].Agent != agentDone || len(es[0].ItemIDs) != 1 {
		t.Fatalf("entries: %+v", h.svc.journal.Entries())
	}
	feed := h.get("/fuel/feed").Body.String()
	if strings.Count(feed, `"text":"3 scrambled eggs"`) != 1 || strings.Count(feed, "Logged 3 scrambled eggs, about 200 g.") != 1 {
		t.Errorf("feed: %s", feed)
	}
	if strings.Contains(feed, "Protein 20 of") {
		t.Error("a status line is in the feed")
	}
	// The session and the message.
	fa.mu.Lock()
	c := fa.creates[0]
	fa.mu.Unlock()
	if c["workspace"] != "fuel-e2e" || c["model"] != "claude-opus-5-5" || !strings.HasPrefix(c["title"], "fuel-chat-e2e-") || strings.Contains(c["title"], ":") {
		t.Errorf("session create: %v", c)
	}
	msg := fa.lastTurn()
	for _, want := range []string{"FUEL TURN", "Capability of this turn", "FUEL-END ", "DAY STATE", "BUDGETS", "<<<\n3 scrambled eggs\n>>>", "Log date of this turn: 2026-10-01"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q", want)
		}
	}
	if !strings.HasPrefix(capOf(msg), "e_") {
		t.Errorf("capability prefix: %s", capOf(msg))
	}
	for _, secret := range []string{opTok, agentTok, testToken, "vkey"} {
		if strings.Contains(msg, secret) {
			t.Errorf("a token is in the message")
		}
	}
	if fa.deletes != 0 {
		t.Error("the broker sent a DELETE")
	}
	// A repeat of the client_id replays the stored answer and asks nobody.
	// The entry poll has the same blocks.
	e := decode[entryWire](t, h.get("/fuel/entry/"+r.EntryID))
	if e.Status != StatusDone || len(e.Items) != 1 {
		t.Errorf("entry: %+v", e)
	}
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Errorf("turns %d", turns)
	}
}

// The second turn has the first one's item in its day state, with the id.
func TestChatDayStateHasItemsAndChat(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged the skyr.", food("skyr", 250, 160, 27, 0.3))
	r := decode[LogResponse](t, h.logText(cid(), "250 g skyr"))
	fa.script = func(msg string) (int, string) { return 200, end(msg, "You have had one item.") }
	h.clk.Add(time.Minute)
	h.logText(cid(), "what did I eat?")
	msg := fa.lastTurn()
	if !strings.Contains(msg, r.Items[0].ItemID+" | ") || !strings.Contains(msg, "skyr (food) | 250 g") {
		t.Errorf("the day state lacks the item: %s", msg)
	}
	if !strings.Contains(msg, "Joe: 250 g skyr") || !strings.Contains(msg, "Logged the skyr.") {
		t.Errorf("the day state lacks the chat: %s", msg)
	}
	if c, _, _ := fa.counts(); c != 1 {
		t.Errorf("the session was not reused: %d creates", c)
	}
}

// A photo turn: each photo goes through the media route, every path is in
// the message, the rows carry the photo ids of the entry.
func TestChatPhotoTurn(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged the plate.", food("glass noodle salad", 350, 420, 18, 2))
	body, ct := multipartBody(t, map[string]string{"client_id": cid()},
		[]filePart{{"image", "a.jpg", "image/jpeg", testJPEG(300, 200)}, {"image", "b.jpg", "image/jpeg", testJPEG(200, 300)}})
	rec := h.do("POST", "/fuel/log", body, ct)
	r := decode[LogResponse](t, rec)
	if rec.Code != 200 || r.Intent != "log" || len(r.PhotoIDs) != 2 || len(r.Items) != 1 {
		t.Fatalf("%d %+v", rec.Code, r)
	}
	fa.mu.Lock()
	media, files := append([]string{}, fa.media...), fa.files
	fa.mu.Unlock()
	if files != 2 || len(media) != 2 || !strings.HasPrefix(media[0], "FUEL PHOTO 1 of 2") || !strings.HasPrefix(media[1], "FUEL PHOTO 2 of 2") {
		t.Fatalf("media turns: %d %q", files, media)
	}
	msg := fa.lastTurn()
	if !strings.Contains(msg, "/home/agentd/media/web/photo-1.jpg") || !strings.Contains(msg, "/home/agentd/media/web/photo-2.jpg") || !strings.Contains(msg, "Photos of this turn (2)") {
		t.Errorf("the message lacks the photo paths")
	}
	if strings.Contains(media[0], "e_") || strings.Contains(media[0], "Capability") {
		t.Error("a photo upload carries the capability")
	}
	rows := h.vars.rows("var-food")
	if len(rows) != 1 || rows[0]["photo_ref"] != strings.Join(r.PhotoIDs, ",") {
		t.Errorf("rows %v", rows)
	}
	// No second opinion for an entry of the agent chat (22.9).
	if h.svc.recal != nil && len(h.svc.recal.all()) != 0 {
		t.Error("a recalibration job was made for an agent chat entry")
	}
}

// fix, undo, move and relog in a turn bind to the entry of the turn.
func TestChatCorrectUndoMoveRelog(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged chicken, water and wine.", food("roast chicken", 260, 620, 70, 6),
		map[string]any{"item": "water", "kind": "drink", "volume_ml": 250, "kcal": 0, "protein_g": 0, "carbs_g": 0, "fat_g": 0, "sat_fat_g": 0},
		map[string]any{"item": "red wine", "kind": "drink", "volume_ml": 150, "alcohol_g": 14, "kcal": 125, "protein_g": 0, "carbs_g": 4, "fat_g": 0, "sat_fat_g": 0})
	r := decode[LogResponse](t, h.logText(cid(), "chicken 260 g, a glass of water, a glass of red wine"))
	chicken, water, wine := r.Items[0].ItemID, r.Items[1].ItemID, r.Items[2].ItemID

	// "one more bite": a delta fix. It adds, and a second fix is refused.
	h.clk.Add(time.Minute)
	fa.script = func(msg string) (int, string) {
		c := capOf(msg)
		rec := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": opID(), "item_id": chicken, "portion_g_delta": 20})
		if rec.Code != 200 {
			t.Errorf("fix: %d %s", rec.Code, rec.Body)
		}
		w := decode[turnWriteResponse](t, rec)
		if w.Result != "written" || len(w.Lines) != 1 || !strings.Contains(w.Lines[0], "280 g") {
			t.Errorf("fix answer: %+v", w)
		}
		again := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": opID(), "item_id": chicken, "portion_g_delta": 20})
		if again.Code != 409 || errCode(t, again) != "item_changed_in_turn" {
			t.Errorf("a second fix of the item: %d %s", again.Code, again.Body)
		}
		return 200, end(msg, "Added one bite: the chicken is 280 g now.")
	}
	fx := decode[LogResponse](t, h.logText(cid(), "one more bite"))
	if fx.Intent != "correct" || fx.Status != StatusDone || len(fx.Items) != 0 {
		t.Fatalf("fix turn: %+v", fx)
	}
	if tb := textBlocks(fx.Blocks); len(tb) != 1 || tb[0] != "Added one bite: the chicken is 280 g now." {
		t.Errorf("fix blocks: %q", tb)
	}
	if _, ok := widgetOf(fx.Blocks); !ok {
		t.Error("a turn that wrote has no widget")
	}
	for _, d := range decode[struct {
		Items []DayItem `json:"items"`
	}](t, h.get("/fuel/day")).Items {
		if d.RowKey == chicken && (d.PortionG == nil || *d.PortionG != 280) {
			t.Errorf("chicken is %v g", d.PortionG)
		}
	}

	// "remove the water": an undo, never a fix.
	h.clk.Add(time.Minute)
	fa.script = func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/undo", capOf(msg), map[string]any{"client_id": opID(), "item_id": water})
		if rec.Code != 200 {
			t.Errorf("undo: %d %s", rec.Code, rec.Body)
		}
		return 200, end(msg, "Removed the water.")
	}
	un := decode[LogResponse](t, h.logText(cid(), "remove the water"))
	if un.Intent != "undo" || un.Status != StatusDone {
		t.Fatalf("undo turn: %+v", un)
	}
	if it, _ := h.svc.journal.Item(water); !h.svc.viewItem(it).state.Undone {
		t.Error("the water is not undone")
	}

	// "the wine was yesterday": a move. Today's alcohol goes, yesterday has it.
	h.clk.Add(time.Minute)
	fa.script = func(msg string) (int, string) {
		c := capOf(msg)
		rec := h.op("POST", "/fuel/move", c, map[string]any{"client_id": opID(), "day": "yesterday", "items": []string{wine}})
		if rec.Code != 200 {
			t.Errorf("move: %d %s", rec.Code, rec.Body)
		}
		again := h.op("POST", "/fuel/move", c, map[string]any{"client_id": opID(), "day": "yesterday", "items": []string{wine}})
		if again.Code != 409 {
			t.Errorf("a second move of the item: %d %s", again.Code, again.Body)
		}
		return 200, end(msg, "Moved the wine to yesterday.")
	}
	mv := decode[LogResponse](t, h.logText(cid(), "the wine was yesterday"))
	if mv.Intent != "move" || mv.Status != StatusDone {
		t.Fatalf("move turn: %+v", mv)
	}
	if a := intake(mv.Snapshot, "alcohol_g").Consumed; a != 0 {
		t.Errorf("alcohol today after the move: %v", a)
	}
	y := h.snap(t, "?date=2026-09-30")
	if a := intake(y, "alcohol_g").Consumed; a != 14 {
		t.Errorf("alcohol yesterday after the move: %v", a)
	}

	// relog of a recent item in a turn.
	h.clk.Add(time.Minute)
	var key string
	for _, ri := range decode[struct {
		Items []RecentItem `json:"items"`
	}](t, h.op("GET", "/fuel/recent", "", nil)).Items {
		if ri.Item == "roast chicken" {
			key = ri.Key
		}
	}
	fa.script = func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/relog", capOf(msg), map[string]any{"client_id": opID(), "key": key, "new": true})
		if rec.Code != 200 {
			t.Errorf("relog: %d %s", rec.Code, rec.Body)
		}
		return 200, end(msg, "Logged the chicken again.")
	}
	rl := decode[LogResponse](t, h.logText(cid(), "the same chicken again"))
	if rl.Intent != "log" || len(rl.Items) != 1 || rl.Items[0].Item != "roast chicken" {
		t.Fatalf("relog turn: %+v", rl)
	}
	// Five turns, five entries, and no entry of a write of its own.
	if n := len(h.svc.journal.Entries()); n != 5 || len(chatEntries(h)) != 5 {
		t.Errorf("entries: %d", n)
	}
	// One reply line per turn in the feed, no line of a write route.
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.get("/fuel/feed"))
	fuelLines := 0
	for _, it := range feed.Items {
		if it.Role == "fuel" {
			fuelLines++
		}
	}
	if fuelLines != 5 {
		t.Errorf("fuel lines in the feed: %d", fuelLines)
	}
}

// The agent token: reads and turn-bound writes only.
func TestChatAgentTokenScope(t *testing.T) {
	h, fa := newChat(t)
	var capability string
	fa.script = func(msg string) (int, string) {
		capability = capOf(msg)
		// Inside the turn: scope checks with a live capability.
		for _, path := range []string{"/fuel/fraction", "/fuel/recalibration/revert", "/fuel/record", "/fuel/log"} {
			if rec := h.op("POST", path, capability, map[string]any{"client_id": opID()}); rec.Code != 403 || errCode(t, rec) != "agent_scope" {
				t.Errorf("%s with the agent token in a turn: %d %s", path, rec.Code, rec.Body)
			}
		}
		// A capability of the other instance, and an unknown one.
		for _, c := range []string{"p_" + strings.TrimPrefix(capability, "e_"), "e_00000000000000000000000000000000"} {
			if rec := h.op("POST", "/fuel/items", c, map[string]any{"client_id": opID(), "items": []any{food("x", 1, 1, 0, 0)}}); rec.Code != 409 || errCode(t, rec) != "turn_closed" {
				t.Errorf("a foreign capability: %d %s", rec.Code, rec.Body)
			}
		}
		// The app token never carries a turn.
		b, _ := json.Marshal(map[string]any{"client_id": opID(), "items": []any{food("x", 1, 1, 0, 0)}})
		req := httptest.NewRequest("POST", "/fuel/items", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set(turnHeader, capability)
		rr := httptest.NewRecorder()
		h.h.ServeHTTP(rr, req)
		if rr.Code != 400 {
			t.Errorf("the app token with a capability: %d", rr.Code)
		}
		return 200, end(msg, "ok")
	}
	h.logText(cid(), "hello")
	// Reads are open to the agent token.
	for _, path := range []string{"/fuel/day", "/fuel/week", "/fuel/snapshot", "/fuel/recent", "/fuel/feed"} {
		if rec := h.op("GET", path, "", nil); rec.Code != 200 {
			t.Errorf("GET %s with the agent token: %d", path, rec.Code)
		}
	}
	// Everything else is refused.
	for _, c := range []struct{ m, p string }{{"GET", "/fuel/records"}, {"GET", "/fuel/photo/ph_x"}, {"GET", "/fuel/entry/en_x"},
		{"POST", "/fuel/log"}, {"POST", "/fuel/record"}, {"POST", "/fuel/calibration/run"}} {
		if rec := h.op(c.m, c.p, "", map[string]any{"client_id": opID(), "text": "x"}); rec.Code != 403 {
			t.Errorf("%s %s with the agent token: %d", c.m, c.p, rec.Code)
		}
	}
	// A write with the agent token and no capability.
	for _, path := range []string{"/fuel/items", "/fuel/fix", "/fuel/undo", "/fuel/move", "/fuel/relog"} {
		if rec := h.op("POST", path, "", map[string]any{"client_id": opID()}); rec.Code != 403 || errCode(t, rec) != "agent_scope" {
			t.Errorf("%s with no capability: %d %s", path, rec.Code, rec.Body)
		}
	}
	// The capability of the closed turn writes nothing.
	rec := h.op("POST", "/fuel/items", capability, map[string]any{"client_id": opID(), "items": []any{food("late apple", 100, 52, 0, 0)}})
	if rec.Code != 409 || errCode(t, rec) != "turn_closed" {
		t.Errorf("a write after the close: %d %s", rec.Code, rec.Body)
	}
	if n := len(h.vars.rows("var-food")); n != 0 {
		t.Errorf("rows: %d", n)
	}
	// A wrong token is still 401.
	req := httptest.NewRequest("GET", "/fuel/day", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Errorf("wrong token: %d", rr.Code)
	}
}

// Idempotency and the one-write rules inside a turn.
func TestChatTurnIdempotencyAndDuplicates(t *testing.T) {
	h, fa := newChat(t)
	fa.script = func(msg string) (int, string) {
		c := capOf(msg)
		id := opID()
		body := map[string]any{"client_id": id, "items": []any{food("scrambled eggs", 150, 220, 14, 4.5)}}
		first := h.op("POST", "/fuel/items", c, body)
		again := h.op("POST", "/fuel/items", c, body) // a retry of fuel-op: the same client_id
		if first.Code != 200 || again.Code != 200 {
			t.Errorf("items: %d then %d %s", first.Code, again.Code, again.Body)
		}
		a, b := decode[turnWriteResponse](t, first), decode[turnWriteResponse](t, again)
		if len(a.Items) != 1 || len(b.Items) != 1 || a.Items[0].ItemID != b.Items[0].ItemID {
			t.Errorf("the replay is another item: %+v %+v", a.Items, b.Items)
		}
		// The same client_id with another body.
		other := h.op("POST", "/fuel/items", c, map[string]any{"client_id": id, "items": []any{food("toast", 40, 100, 3, 0.2)}})
		if other.Code != 409 || errCode(t, other) != "idempotency_conflict" {
			t.Errorf("another body: %d %s", other.Code, other.Body)
		}
		// The same food again under a new client_id (a lost answer).
		dup := h.op("POST", "/fuel/items", c, map[string]any{"client_id": opID(), "items": []any{food("Scrambled  Eggs", 150, 220, 14, 4.5)}})
		if dup.Code != 409 || errCode(t, dup) != "turn_item_exists" {
			t.Errorf("the same name again: %d %s", dup.Code, dup.Body)
		}
		// Twice in one call.
		two := h.op("POST", "/fuel/items", c, map[string]any{"client_id": opID(), "items": []any{food("toast", 40, 100, 3, 0.2), food("toast", 40, 100, 3, 0.2)}})
		if two.Code != 409 {
			t.Errorf("twice in one call: %d", two.Code)
		}
		return 200, end(msg, "Logged the eggs.")
	}
	r := decode[LogResponse](t, h.logText(cid(), "[photo of eggs]"))
	if len(r.Items) != 1 || len(h.vars.rows("var-food")) != 1 {
		t.Fatalf("items %d rows %d", len(r.Items), len(h.vars.rows("var-food")))
	}
	eggs := r.Items[0].ItemID

	// 21 s later: "4x eggs and a little bit of butter". The same food is
	// refused as a new item; it is revised, and only the butter is new.
	h.clk.Add(21 * time.Second)
	fa.script = func(msg string) (int, string) {
		c := capOf(msg)
		dup := h.op("POST", "/fuel/items", c, map[string]any{"client_id": opID(), "items": []any{food("eggs", 200, 290, 19, 6), food("butter", 5, 37, 0, 2.6)}})
		if dup.Code != 409 || errCode(t, dup) != "likely_duplicate" || !strings.Contains(dup.Body.String(), eggs) {
			t.Errorf("likely duplicate: %d %s", dup.Code, dup.Body)
		}
		rv := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": opID(), "item_id": eggs, "revised": map[string]any{"item": "scrambled eggs, 4 eggs", "portion_g": 220,
			"kcal": 320, "protein_g": 22, "carbs_g": 2, "net_carbs_g": 2, "fat_g": 24, "sat_fat_g": 7, "fiber_g": 0, "food_class": "meat_fish"}})
		if rv.Code != 200 {
			t.Errorf("revised: %d %s", rv.Code, rv.Body)
		}
		add := h.op("POST", "/fuel/items", c, map[string]any{"client_id": opID(), "items": []any{food("butter", 5, 37, 0, 2.6)}})
		if add.Code != 200 {
			t.Errorf("butter: %d %s", add.Code, add.Body)
		}
		return 200, end(msg, "Updated the eggs to 4 eggs and added the butter.")
	}
	r2 := decode[LogResponse](t, h.logText(cid(), "4x eggs and a little bit of butter and salt"))
	if r2.Intent != "log" || len(r2.Items) != 1 || r2.Items[0].Item != "butter" {
		t.Fatalf("second turn: %+v", r2.Items)
	}
	day := decode[struct {
		Items []DayItem `json:"items"`
	}](t, h.get("/fuel/day"))
	eggItems := 0
	for _, d := range day.Items {
		if strings.Contains(d.Item, "egg") {
			eggItems++
			if d.PortionG == nil || *d.PortionG != 220 {
				t.Errorf("eggs portion %v", d.PortionG)
			}
		}
	}
	if eggItems != 1 || len(day.Items) != 2 {
		t.Errorf("the day has %d egg items and %d items", eggItems, len(day.Items))
	}

	// "I had another two eggs": more food, confirmed with new.
	h.clk.Add(time.Minute)
	fa.script = func(msg string) (int, string) {
		c := capOf(msg)
		body := map[string]any{"client_id": opID(), "items": []any{food("fried eggs", 100, 180, 12, 4)}}
		if dup := h.op("POST", "/fuel/items", c, body); dup.Code != 409 {
			t.Errorf("without new: %d", dup.Code)
		}
		body["new"] = true
		body["client_id"] = opID()
		if ok := h.op("POST", "/fuel/items", c, body); ok.Code != 200 {
			t.Errorf("with new: %d %s", ok.Code, ok.Body)
		}
		return 200, end(msg, "Logged two more eggs.")
	}
	h.logText(cid(), "I had another two eggs")
	day = decode[struct {
		Items []DayItem `json:"items"`
	}](t, h.get("/fuel/day"))
	if len(day.Items) != 3 {
		t.Errorf("after the additive turn: %d items", len(day.Items))
	}
}

// "for yesterday": the items land on the day before the MESSAGE, and the
// reply shows that day.
func TestChatLogForYesterday(t *testing.T) {
	h, fa := newChat(t)
	fa.script = func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "day": "yesterday", "items": []any{
			map[string]any{"item": "champagne", "kind": "drink", "volume_ml": 300, "alcohol_g": 28, "kcal": 230, "protein_g": 0, "carbs_g": 4, "fat_g": 0, "sat_fat_g": 0}}})
		if rec.Code != 200 {
			t.Errorf("items: %d %s", rec.Code, rec.Body)
		}
		return 200, end(msg, "Logged the champagne for yesterday.")
	}
	r := decode[LogResponse](t, h.logText(cid(), "log for yesterday: 2 glasses of champagne"))
	rows := h.vars.rows("var-food")
	if len(rows) != 1 || rows[0]["_record_date"] != "2026-09-30" {
		t.Fatalf("rows %v", rows)
	}
	if r.Snapshot.Date != "2026-09-30" || intake(r.Snapshot, "alcohol_g").Consumed != 28 {
		t.Errorf("snapshot %s %v", r.Snapshot.Date, intake(r.Snapshot, "alcohol_g").Consumed)
	}
	if e := chatEntries(h)[0]; e.DayLabel != "Wed 30 Sep" || e.Date != "2026-09-30" {
		t.Errorf("entry %+v", e)
	}
}

// Each failure class: a clear text, nothing written, no second turn, a new
// session for the next turn.
func TestChatFailures(t *testing.T) {
	cases := []struct {
		name   string
		script func(msg string) (int, string)
		text   string
	}{
		{"error status", func(string) (int, string) { return 500, "" }, chatNoAnswer},
		{"empty answer", func(string) (int, string) { return 200, "   " }, chatNoAnswer},
		{"checkpoint text", func(string) (int, string) { return 200, "CHECKPOINT SAVED" }, chatNoAnswer},
		{"foreign mark", func(string) (int, string) { return 200, "Logged two eggs.\nFUEL-END NOTMINE" }, chatNoAnswer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fa := newChat(t)
			fa.script = c.script
			id := cid()
			rec := h.logText(id, "two eggs")
			r := decode[LogResponse](t, rec)
			tb := textBlocks(r.Blocks)
			if rec.Code != 200 || r.Status != StatusDone || r.Intent != "question" || len(tb) != 1 || tb[0] != c.text || len(r.Blocks) != 1 {
				t.Fatalf("%d %+v", rec.Code, r.Blocks)
			}
			if n := len(h.vars.rows("var-food")); n != 0 {
				t.Errorf("rows %d", n)
			}
			if cr, turns, tried := fa.counts(); cr != 1 || turns != 1 || tried != 1 {
				t.Errorf("a failed turn was sent again: creates %d turns %d tried %d", cr, turns, tried)
			}
			feed := h.get("/fuel/feed").Body.String()
			if strings.Count(feed, `"text":"two eggs"`) != 1 || !strings.Contains(feed, c.text) {
				t.Errorf("feed: %s", feed)
			}
			if again := h.logText(id, "two eggs"); !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
				t.Error("the replay differs")
			}
			if _, _, tried := fa.counts(); tried != 1 {
				t.Error("the replay started a turn")
			}
			// The next turn gets a new session.
			fa.mu.Lock()
			fa.script = func(msg string) (int, string) { return 200, end(msg, "fine") }
			fa.mu.Unlock()
			h.logText(cid(), "hello again")
			if cr, _, _ := fa.counts(); cr != 2 {
				t.Errorf("no new session after a failure: %d", cr)
			}
		})
	}
}

func TestChatUnreachableAndTimeout(t *testing.T) {
	h, fa := newChat(t)
	fa.srv.Close()
	rec := h.logText(cid(), "two eggs")
	tb := textBlocks(decode[LogResponse](t, rec).Blocks)
	if rec.Code != 200 || len(tb) != 1 || tb[0] != chatUnreachable {
		t.Fatalf("unreachable: %d %q", rec.Code, tb)
	}
	if !strings.Contains(h.get("/fuel/feed").Body.String(), `"text":"two eggs"`) {
		t.Error("the message is not in the feed")
	}
	// The buttons work with the agent down: POST /fuel/items and undo with the app token.
	it := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{food("apple", 150, 78, 0, 0)}}))
	if len(it.Items) != 1 {
		t.Fatalf("items by the app: %+v", it)
	}
	if u := h.mutate("undo", cid(), it.Items[0].ItemID, nil); u.Code != 200 {
		t.Errorf("undo button: %d %s", u.Code, u.Body)
	}

	// A timeout: one interrupt, the failure text, the turn is closed.
	h2, fa2 := newChat(t, func(o *Options) { o.Chat.Timeout = 80 * time.Millisecond })
	var capability string
	fa2.delay = 2 * time.Second
	fa2.script = func(msg string) (int, string) { capability = capOf(msg); return 200, end(msg, "late") }
	rec = h2.logText(cid(), "slow one")
	tb = textBlocks(decode[LogResponse](t, rec).Blocks)
	if rec.Code != 200 || len(tb) != 1 || tb[0] != chatNoAnswer {
		t.Fatalf("timeout: %d %q", rec.Code, tb)
	}
	waitFor(t, "the interrupt", func() bool { fa2.mu.Lock(); defer fa2.mu.Unlock(); return fa2.interrupt == 1 })
	_ = capability
	msg := fa2.lastTurn()
	late := h2.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "items": []any{food("late apple", 100, 52, 0, 0)}})
	if late.Code != 409 || len(h2.vars.rows("var-food")) != 0 {
		t.Errorf("a write after the timeout: %d", late.Code)
	}
}

// A failure after a write: the rows stay, the reply names them by code.
func TestChatFailureAfterWrite(t *testing.T) {
	h, fa := newChat(t)
	fa.script = func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "items": []any{food("scrambled eggs", 200, 300, 20, 6), food("butter", 5, 37, 0, 2.6)}})
		if rec.Code != 200 {
			t.Errorf("items: %d", rec.Code)
		}
		return 500, ""
	}
	rec := h.logText(cid(), "eggs with butter")
	r := decode[LogResponse](t, rec)
	tb := textBlocks(r.Blocks)
	if rec.Code != 200 || r.Intent != "log" || len(r.Items) != 2 || len(tb) != 2 || tb[0] != "Logged: scrambled eggs and butter." || tb[1] != chatNoFinish {
		t.Fatalf("%d %q", rec.Code, tb)
	}
	if len(h.vars.rows("var-food")) != 2 {
		t.Error("the rows of the turn are gone")
	}
	if _, turns, tried := fa.counts(); turns != 1 || tried != 1 {
		t.Error("the turn was sent again after a write")
	}
}

// A session that is gone (404): the turn did not run; one repeat on a new session.
func TestChatSessionGone(t *testing.T) {
	h, fa := newChat(t)
	h.logText(cid(), "first")
	fa.mu.Lock()
	fa.sessions = map[string]bool{} // the daemon reclaimed it
	fa.mu.Unlock()
	h.clk.Add(time.Minute)
	r := decode[LogResponse](t, h.logText(cid(), "second"))
	c, turns, tried := fa.counts()
	if tb := textBlocks(r.Blocks); c != 2 || turns != 2 || tried != 3 || len(tb) != 1 || tb[0] != "Hello from the agent." {
		t.Fatalf("creates %d turns %d tried %d blocks %q", c, turns, tried, textBlocks(r.Blocks))
	}
	// The same with photos: the upload answers 404, the photos go again.
	fa.mu.Lock()
	fa.sessions = map[string]bool{}
	fa.mu.Unlock()
	body, ct := multipartBody(t, map[string]string{"client_id": cid(), "text": "this"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(300, 200)}})
	rec := h.do("POST", "/fuel/log", body, ct)
	if tb := textBlocks(decode[LogResponse](t, rec).Blocks); rec.Code != 200 || len(tb) != 1 || tb[0] != "Hello from the agent." {
		t.Fatalf("photo after a 404: %d %q", rec.Code, tb)
	}
	if !strings.Contains(fa.lastTurn(), "photo-1.jpg") {
		t.Error("the photo path is not in the repeated turn")
	}
}

// A turn longer than the wait: 202 pending, the message is in the feed at
// once, then the entry is final. A repeat never starts a turn.
func TestChatPendingThenDone(t *testing.T) {
	h, fa := newChat(t, func(o *Options) { o.Chat.SyncWait = 30 * time.Millisecond })
	fa.delay = 300 * time.Millisecond
	fa.script = logsItems(h, "Logged the eggs.", food("scrambled eggs", 200, 300, 20, 6))
	id := cid()
	rec := h.logText(id, "eggs please")
	r := decode[LogResponse](t, rec)
	if tb := textBlocks(r.Blocks); rec.Code != 202 || r.Status != StatusPending || len(tb) != 1 || tb[0] != chatPendingText {
		t.Fatalf("%d %+v", rec.Code, r.Blocks)
	}
	if !strings.Contains(h.get("/fuel/feed").Body.String(), `"text":"eggs please"`) {
		t.Error("the user line is not in the feed while the turn runs")
	}
	if e := h.get("/fuel/entry/" + r.EntryID); e.Code != 202 {
		t.Errorf("entry while pending: %d", e.Code)
	}
	if again := h.logText(id, "eggs please"); again.Code != 202 {
		t.Errorf("replay while pending: %d", again.Code)
	}
	waitFor(t, "the entry", func() bool { return h.get("/fuel/entry/"+r.EntryID).Code == 200 })
	eb := h.get("/fuel/entry/" + r.EntryID).Body.String()
	if !strings.Contains(eb, "Logged the eggs.") || !strings.Contains(eb, `"item":"scrambled eggs"`) {
		t.Errorf("the polled entry: %s", eb)
	}
	waitFor(t, "the feed", func() bool { return strings.Contains(h.get("/fuel/feed").Body.String(), "Logged the eggs.") })
	final := h.logText(id, "eggs please")
	fr := decode[LogResponse](t, final)
	if final.Code != 200 || fr.Intent != "log" || len(fr.Items) != 1 {
		t.Fatalf("final replay: %d %+v", final.Code, fr)
	}
	if _, turns, tried := fa.counts(); turns != 1 || tried != 1 {
		t.Errorf("a repeat started a turn: %d %d", turns, tried)
	}
	feed := h.get("/fuel/feed").Body.String()
	if strings.Count(feed, `"text":"eggs please"`) != 1 || strings.Contains(feed, chatPendingText) {
		t.Errorf("feed: %s", feed)
	}
}

// A restart while a turn runs: the entry becomes failed, the message is in
// the feed, the agent is not asked again, the rows of the turn stay.
func TestChatRestartWhilePending(t *testing.T) {
	h, fa := newChat(t, func(o *Options) { o.Chat.SyncWait = 20 * time.Millisecond })
	block := make(chan struct{})
	fa.script = func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "items": []any{food("scrambled eggs", 200, 300, 20, 6)}})
		if rec.Code != 200 {
			t.Errorf("items: %d", rec.Code)
		}
		<-block
		return 200, end(msg, "too late")
	}
	id := cid()
	rec := h.logText(id, "eggs before the restart")
	if rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	waitFor(t, "the row", func() bool { return len(h.vars.rows("var-food")) == 1 })
	msg := fa.lastTurn()
	done := make(chan struct{})
	go func() { h.restart(); close(done) }()
	time.Sleep(50 * time.Millisecond)
	close(block)
	<-done
	es := chatEntries(h)
	if len(es) != 1 || es[0].Agent != agentFailed || len(es[0].ItemIDs) != 1 {
		t.Fatalf("after the restart: %+v", es)
	}
	final := h.logText(id, "eggs before the restart")
	tb := textBlocks(decode[LogResponse](t, final).Blocks)
	if final.Code != 200 || len(tb) != 2 || tb[0] != "Logged: scrambled eggs." || tb[1] != chatNoFinish {
		t.Fatalf("final: %d %q", final.Code, tb)
	}
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Errorf("the agent was asked again: %d turns", turns)
	}
	// The old capability is dead after the restart.
	late := h.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "items": []any{food("late apple", 100, 52, 0, 0)}})
	if late.Code != 409 || len(h.vars.rows("var-food")) != 1 {
		t.Errorf("a write with the capability of before the restart: %d", late.Code)
	}
	if fa.interrupt == 0 {
		t.Error("the old session was not interrupted")
	}
	feed := h.get("/fuel/feed").Body.String()
	if strings.Count(feed, `"text":"eggs before the restart"`) != 1 || !strings.Contains(feed, chatNoFinish) {
		t.Errorf("feed: %s", feed)
	}
}

// The off file and the default backend use the estimator.
func TestChatOffUsesEstimator(t *testing.T) {
	h, fa := newChat(t)
	h.model.fn = func(ModelInput) string { return skyrWalnuts }
	if err := os.WriteFile(filepath.Join(h.opts.StateDir, "chat-agent.off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := decode[LogResponse](t, h.logText(cid(), "250 g skyr and 30 g walnuts"))
	if r.Intent != "log" || len(r.Items) != 2 {
		t.Fatalf("estimator path: %+v", r)
	}
	if c, _, tried := fa.counts(); c != 0 || tried != 0 {
		t.Error("the agent was asked with the off file")
	}
	if len(chatEntries(h)) != 0 {
		t.Error("an estimator entry carries the chat mark")
	}
	_ = os.Remove(filepath.Join(h.opts.StateDir, "chat-agent.off"))
	h.model.fn = func(ModelInput) string { t.Error("model called"); return skyrWalnuts }
	h.clk.Add(time.Minute)
	h.logText(cid(), "hello")
	if _, _, tried := fa.counts(); tried != 1 {
		t.Error("the agent was not asked after the off file went")
	}
	// The default backend.
	h2 := newHarness(t)
	if h2.svc.chatAgentOn() {
		t.Error("the default backend is the agent")
	}
}

// The clinical words are not sent to the agent.
func TestChatClinicalGuard(t *testing.T) {
	h, fa := newChat(t)
	for _, text := range []string{"my blood pressure was 150 over 95, should I change my pills", "is my aortic valve ok with coffee?"} {
		rec := h.logText(cid(), text)
		r := decode[LogResponse](t, rec)
		tb := textBlocks(r.Blocks)
		if rec.Code != 200 || len(tb) != 2 || tb[0] != clinicalLine || tb[1] != chatClinicalNoLog || len(r.Blocks) != 2 {
			t.Fatalf("%q: %d %+v", text, rec.Code, r.Blocks)
		}
	}
	if c, _, tried := fa.counts(); c != 0 || tried != 0 {
		t.Error("a clinical turn reached the agent")
	}
	// Such a turn is not in the chat lines of a later message.
	h.clk.Add(time.Minute)
	h.logText(cid(), "two eggs")
	if msg := fa.lastTurn(); strings.Contains(msg, "150 over 95") || strings.Contains(msg, clinicalLine) {
		t.Error("a clinical turn is in the day state of a later turn")
	}
}

// POST /fuel/preview writes nothing.
func TestChatPreview(t *testing.T) {
	h, _ := newChat(t)
	rec := h.op("POST", "/fuel/preview", "", map[string]any{"items": []any{food("almonds", 30, 174, 6.3, 1.1), food("walnuts", 20, 131, 3, 1.2)}})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var p struct {
		Written bool                `json:"written"`
		Sum     map[string]*float64 `json:"sum"`
		Budgets []previewBudget     `json:"budgets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Written || p.Sum["kcal"] == nil || *p.Sum["kcal"] != 305 || len(p.Budgets) == 0 {
		t.Fatalf("preview: %s", rec.Body)
	}
	for _, b := range p.Budgets {
		if b.Key == "protein_g" && (b.After == nil || *b.After != 9.3 || b.LeftAfter == nil || *b.LeftAfter != 150.7) {
			t.Errorf("protein after: %+v", b)
		}
	}
	if len(h.vars.rows("var-food")) != 0 || len(h.svc.journal.Entries()) != 0 {
		t.Error("the preview wrote")
	}
	if bad := h.op("POST", "/fuel/preview", "", map[string]any{"items": []any{map[string]any{"item": "x"}}}); bad.Code != 400 {
		t.Errorf("a bad item: %d", bad.Code)
	}
}

// The answer text: the summary part of a watch reply and the end line go.
func TestParseChatAnswer(t *testing.T) {
	for _, c := range []struct {
		raw, mark, want string
		ok              bool
	}{
		{"Logged the eggs.\nFUEL-END AB12", "AB12", "Logged the eggs.", true},
		{"Logged the eggs.\nFUEL-END AB12\n---SUMMARY---\nshort FUEL-END AB12", "AB12", "Logged the eggs.", true},
		{"Logged **the** eggs.\n\n`FUEL-END AB12`", "AB12", "Logged the eggs.", true},
		{"Logged the eggs.", "AB12", "Logged the eggs.", true}, // the model forgot the line
		{"FUEL-END AB12", "AB12", "", true},
		{"Logged the eggs.\nFUEL-END ZZ99", "AB12", "", false},
		{"CHECKPOINT SAVED", "AB12", "", false},
		{"CHECKPOINT SAVED\nFUEL-END AB12", "AB12", "", false},
		{"FUEL-READY", "AB12", "", false},
		{"", "AB12", "", false},
	} {
		got, ok := parseChatAnswer(c.raw, c.mark)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q %v, want %q %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// At most 4 accepted turns wait; one more is 503 before anything is stored.
func TestChatQueueFull(t *testing.T) {
	h, fa := newChat(t, func(o *Options) { o.Chat.SyncWait = 5 * time.Millisecond; o.Chat.QueueMax = 2 })
	block := make(chan struct{})
	fa.script = func(msg string) (int, string) { <-block; return 200, end(msg, "ok") }
	for i := 0; i < 3; i++ { // one runs, two wait
		if rec := h.logText(cid(), fmt.Sprintf("message %d", i)); rec.Code != 202 {
			t.Fatalf("message %d: %d %s", i, rec.Code, rec.Body)
		}
		if i == 0 {
			waitFor(t, "the first turn", func() bool { _, _, tried := fa.counts(); return tried == 1 })
		}
	}
	entries := len(h.svc.journal.Entries())
	rec := h.logText(cid(), "one too many")
	if rec.Code != 503 || errCode(t, rec) != "busy" {
		t.Fatalf("the full queue: %d %s", rec.Code, rec.Body)
	}
	if len(h.svc.journal.Entries()) != entries || strings.Contains(h.get("/fuel/feed").Body.String(), "one too many") {
		t.Error("a refused message was stored")
	}
	close(block)
	waitFor(t, "all turns", func() bool { _, turns, _ := fa.counts(); return turns == 3 })
	waitFor(t, "all entries final", func() bool {
		for _, e := range chatEntries(h) {
			if e.Agent == agentPending {
				return false
			}
		}
		return true
	})
}

// No log line holds the text, the answer, the capability or a token.
func TestChatLogsNoContent(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged your secret sandwich.", food("secret sandwich", 200, 400, 20, 5))
	h.logText(cid(), "my secret sandwich please")
	msg := fa.lastTurn()
	out := buf.String()
	for _, bad := range []string{"secret sandwich", capOf(msg), opTok, agentTok, testToken, markOf(msg)} {
		if strings.Contains(out, bad) {
			t.Errorf("the log holds %q", bad)
		}
	}
	if !strings.Contains(out, "fuel: chat en_") || !strings.Contains(out, "agent=ok") {
		t.Errorf("no chat log line: %s", out)
	}
}

// Coffee from a moka pot is unfiltered for the brew-method lever, also when
// the estimate says espresso (spec 18.6).
func TestMokaIsUnfiltered(t *testing.T) {
	h, fa := newChat(t)
	fa.script = func(msg string) (int, string) {
		rec := h.op("POST", "/fuel/items", capOf(msg), map[string]any{"client_id": opID(), "items": []any{
			map[string]any{"item": "Bialetti moka coffee", "kind": "drink", "volume_ml": 60, "caffeine_mg": 100, "kcal": 2, "protein_g": 0, "carbs_g": 0, "fat_g": 0, "sat_fat_g": 0,
				"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 0, "pulses_g": 0, "plant_protein_g": 0, "brew_method": "espresso"}}}})
		if rec.Code != 200 {
			t.Errorf("items: %d %s", rec.Code, rec.Body)
		}
		return 200, end(msg, "Logged the moka coffee.")
	}
	h.logText(cid(), "a Bialetti coffee")
	rows := h.vars.rows("var-food")
	if len(rows) != 1 || rows[0]["brew_method"] != "unfiltered" {
		t.Fatalf("rows %v", rows)
	}
}

func TestChatConfig(t *testing.T) {
	base := "token = \"t\"\nvariables_key = \"v\"\nmodel_key = \"m\"\n"
	if c, err := ParseDaemonConfig([]byte(base)); err != nil || c.ChatBackend != "estimator" || c.ChatAgentWorkspace != "fuel" || c.ChatAgentTimeout != "130s" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, bad := range []string{
		"chat_backend = \"agent\"\n",                           // no tokens
		"chat_backend = \"agent\"\nchat_agent_token = \"x\"\n", // no agent_op_token
		"chat_backend = \"robot\"\n",
		"chat_agent_timeout = \"301s\"\n",
		"agent_op_token = \"t\"\n", // the app token
		"chat_backend = \"agent\"\nchat_agent_token = \"x\"\nagent_op_token = \"y\"\nchat_agent_workspace = \"a/b\"\n",
	} {
		if _, err := ParseDaemonConfig([]byte(base + bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
	if _, err := ParseDaemonConfig([]byte(base + "chat_backend = \"agent\"\nchat_agent_token = \"env:A\"\nagent_op_token = \"env:B\"\nchat_agent_workspace = \"fuel-e2e\"\n")); err != nil {
		t.Errorf("a valid agent config: %v", err)
	}
}

// A pending chat entry at a start is ended by the chat rule (failed, the
// rows kept), never by the question rule (fallback); no second opinion job
// is made for an agent chat entry, also not by the restart recovery.
func TestChatSweepNotQuestionSweepAndNoRecal(t *testing.T) {
	h, fa := newChat(t, func(o *Options) {
		o.Chat.SyncWait = 20 * time.Millisecond
		o.Recal = RecalOptions{Enabled: true, Agent: fakeRecalAgent{}}
	})
	fa.script = logsItems(h, "Logged the plate.", food("glass noodle salad", 350, 420, 18, 2))
	body, ct := multipartBody(t, map[string]string{"client_id": cid()}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(300, 200)}})
	r := decode[LogResponse](t, h.do("POST", "/fuel/log", body, ct))
	waitFor(t, "the entry", func() bool { return h.get("/fuel/entry/"+r.EntryID).Code == 200 })
	// A pending entry as a crash leaves it: journaled, no running turn.
	e := Entry{ID: newID("en_"), ClientID: cid(), Date: "2026-10-01", EatenAt: h.clk.Now(), CreatedAt: h.clk.Now(), Intent: "question",
		PhotoIDs: []string{}, UserText: "lost in a crash", Chat: chatAgent, Agent: agentPending, ReqHash: "x"}
	if err := h.svc.journal.Append(journalRec{T: "txn", Entry: &e}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	got, _ := h.svc.journal.Entry(e.ID)
	if got.Agent != agentFailed || got.AgentText != chatNoAnswer {
		t.Errorf("the pending chat entry after a start: agent=%q text=%q", got.Agent, got.AgentText)
	}
	if !strings.Contains(h.get("/fuel/feed").Body.String(), "lost in a crash") {
		t.Error("the message of the crashed turn is not in the feed")
	}
	if jobs := h.svc.recal.all(); len(jobs) != 0 {
		t.Errorf("recalibration jobs for agent chat entries: %+v", jobs)
	}
}

// A fix of an item that the same turn logged counts once in the widget.
func TestChatAddedCountsOnce(t *testing.T) {
	h, fa := newChat(t)
	fa.script = func(msg string) (int, string) {
		c := capOf(msg)
		rec := h.op("POST", "/fuel/items", c, map[string]any{"client_id": opID(), "items": []any{food("rice", 200, 260, 5, 0.2)}})
		id := decode[turnWriteResponse](t, rec).Items[0].ItemID
		if fx := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": opID(), "item_id": id, "share": 0.5}); fx.Code != 200 {
			t.Errorf("fix: %d %s", fx.Code, fx.Body)
		}
		return 200, end(msg, "Logged half of the rice.")
	}
	r := decode[LogResponse](t, h.logText(cid(), "rice, but only half"))
	wb, _ := widgetOf(r.Blocks)
	added, _ := json.Marshal(wb.Data)
	if !strings.Contains(string(added), `"kcal":130`) {
		t.Errorf("added: %s", added)
	}
	// Next turn: a fix to the amount the item already has writes nothing. Its
	// client_id is kept: a repeat replays, another request with it is a conflict.
	id := r.Items[0].ItemID
	h.clk.Add(time.Minute)
	fa.script = func(msg string) (int, string) {
		c, noop := capOf(msg), opID()
		n1 := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": noop, "item_id": id, "share": 0.5})
		n2 := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": noop, "item_id": id, "share": 0.5})
		if n1.Code != 200 || decode[turnWriteResponse](t, n1).Result != "nothing" || n2.Code != 200 || decode[turnWriteResponse](t, n2).Result != "nothing" {
			t.Errorf("a no-op fix and its repeat: %d %s / %d %s", n1.Code, n1.Body, n2.Code, n2.Body)
		}
		other := h.op("POST", "/fuel/fix", c, map[string]any{"client_id": noop, "item_id": id, "portion_g": 300})
		if other.Code != 409 || errCode(t, other) != "idempotency_conflict" {
			t.Errorf("the client_id of a no-op with another body: %d %s", other.Code, other.Body)
		}
		return 200, end(msg, "The rice is at half already.")
	}
	rows := len(h.vars.rows("var-food"))
	r2 := decode[LogResponse](t, h.logText(cid(), "the rice was half"))
	if len(h.vars.rows("var-food")) != rows || r2.Intent != "question" {
		t.Errorf("a no-op turn wrote: rows %d -> %d, intent %s", rows, len(h.vars.rows("var-food")), r2.Intent)
	}
}

// fakeRecalAgent fails the test's expectation when a job runs: none is made.
type fakeRecalAgent struct{}

func (fakeRecalAgent) Ask(context.Context, string, [][]byte) (string, string, error) {
	return "", "fake", fmt.Errorf("no job is expected")
}
