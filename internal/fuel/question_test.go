package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeQAgentd is the generic agentd API as a question needs it.
type fakeQAgentd struct {
	mu        sync.Mutex
	srv       *httptest.Server
	sessions  map[string]bool
	creates   []map[string]string // the bodies of POST /sessions
	turns     []string            // the texts of the turns that RAN
	tried     int                 // POST /sessions/:id/input calls (also 404)
	deletes   int
	interrupt int
	auth      []string
	answer    func(text string) (int, string) // status and result of a turn
	delay     time.Duration
	seq       int
}

const agentTok = "agentd-secret-token-xyz"

func newQAgentd(t *testing.T) *fakeQAgentd {
	f := &fakeQAgentd{sessions: map[string]bool{}}
	f.answer = func(string) (int, string) {
		return 200, "Yes. This chat is answered by Claude Opus 5.5 on agentd-safe."
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
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
			id := "sess-" + string(rune('0'+f.seq))
			f.sessions[id] = true
			f.creates = append(f.creates, body)
			f.mu.Unlock()
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "harness": "claude-code"})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/interrupt"):
			f.mu.Lock()
			f.interrupt++
			f.mu.Unlock()
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/input"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")[0]
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.tried++
			ok := f.sessions[id]
			d, ans := f.delay, f.answer
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
			code, res := ans(body.Text)
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

func (f *fakeQAgentd) counts() (creates, turns, tried int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), len(f.turns), f.tried
}

const modelQuestion = `{"intent":"question","items":[],"text":"Beans and tofu are good sources.","widgets":[]}`
const modelEmptyQuestion = `{"intent":"question","items":[],"text":"","widgets":[]}`

// newQ is a harness with question_backend "agent" and a fake agentd.
func newQ(t *testing.T, mut ...func(*Options)) (*harness, *fakeQAgentd) {
	t.Helper()
	fa := newQAgentd(t)
	ms := append([]func(*Options){func(o *Options) {
		o.Question = QuestionOptions{Backend: "agent", Model: "claude-opus-5-5",
			Agent: &AgentdClient{Base: fa.srv.URL, Token: agentTok, Client: loopbackClient()}}
	}}, mut...)
	h := newHarness(t, ms...)
	h.model.fn = func(ModelInput) string { return modelQuestion }
	return h, fa
}

// waitFor polls until ok or 3 s.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateFile(h *harness, name string) string {
	b, _ := os.ReadFile(filepath.Join(h.opts.StateDir, name))
	return string(b)
}

