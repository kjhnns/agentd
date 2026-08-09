package claudecode

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// TestParseFixture drives the stream-json parser against a recorded transcript
// (testdata/pong.stream-json), captured from a real
// `claude -p --output-format stream-json --verbose "...PONG..."` run. This proves
// the mapping WITHOUT a live model call.
func TestParseFixture(t *testing.T) {
	f, err := os.Open("testdata/pong.stream-json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var got []eventbus.Event
	var lastSession string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, sess := ParseLine(line, "sess-1")
		if sess != "" {
			lastSession = sess
		}
		got = append(got, evs...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if lastSession != "36f3e368-27ef-47b3-82ee-3c05149069a3" {
		t.Errorf("captured session id = %q", lastSession)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Kind != eventbus.KindOutput || got[0].Text != "PONG" {
		t.Errorf("event0 = %+v", got[0])
	}
	if got[1].Kind != eventbus.KindResult || got[1].Text != "PONG" {
		t.Errorf("event1 = %+v", got[1])
	}
	if got[0].Source != eventbus.SourceNative {
		t.Errorf("source = %q, want native", got[0].Source)
	}
}

// TestParseMultiTurnFixture proves the streaming/multi-message case: a persistent
// session emits MANY result events over its lifetime, one per turn. The recorded
// two-turn transcript (testdata/two-turns.stream-json) carries a codeword across
// turns; the parser must surface both turns' results in order.
func TestParseMultiTurnFixture(t *testing.T) {
	f, err := os.Open("testdata/two-turns.stream-json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var results []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, _ := ParseLine(line, "sess-multi")
		for _, e := range evs {
			if e.Kind == eventbus.KindResult {
				results = append(results, e.Text)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 result events (one per turn), got %d: %v", len(results), results)
	}
	if results[0] != "OK" {
		t.Errorf("turn1 result = %q, want OK", results[0])
	}
	if !strings.Contains(results[1], "HELIOTROPE") {
		t.Errorf("turn2 result = %q, want it to contain HELIOTROPE (continuity)", results[1])
	}
}

// TestStreamedFixtureSingleVisibleReply asserts the DEDUPED streaming path (the
// one readLoop/OneShot actually use: ParseLine + resultDeduper): the turn's
// final reply text must reach the event stream EXACTLY ONCE. claude's terminal
// result line repeats the final assistant text; without the deduper every
// channel rendered the reply twice (web UI: identical OUTPUT + RESULT cards).
func TestStreamedFixtureSingleVisibleReply(t *testing.T) {
	f, err := os.Open("testdata/pong.stream-json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var got []eventbus.Event
	dedup := &resultDeduper{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, _ := ParseLine(line, "sess-1")
		got = append(got, dedup.Apply(evs)...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	visible := 0
	var sawResult bool
	for _, e := range got {
		if strings.Contains(e.Text, "PONG") {
			visible++
		}
		if e.Kind == eventbus.KindResult {
			sawResult = true
			if e.Text != "" {
				t.Errorf("result event still carries duplicated text %q, want blank", e.Text)
			}
			if e.Status != string(harness.StatusIdle) {
				t.Errorf("result event lost its status metadata: %+v", e)
			}
		}
	}
	if visible != 1 {
		t.Fatalf("reply text appears in %d events, want exactly 1: %+v", visible, got)
	}
	if !sawResult {
		t.Fatal("turn-terminal result event missing from the stream")
	}
}

// TestStreamedMultiTurnDedup proves the deduper resets at turn boundaries: over
// the recorded two-turn transcript each turn's reply text appears exactly once,
// and each turn still ends with a (text-less) result event.
func TestStreamedMultiTurnDedup(t *testing.T) {
	f, err := os.Open("testdata/two-turns.stream-json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var got []eventbus.Event
	dedup := &resultDeduper{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, _ := ParseLine(line, "sess-multi")
		got = append(got, dedup.Apply(evs)...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	results := 0
	counts := map[string]int{}
	for _, e := range got {
		if e.Kind == eventbus.KindResult {
			results++
			if e.Text != "" {
				t.Errorf("result event still carries duplicated text %q", e.Text)
			}
		}
		if e.Text != "" {
			counts[e.Text]++
		}
	}
	if results != 2 {
		t.Fatalf("want 2 result events (one per turn), got %d", results)
	}
	for text, n := range counts {
		if n != 1 {
			t.Errorf("text %q appears %d times, want 1", text, n)
		}
	}
}

// TestDeduperKeepsDistinctResultText: a result whose text does NOT duplicate the
// streamed output (or an error) keeps its text.
func TestDeduperKeepsDistinctResultText(t *testing.T) {
	d := &resultDeduper{}
	evs := d.Apply([]eventbus.Event{
		{Kind: eventbus.KindOutput, Text: "working on it"},
		{Kind: eventbus.KindResult, Text: "final summary"},
	})
	if evs[1].Text != "final summary" {
		t.Errorf("distinct result text was blanked: %+v", evs[1])
	}
	evs = d.Apply([]eventbus.Event{
		{Kind: eventbus.KindOutput, Text: "boom"},
		{Kind: eventbus.KindError, Text: "boom"},
	})
	if evs[1].Text != "boom" {
		t.Errorf("error text must never be blanked: %+v", evs[1])
	}
}

func TestParseLineToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]},"session_id":"s"}`)
	evs, sess := ParseLine(line, "sess-1")
	if sess != "s" {
		t.Errorf("session = %q", sess)
	}
	if len(evs) != 1 || evs[0].Kind != eventbus.KindToolCall || evs[0].Tool != "Bash" {
		t.Fatalf("tool_use mapping = %+v", evs)
	}
}

func TestParseLineError(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"error","is_error":true,"result":"boom","session_id":"s"}`)
	evs, _ := ParseLine(line, "sess-1")
	if len(evs) != 1 || evs[0].Kind != eventbus.KindError || evs[0].Text != "boom" {
		t.Fatalf("error mapping = %+v", evs)
	}
}

func TestParseLineNonJSON(t *testing.T) {
	evs, _ := ParseLine([]byte("not json at all"), "sess-1")
	if len(evs) != 1 || evs[0].Kind != eventbus.KindOutput {
		t.Fatalf("non-json should map to raw output, got %+v", evs)
	}
}

func TestUserEnvelopeShape(t *testing.T) {
	got := string(userEnvelope("hello there"))
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello there"}]}}`
	if got != want {
		t.Fatalf("envelope = %s\nwant     = %s", got, want)
	}
}

// TestClaudePersistentContinuity is the acceptance centerpiece: it opens ONE
// persistent streaming session and sends TWO turns on it. Turn 1 establishes a
// codeword; turn 2 asks for it back and must get HELIOTROPE, proving the SAME
// process retained context across turns (not a fresh cold start per message).
//
// Skipped unless AGENTD_LIVE_CLAUDE=1 and `claude` is on PATH so `go test ./...`
// does not burn tokens by default. Run manually to capture live evidence.
func TestClaudePersistentContinuity(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_CLAUDE") != "1" {
		t.Skip("set AGENTD_LIVE_CLAUDE=1 to exercise a real persistent 2-turn session")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	a := New("claude")
	h, err := a.Start(ctx, harness.SessionConfig{
		SessionID: "continuity-test", Cwd: os.TempDir(), SkipPermissions: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Teardown(h)

	results := make(chan string, 4)
	go func() {
		var lastOutput string
		for e := range a.Events(h) {
			switch e.Kind {
			case eventbus.KindOutput:
				lastOutput = e.Text
			case eventbus.KindResult:
				// The streaming path blanks a result text that duplicates the
				// turn's final output; the reply is then that output text.
				r := e.Text
				if r == "" {
					r = lastOutput
				}
				results <- r
				lastOutput = ""
			}
		}
	}()

	send := func(text string) string {
		if err := a.Send(ctx, h, harness.Input{Text: text}); err != nil {
			t.Fatalf("Send(%q): %v", text, err)
		}
		select {
		case r := <-results:
			return r
		case <-time.After(150 * time.Second):
			t.Fatalf("no result for %q", text)
			return ""
		}
	}

	r1 := send("Remember the codeword is HELIOTROPE. Reply with just OK.")
	t.Logf("turn 1 result: %q", r1)
	r2 := send("What is the codeword? Reply with just the word.")
	t.Logf("turn 2 result: %q", r2)

	if !strings.Contains(strings.ToUpper(r2), "HELIOTROPE") {
		t.Fatalf("turn 2 did not recall the codeword across turns: %q", r2)
	}
}

// TestIsAuthErrorClassification pins the credential-layer error classifier.
// Regression guard for 2026-08-09: a claude child spawned while the OAuth store
// was tombstoned stayed broken for the whole session lifetime because the turn
// error was returned but the process was kept. Only credential-layer errors may
// discard the process; model/transport errors must NOT, or a transient API blip
// would throw away a live session's continuity.
func TestIsAuthErrorClassification(t *testing.T) {
	authErrors := []string{
		"Failed to authenticate: OAuth session expired and could not be refreshed",
		"Not logged in · Please run /login",
		"claudecode: turn error: Failed to authenticate: OAuth session expired and could not be refreshed",
		"OAuth token has expired",
	}
	for _, s := range authErrors {
		if !isAuthError(s) {
			t.Errorf("expected auth error, got false for %q", s)
		}
	}

	notAuthErrors := []string{
		"API Error: 500 Internal Server Error",
		"Claude Opus is not available with the Claude Pro plan.",
		"API Error: 400 due to tool use concurrency issues.",
		"rate limit exceeded",
		"The socket connection was closed unexpectedly",
		"context deadline exceeded",
		"",
	}
	for _, s := range notAuthErrors {
		if isAuthError(s) {
			t.Errorf("expected NOT an auth error, got true for %q", s)
		}
	}
}

// TestAuthErrorMarksHandleDead proves the recovery contract end to end at the
// adapter boundary: after an auth-class turn error the handle reports
// StatusDead, which is the exact condition session.gcSweep watches to tear the
// process down and restart it fresh (re-reading the credential store).
func TestAuthErrorMarksHandleDead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		errText  string
		wantDead bool
	}{
		{"auth error kills the process", "Failed to authenticate: OAuth session expired and could not be refreshed", true},
		{"model error keeps the process", "API Error: 500 Internal Server Error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &handle{
				id:     "test-session",
				events: make(chan eventbus.Event, 8),
				done:   make(chan struct{}),
				status: harness.StatusIdle,
			}
			// Mirror the Send() error branch without needing a real subprocess.
			if isAuthError(tc.errText) {
				h.markDead()
			}
			gotDead := h.status == harness.StatusDead
			if gotDead != tc.wantDead {
				t.Errorf("status=%v dead=%v, want dead=%v", h.status, gotDead, tc.wantDead)
			}
		})
	}
}

// TestAuthErrorKillsProcessEndToEnd drives the REAL adapter (Start -> Send ->
// error event -> markDead) against a fake `claude` binary that replies to every
// turn with the credential-layer failure the CLI emits. It proves the recovery
// contract without any credential material: after the turn, the handle reports
// StatusDead, which is precisely what session.gcSweep polls to tear the process
// down and restart it fresh. The control case (a model error) must leave the
// process alive so a transient API blip never costs a session its continuity.
func TestAuthErrorKillsProcessEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   string
		wantDead bool
	}{
		{"auth", "Failed to authenticate: OAuth session expired and could not be refreshed", true},
		{"model", "API Error: 500 Internal Server Error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "fake-claude")
			// Reads one turn envelope from stdin, emits one stream-json result
			// event carrying the error text, then waits so the process stays up
			// (exactly like the real persistent session).
			script := "#!/bin/sh\nwhile IFS= read -r _line; do\n" +
				"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"result\":\"" +
				tc.result + "\",\"session_id\":\"s\"}'\ndone\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a := New(bin)
			h, err := a.Start(ctx, harness.SessionConfig{SessionID: "auth-test", Cwd: dir})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer a.Teardown(h)

			// Drain events so the reader loop never blocks on a full channel.
			go func() {
				for range a.Events(h) {
				}
			}()

			err = a.Send(ctx, h, harness.Input{Text: "hello"})
			if err == nil {
				t.Fatal("expected the turn to return an error")
			}
			if !strings.Contains(err.Error(), tc.result) {
				t.Fatalf("turn error = %v, want it to carry %q", err, tc.result)
			}

			gotDead := a.Status(h) == harness.StatusDead
			if gotDead != tc.wantDead {
				t.Fatalf("after %s turn error: status=%v dead=%v, want dead=%v",
					tc.name, a.Status(h), gotDead, tc.wantDead)
			}
		})
	}
}
