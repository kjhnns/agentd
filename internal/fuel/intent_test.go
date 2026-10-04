package fuel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Spec 22.13: the intent the user marks at the input ("log" | "ask").

func (h *harness) logIntent(clientID, text, intent string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"client_id": clientID, "text": text, "intent_hint": intent})
	return h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json")
}

func TestIntentHintHashAndValidation(t *testing.T) {
	plain := logInput{Text: "two eggs", LocalTime: "2026-10-01T12:00:00+02:00"}
	before := plain.hash()
	marked := plain
	marked.Intent = intentAsk
	logged := plain
	logged.Intent = intentLog
	if marked.hash() == before || logged.hash() == before || marked.hash() == logged.hash() {
		t.Fatal("the intent is not in the hash of a marked request")
	}
	// The hash of an unmarked request is what the builds before the hint
	// computed: text, local_time, the audio hash (zero), no images.
	old := sha256.New()
	fmt.Fprintf(old, "log\x00%s\x00%s\x00", "two eggs", "2026-10-01T12:00:00+02:00")
	old.Write(make([]byte, 32))
	if before != hex.EncodeToString(old.Sum(nil)) {
		t.Fatal("the hash of an unmarked request changed")
	}

	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return modelQuestion }
	if rec := h.logIntent("d0000001-0001", "what now", "question"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "intent_hint") {
		t.Fatalf("bad intent: %d %s", rec.Code, rec.Body)
	}
	// Multipart carries the field too.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("client_id", "d0000001-0002")
	_ = mw.WriteField("text", "what should I eat next?")
	_ = mw.WriteField("intent_hint", "ask")
	mw.Close()
	if rec := h.do("POST", "/fuel/log", &buf, mw.FormDataContentType()); rec.Code != 200 {
		t.Fatalf("multipart ask: %d %s", rec.Code, rec.Body)
	}
	// The same client_id with another intent is another request.
	if rec := h.logIntent("d0000001-0002", "what should I eat next?", "log"); rec.Code != 409 {
		t.Fatalf("same id, other intent: %d %s", rec.Code, rec.Body)
	}
	// The same request again is a replay.
	if rec := h.logIntent("d0000001-0002", "what should I eat next?", "ask"); rec.Code != 200 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(h.get("/fuel/feed").Body.String(), `"intent_hint":"ask"`) {
		t.Error("the user line of the feed has no intent_hint")
	}
}

func TestIntentAskWritesNothingOnTheEstimator(t *testing.T) {
	h := newHarness(t)
	var seen string
	h.model.fn = func(in ModelInput) string {
		seen = in.Text
		// The model ignores the mark and answers with a log.
		return oneItem("Skyr", 250, 160, 27, false)
	}
	h.clk.Add(time.Minute)
	rec := h.logIntent("d0000002-0001", "skyr 250 g", "ask")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decode[LogResponse](t, rec)
	if r.Intent != "question" || len(r.Items) != 0 {
		t.Fatalf("a marked question logged: %+v", r)
	}
	if rows := h.vars.rows("var-food"); len(rows) != 0 {
		t.Fatalf("rows written: %v", rows)
	}
	if !strings.HasPrefix(seen, "QUESTION of the user.") || !strings.Contains(seen, "skyr 250 g") {
		t.Errorf("the model did not get the mark: %q", seen)
	}
	if tb := textBlocks(r.Blocks); len(tb) == 0 || !strings.Contains(tb[0], "nothing was logged") {
		t.Errorf("answer: %q", tb)
	}
	// Unmarked, the same words are a log (the control).
	h.clk.Add(time.Minute)
	if r := decode[LogResponse](t, h.logText("d0000002-0002", "skyr 250 g")); r.Intent != "log" || len(h.vars.rows("var-food")) != 1 {
		t.Fatalf("control: %+v", r)
	}
}

func TestIntentAskTurnOfTheAgentHasNoCapability(t *testing.T) {
	h, fa := newChat(t)
	var msg string
	var openTurns int
	var write int
	fa.script = func(m string) (int, string) {
		msg = m
		h.svc.chat.mu.Lock()
		openTurns = len(h.svc.chat.turns)
		h.svc.chat.mu.Unlock()
		// The agent tries a write anyway, with a made-up capability.
		write = h.op("POST", "/fuel/items", "p_0000000000000000", map[string]any{"client_id": "d0000003-00aa",
			"items": []map[string]any{food("pizza", 400, 900, 30, 12)}}).Code
		return 200, end(m, "Take the miso chicken bowl.")
	}
	rec := h.logIntent(cid(), "Which of these should I eat?", "ask")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decode[LogResponse](t, rec)
	if r.Intent != "question" || len(r.Items) != 0 || len(h.vars.rows("var-food")) != 0 {
		t.Fatalf("response %+v rows %v", r, h.vars.rows("var-food"))
	}
	if capRE.MatchString(msg) || strings.Contains(msg, "--turn") {
		t.Error("a capability or a write instruction is in the message of a question turn")
	}
	if !strings.Contains(msg, "Joe marked this message as a QUESTION") || !strings.Contains(msg, "Which of these should I eat?") {
		t.Errorf("message: %.300s", msg)
	}
	if openTurns != 0 {
		t.Errorf("%d turn(s) open for writes during a question", openTurns)
	}
	if write == 200 || write == 201 {
		t.Errorf("a write in a question turn answered %d", write)
	}
	if tb := textBlocks(r.Blocks); len(tb) != 1 || tb[0] != "Take the miso chicken bowl." {
		t.Errorf("text %q", tb)
	}

	// A marked log: the capability is there, and the line that says so.
	fa.script = func(m string) (int, string) { msg = m; return 200, end(m, "ok") }
	if rec := h.logIntent(cid(), "this", "log"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !capRE.MatchString(msg) || !strings.Contains(msg, "Joe marked this message as a LOG") {
		t.Errorf("log message: %.400s", msg)
	}
	// Unmarked: as before, no line.
	if rec := h.logText(cid(), "an apple"); rec.Code != 200 || !capRE.MatchString(msg) || strings.Contains(msg, "Joe marked") {
		t.Errorf("unmarked message: %.300s", msg)
	}
	// The entry keeps the mark (a restart reads it from the journal).
	n := 0
	for _, e := range h.svc.journal.Entries() {
		if e.IntentHint == intentAsk {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d journal entries with the ask mark", n)
	}
}