// A question goes to the agent: 200, the answer is the text block after the
// status line (digits kept), then the widget. Nothing is written to
// Variables. The message carries the brief, the text and the snapshot, the
// session carries the title and the model. No DELETE.
func TestQuestionAgentSuccess(t *testing.T) {
	h, fa := newQ(t)
	fa.answer = func(string) (int, string) {
		return 200, "30 g almonds = 174 kcal, 6.3 g protein.\nAfter it 42 g protein are left.\nA plan logs nothing."
	}
	id := cid()
	rec := h.logText(id, "Plan is to eat 30 g almonds, what do I get")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decode[LogResponse](t, rec)
	tb := textBlocks(r.Blocks)
	if r.Intent != "question" || r.Status != StatusDone || len(r.Items) != 0 || len(tb) != 1 ||
		!strings.Contains(tb[0], "174 kcal") || !strings.Contains(tb[0], "A plan logs nothing.") {
		t.Fatalf("blocks: %+v", r.Blocks)
	}
	if last := r.Blocks[len(r.Blocks)-1]; last.Type != "widget" || last.Widget != "macros_today" {
		t.Errorf("the widget is not the last block: %+v", r.Blocks)
	}
	if strings.Contains(rec.Body.String(), "Beans and tofu") {
		t.Error("the model text is in an agent answer")
	}
	if h.vars.posts != 0 {
		t.Errorf("a question wrote %d row(s)", h.vars.posts)
	}
	creates, turns, _ := fa.counts()
	if creates != 1 || turns != 1 || fa.deletes != 0 {
		t.Fatalf("creates %d turns %d deletes %d", creates, turns, fa.deletes)
	}
	if c := fa.creates[0]; c["title"] != "Fuel questions 2026-10-01" || c["model"] != "claude-opus-5-5" {
		t.Errorf("session: %v", c)
	}
	task := fa.turns[0]
	for _, want := range []string{"READ-ONLY", "Write NOTHING", "no post_turn", clinicalLine, "---- DATA ----", "SNAPSHOT of 2026-10-01",
		`"key":"protein_g"`, `"left":`, "WEEK BUDGETS", "TARGETS SUMMARY", "CHAT TODAY", "<<<\nPlan is to eat 30 g almonds, what do I get\n>>>"} {
		if !strings.Contains(task, want) {
			t.Errorf("the message lacks %q", want)
		}
	}
	if strings.Contains(task, agentTok) || strings.Contains(task, testToken) || strings.Contains(task, "vkey") {
		t.Error("a secret is in the message")
	}
	for _, a := range fa.auth {
		if a != "Bearer "+agentTok {
			t.Errorf("auth header %q", a)
		}
	}
	// The feed holds the user line and the answer; the entry is done.
	feed := h.get("/fuel/feed").Body.String()
	if !strings.Contains(feed, "Plan is to eat 30 g almonds") || !strings.Contains(feed, "174 kcal") {
		t.Errorf("feed: %s", feed)
	}
	if e := h.get("/fuel/entry/" + r.EntryID); e.Code != 200 {
		t.Errorf("entry: %d", e.Code)
	}
	// A replay answers the stored bytes and asks nobody.
	calls := h.model.calls
	again := h.logText(id, "Plan is to eat 30 g almonds, what do I get")
	if again.Code != 200 || !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("replay differs: %d %s", again.Code, again.Body)
	}
	if _, turns, tried := fa.counts(); turns != 1 || tried != 1 || h.model.calls != calls {
		t.Errorf("a replay ran a turn (%d) or a model call", turns)
	}
	// After a restart the stored answer is the same; no new turn.
	h.restart()
	if got := h.logText(id, "Plan is to eat 30 g almonds, what do I get"); !bytes.Equal(got.Body.Bytes(), rec.Body.Bytes()) {
		t.Errorf("replay after a restart differs: %s", got.Body)
	}
	if _, turns, _ := fa.counts(); turns != 1 || h.vars.posts != 0 {
		t.Errorf("turns %d posts %d after the restart", turns, h.vars.posts)
	}
}

// An empty classifier answer (a meta question) is still a question for the
// agent and gets a real answer.
func TestQuestionAgentEmptyClassifierAnswer(t *testing.T) {
	h, fa := newQ(t)
	h.model.fn = func(ModelInput) string { return modelEmptyQuestion }
	r := decode[LogResponse](t, h.logText(cid(), "Are you opus?"))
	tb := textBlocks(r.Blocks)
	if len(tb) != 1 || !strings.Contains(tb[0], "Opus 5.5") {
		t.Fatalf("blocks: %+v", tb)
	}
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Errorf("turns %d", turns)
	}
}

// One session per local date: reused on the same date, a new one on the
// next date. The session id survives a restart.
func TestQuestionAgentSessionPerDate(t *testing.T) {
	h, fa := newQ(t)
	h.logText(cid(), "q one?")
	h.logText(cid(), "q two?")
	if c, turns, _ := fa.counts(); c != 1 || turns != 2 {
		t.Fatalf("same date: creates %d turns %d", c, turns)
	}
	h.restart()
	h.logText(cid(), "q three?")
	if c, turns, _ := fa.counts(); c != 1 || turns != 3 {
		t.Fatalf("after a restart: creates %d turns %d", c, turns)
	}
	h.clk.Add(24 * time.Hour)
	h.logText(cid(), "q four?")
	c, turns, _ := fa.counts()
	if c != 2 || turns != 4 || fa.creates[1]["title"] != "Fuel questions 2026-10-02" || fa.deletes != 0 {
		t.Fatalf("next date: creates %d turns %d %v deletes %d", c, turns, fa.creates, fa.deletes)
	}
}

// A session that is gone (404, the turn did not run) is replaced once and
// the question is answered by the new session: exactly one turn ran.
func TestQuestionAgentSessionGone(t *testing.T) {
	h, fa := newQ(t)
	h.logText(cid(), "q one?")
	fa.mu.Lock()
	fa.sessions = map[string]bool{} // the daemon restarted
	fa.mu.Unlock()
	r := decode[LogResponse](t, h.logText(cid(), "q two?"))
	tb := textBlocks(r.Blocks)
	c, turns, tried := fa.counts()
	if c != 2 || turns != 2 || tried != 3 || len(tb) != 1 || !strings.Contains(tb[0], "Opus 5.5") {
		t.Fatalf("creates %d turns %d tried %d blocks %+v", c, turns, tried, tb)
	}
}

// An agent error is a 200 with the model text and the fallback line. The
// turn is not sent again. The next question starts a new session.
func TestQuestionAgentErrorFallsBack(t *testing.T) {
	h, fa := newQ(t)
	fa.answer = func(string) (int, string) { return 500, "" }
	id := cid()
	rec := h.logText(id, "what should I eat tonight?")
	r := decode[LogResponse](t, rec)
	tb := textBlocks(r.Blocks)
	if rec.Code != 200 || r.Status != StatusDone || len(tb) != 2 || tb[0] != "Beans and tofu are good sources." || tb[1] != questionFallbackLine {
		t.Fatalf("%d blocks %+v", rec.Code, tb)
	}
	if c, turns, tried := fa.counts(); c != 1 || turns != 1 || tried != 1 {
		t.Fatalf("a failed turn was sent again: creates %d turns %d tried %d", c, turns, tried)
	}
	if again := h.logText(id, "what should I eat tonight?"); !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Error("the replay of a fallback differs")
	}
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Error("the replay of a fallback ran a turn")
	}
	feed := h.get("/fuel/feed").Body.String()
	if !strings.Contains(feed, "what should I eat tonight?") || !strings.Contains(feed, questionFallbackLine) {
		t.Errorf("feed: %s", feed)
	}
	fa.answer = func(string) (int, string) { return 200, "fine" }
	h.logText(cid(), "and now?")
	if c, _, _ := fa.counts(); c != 2 {
		t.Errorf("no new session after an error (creates %d)", c)
	}
	// An empty answer and an unreachable daemon fall back too.
	fa.answer = func(string) (int, string) { return 200, "  \n " }
	c0, _, _ := fa.counts()
	if tb := textBlocks(decode[LogResponse](t, h.logText(cid(), "empty?")).Blocks); tb[len(tb)-1] != questionFallbackLine {
		t.Errorf("empty answer: %+v", tb)
	}
	fa.answer = func(string) (int, string) { return 200, "fine" }
	h.logText(cid(), "after the empty one?")
	if c, _, _ := fa.counts(); c != c0+1 {
		t.Errorf("the session of an empty answer was used again (creates %d, before %d)", c, c0)
	}
	fa.srv.Close()
	rec = h.logText(cid(), "anyone there?")
	if tb := textBlocks(decode[LogResponse](t, rec).Blocks); rec.Code != 200 || tb[len(tb)-1] != questionFallbackLine {
		t.Errorf("daemon down: %d %+v", rec.Code, tb)
	}
	if h.vars.posts != 0 {
		t.Errorf("a failed question wrote %d row(s)", h.vars.posts)
	}
}

// A turn over question_agent_timeout: the fallback, the running turn is
// interrupted, nothing is sent again.
func TestQuestionAgentTimeout(t *testing.T) {
	h, fa := newQ(t, func(o *Options) { o.Question.Timeout = 80 * time.Millisecond })
	fa.delay = 2 * time.Second
	rec := h.logText(cid(), "slow one?")
	tb := textBlocks(decode[LogResponse](t, rec).Blocks)
	if rec.Code != 200 || len(tb) != 2 || tb[1] != questionFallbackLine {
		t.Fatalf("%d %+v", rec.Code, tb)
	}
	waitFor(t, "the interrupt", func() bool { fa.mu.Lock(); defer fa.mu.Unlock(); return fa.interrupt == 1 })
	if _, _, tried := fa.counts(); tried != 1 || fa.deletes != 0 || h.vars.posts != 0 {
		t.Errorf("tried %d deletes %d posts %d", tried, fa.deletes, h.vars.posts)
	}
}

// A turn longer than the wait of the request: 202 pending with the
// placeholder; a replay while it runs is 202 and asks nobody; then the
// entry is done, the feed has the answer and the replay answers 200.
func TestQuestionAgentPendingThenDone(t *testing.T) {
	h, fa := newQ(t, func(o *Options) { o.Question.SyncWait = 40 * time.Millisecond })
	fa.delay = 400 * time.Millisecond
	id := cid()
	rec := h.logText(id, "long lookup?")
	r := decode[LogResponse](t, rec)
	tb := textBlocks(r.Blocks)
	if rec.Code != 202 || r.Status != StatusPending || r.Intent != "question" || len(tb) != 1 || tb[0] != questionPendingText {
		t.Fatalf("%d %+v", rec.Code, r.Blocks)
	}
	if e := h.get("/fuel/entry/" + r.EntryID); e.Code != 202 {
		t.Errorf("entry while pending: %d", e.Code)
	}
	if again := h.logText(id, "long lookup?"); again.Code != 202 {
		t.Errorf("replay while pending: %d %s", again.Code, again.Body)
	}
	if strings.Contains(h.get("/fuel/feed").Body.String(), "long lookup?") {
		t.Error("the feed has lines of a pending question")
	}
	// A second request with the client_id, sent while the first still waits
	// in its handler, replays the pending state at once (no wait, no 503).
	waitFor(t, "the entry", func() bool { return h.get("/fuel/entry/"+r.EntryID).Code == 200 })
	if eb := h.get("/fuel/entry/" + r.EntryID).Body.String(); !strings.Contains(eb, "Opus 5.5") {
		t.Errorf("the polled entry lacks the answer: %s", eb)
	}
	waitFor(t, "the feed", func() bool { return strings.Contains(h.get("/fuel/feed").Body.String(), "Opus 5.5") })
	final := h.logText(id, "long lookup?")
	ftb := textBlocks(decode[LogResponse](t, final).Blocks)
	if final.Code != 200 || len(ftb) != 1 || !strings.Contains(ftb[0], "Opus 5.5") {
		t.Fatalf("final replay: %d %+v", final.Code, ftb)
	}
	if feed := h.get("/fuel/feed").Body.String(); strings.Count(feed, "long lookup?") != 1 || strings.Contains(feed, questionPendingText) {
		t.Errorf("feed: %s", feed)
	}
	if c, turns, tried := fa.counts(); c != 1 || turns != 1 || tried != 1 || h.vars.posts != 0 {
		t.Errorf("creates %d turns %d tried %d posts %d", c, turns, tried, h.vars.posts)
	}
}

// A stop while the turn runs: the message is not lost, the agent is not
// asked again, the answer after the restart is the fallback.
func TestQuestionAgentRestartWhilePending(t *testing.T) {
	h, fa := newQ(t, func(o *Options) { o.Question.SyncWait = 30 * time.Millisecond })
	fa.delay = 5 * time.Second
	id := cid()
	rec := h.logText(id, "interrupted question?")
	if rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decode[LogResponse](t, rec)
	h.restart()
	fa.mu.Lock()
	fa.delay = 0
	fa.mu.Unlock()
	if e := h.get("/fuel/entry/" + r.EntryID); e.Code != 200 {
		t.Fatalf("entry after the restart: %d %s", e.Code, e.Body)
	}
	final := h.logText(id, "interrupted question?")
	tb := textBlocks(decode[LogResponse](t, final).Blocks)
	if final.Code != 200 || tb[len(tb)-1] != questionFallbackLine {
		t.Fatalf("after the restart: %d %+v", final.Code, tb)
	}
	feed := h.get("/fuel/feed").Body.String()
	if strings.Count(feed, "interrupted question?") != 1 || !strings.Contains(feed, questionFallbackLine) {
		t.Errorf("feed: %s", feed)
	}
	if _, _, tried := fa.counts(); tried != 1 || h.vars.posts != 0 {
		t.Errorf("tried %d posts %d", tried, h.vars.posts)
	}
	// The session of the interrupted turn was stopped and is not used again.
	fa.mu.Lock()
	ints := fa.interrupt
	fa.mu.Unlock()
	h.logText(cid(), "next question?")
	if c, turns, _ := fa.counts(); ints < 1 || c != 2 || turns != 2 {
		t.Errorf("interrupts %d creates %d turns %d", ints, c, turns)
	}
}

// A turn that ended without a journaled answer (a storage failure) does
// not stay pending: the next recovery pass makes it a fallback.
func TestQuestionAgentSweepOrphan(t *testing.T) {
	h, fa := newQ(t)
	r := decode[LogResponse](t, h.logText(cid(), "orphan?"))
	e, _ := h.svc.journal.Entry(r.EntryID)
	e.ID, e.ClientID, e.Agent, e.AgentText = "en_orphan", "c0ffee00-0001", agentPending, ""
	if err := h.svc.journal.Append(journalRec{T: "txn", Entry: &e}); err != nil {
		t.Fatal(err)
	}
	h.svc.materializeFeed(context.Background(), 0*time.Second+time.Nanosecond) // not a start: too young
	if cur, _ := h.svc.journal.Entry("en_orphan"); cur.Agent != agentPending {
		t.Fatal("a young pending entry was swept")
	}
	h.clk.Add(10 * time.Minute)
	h.svc.materializeFeed(context.Background(), time.Nanosecond)
	if cur, _ := h.svc.journal.Entry("en_orphan"); cur.Agent != agentFallback {
		t.Fatalf("the orphan stayed %q", cur.Agent)
	}
	if rec := h.get("/fuel/entry/en_orphan"); rec.Code != 200 {
		t.Errorf("entry: %d", rec.Code)
	}
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Errorf("the sweep asked the agent (%d turns)", turns)
	}
}

// The kill switch file and the default backend keep the model answer: no
// agent call, no fallback line, the reply of today.
func TestQuestionAgentOffKeepsModel(t *testing.T) {
	h, fa := newQ(t)
	off := filepath.Join(h.opts.StateDir, "question-agent.off")
	if err := os.WriteFile(off, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := h.logText(cid(), "how am I doing?")
	tb := textBlocks(decode[LogResponse](t, rec).Blocks)
	if rec.Code != 200 || len(tb) != 1 || tb[0] != "Beans and tofu are good sources." {
		t.Fatalf("kill switch: %d %+v", rec.Code, tb)
	}
	if c, _, tried := fa.counts(); c != 0 || tried != 0 {
		t.Fatalf("the agent was called with the kill switch set")
	}
	if strings.Contains(stateFile(h, "journal.jsonl"), `"agent"`) {
		t.Error("a model question carries an agent state")
	}
	_ = os.Remove(off)
	h.logText(cid(), "and now?")
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Errorf("the agent was not called after the kill switch was removed")
	}
	// The default backend: the same reply as before this feature.
	fa2 := newQAgentd(t)
	h2 := newHarness(t, func(o *Options) {
		o.Question.Agent = &AgentdClient{Base: fa2.srv.URL, Token: agentTok, Client: loopbackClient()}
	})
	h2.model.fn = func(ModelInput) string { return modelQuestion }
	tb = textBlocks(decode[LogResponse](t, h2.logText(cid(), "how am I doing?")).Blocks)
	if len(tb) != 1 || tb[0] != "Beans and tofu are good sources." {
		t.Errorf("default backend: %+v", tb)
	}
	if c, _, tried := fa2.counts(); c != 0 || tried != 0 {
		t.Error("the default backend called the agent")
	}
}

// The clinical guard wins: the agent is never asked, the fixed line is the
// only text, also when only the words (not the classifier) say so.
func TestQuestionAgentClinicalGuardWins(t *testing.T) {
	h, fa := newQ(t)
	h.model.fn = func(ModelInput) string { return clinicalOut("question", "") }
	r := decode[LogResponse](t, h.logText(cid(), "my chest hurt on the run today"))
	if tb := textBlocks(r.Blocks); len(r.Blocks) != 1 || tb[0] != clinicalLine {
		t.Fatalf("classifier guard: %+v", r.Blocks)
	}
	h.model.fn = func(ModelInput) string { return modelQuestion }
	for _, q := range []string{"Is salt bad for my blood pressure?", "what should I eat for my aortic valve", "Blutdruck und Kaffee?", "food for the aorta"} {
		r := decode[LogResponse](t, h.logText(cid(), q))
		if tb := textBlocks(r.Blocks); len(r.Blocks) != 1 || tb[0] != clinicalLine {
			t.Errorf("%q: %+v", q, r.Blocks)
		}
	}
	if c, _, tried := fa.counts(); c != 0 || tried != 0 {
		t.Fatalf("the agent was asked a clinical question (%d turns)", tried)
	}
	// A clinical turn of the day is not in the history of a later question.
	h.logText(cid(), "what nuts are best?")
	if _, turns, _ := fa.counts(); turns != 1 {
		t.Fatal("the food question did not reach the agent")
	}
	for _, bad := range []string{"chest hurt", "blood pressure", "aort", "Blutdruck", "systolic", "clinician"} {
		if i := strings.Index(fa.turns[0], "---- DATA ----"); strings.Contains(fa.turns[0][i:], bad) {
			t.Errorf("the message data holds %q", bad)
		}
	}
	if h.vars.posts != 0 {
		t.Errorf("posts %d", h.vars.posts)
	}
}

// Only a question goes to the agent: a log, a correction and a question
// with a photo stay on the model path, and a log writes its rows as before.
func TestQuestionAgentOnlyQuestions(t *testing.T) {
	h, fa := newQ(t)
	h.model.fn = func(ModelInput) string { return skyrWalnuts }
	r := decode[LogResponse](t, h.logText(cid(), "250 g skyr and 30 g walnuts"))
	if r.Intent != "log" || len(r.Items) != 2 || len(h.vars.rows("var-food")) != 2 {
		t.Fatalf("log: %+v", r)
	}
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[` + corrForm("last", `"portion_g":20`) + `],"targets":[],"text":"","widgets":[]}`
	}
	if rc := decode[LogResponse](t, h.logText(cid(), "that was 20 g")); rc.Intent != "correct" {
		t.Fatalf("correct: %+v", rc)
	}
	rows := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return modelQuestion }
	body, ct := multipartBody(t, map[string]string{"client_id": cid(), "text": "is this healthy?"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(300, 200)}})
	rp := decode[LogResponse](t, h.do("POST", "/fuel/log", body, ct))
	if tb := textBlocks(rp.Blocks); rp.Intent != "question" || len(tb) != 1 || tb[0] != "Beans and tofu are good sources." {
		t.Fatalf("photo question: %+v", rp.Blocks)
	}
	if c, _, tried := fa.counts(); c != 0 || tried != 0 {
		t.Fatalf("the agent was called for a log, a correction or a photo question")
	}
	// A question after them: answered by the agent, with the day's items in
	// the message, and no row more.
	h.logText(cid(), "how is my day?")
	if _, turns, _ := fa.counts(); turns != 1 || !strings.Contains(fa.turns[0], `"item":"skyr"`) || !strings.Contains(fa.turns[0], "250 g skyr and 30 g walnuts") {
		t.Fatalf("the message lacks the day's items or the chat")
	}
	if len(h.vars.rows("var-food")) != rows {
		t.Errorf("a question changed the rows")
	}
}

// No log line holds the question, the answer or a token.
func TestQuestionAgentLogsNoContent(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)
	h, fa := newQ(t)
	fa.answer = func(string) (int, string) { return 200, "the-answer-words" }
	h.logText(cid(), "the-question-words?")
	fa.answer = func(string) (int, string) { return 500, "" }
	h.logText(cid(), "the-question-words again?")
	out := buf.String()
	for _, bad := range []string{"the-question-words", "the-answer-words", agentTok, testToken, "Bearer"} {
		if strings.Contains(out, bad) {
			t.Errorf("a log line holds %q", bad)
		}
	}
	if !strings.Contains(out, "agent=ok") || !strings.Contains(out, "agent=HTTP 500") {
		t.Errorf("the outcome lines are missing: %s", out)
	}
}

func TestCleanAnswer(t *testing.T) {
	got := cleanAnswer("  **Yes** — it is.\r\n\n\n`food-log` stays.  \n")
	if got != "Yes, it is.\n\nfood-log stays." {
		t.Errorf("%q", got)
	}
	if n := len([]rune(cleanAnswer(strings.Repeat("a", 5000)))); n > questionMaxRunes+4 {
		t.Errorf("not bounded: %d", n)
	}
}

func TestQuestionConfig(t *testing.T) {
	base := "token = \"t\"\nvariables_key = \"k\"\nmodel_key = \"m\"\n"
	c, err := ParseDaemonConfig([]byte(base))
	if err != nil || c.QuestionBackend != "model" || c.QuestionAgentTimeout != "120s" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if _, err := ParseDaemonConfig([]byte(base + "question_backend = \"agent\"\n")); err == nil {
		t.Error("agent without a token was accepted")
	}
	c, err = ParseDaemonConfig([]byte(base + "question_backend = \"agent\"\nrecalibrate_agentd_token = \"env:FUEL_AGENTD_TOKEN\"\nquestion_agent_timeout = \"90s\"\n"))
	if err != nil || c.QuestionBackend != "agent" || c.QuestionAgentTimeout != "90s" || c.QuestionAgentModel != "claude-opus-5-5" {
		t.Errorf("agent: %+v %v", c, err)
	}
	for _, bad := range []string{"question_backend = \"codex\"\n", "question_agent_timeout = \"0s\"\n", "question_agent_timeout = \"soon\"\n", "question_agent_timeout = \"10m\"\n"} {
		if _, err := ParseDaemonConfig([]byte(base + bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A repeat of the client_id while the first request still waits for the
// agent does not wait for it: it replays the pending state (202).
func TestQuestionAgentRepeatDuringWait(t *testing.T) {
	h, fa := newQ(t)
	fa.delay = 700 * time.Millisecond
	id := cid()
	first := make(chan int, 1)
	go func() { first <- h.logText(id, "slow and repeated?").Code }()
	waitFor(t, "the turn", func() bool { _, turns, _ := fa.counts(); return turns == 1 })
	t0 := time.Now()
	if rec := h.logText(id, "slow and repeated?"); rec.Code != 202 || time.Since(t0) > 400*time.Millisecond {
		t.Errorf("repeat during the wait: %d after %s", rec.Code, time.Since(t0))
	}
	if c := <-first; c != 200 {
		t.Errorf("first: %d", c)
	}
	if _, turns, tried := fa.counts(); turns != 1 || tried != 1 {
		t.Errorf("turns %d tried %d", turns, tried)
	}
}
