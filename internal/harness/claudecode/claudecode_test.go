package claudecode

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

func harnessCfg() harness.SessionConfig {
	return harness.SessionConfig{SessionID: "live-test", Cwd: os.TempDir()}
}

// TestParseFixture drives the stream-json parser against a recorded transcript
// (testdata/pong.stream-json), captured from a real
// `claude -p --output-format stream-json --verbose "...PONG..."` run on
// 2026-07-21. This proves the mapping WITHOUT a live model call.
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

	// Expect: one output "PONG" then one result "PONG". system/rate_limit/thinking
	// produce nothing.
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

// TestClaudeLiveRoundTrip does a REAL headless round-trip. It is skipped unless
// AGENTD_LIVE_CLAUDE=1 and `claude` is on PATH, so `go test ./...` does not burn
// tokens or hang by default. Run manually to capture live evidence.
func TestClaudeLiveRoundTrip(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_CLAUDE") != "1" {
		t.Skip("set AGENTD_LIVE_CLAUDE=1 to exercise a real claude -p round-trip")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	a := New("claude")
	h, err := a.Start(ctx, harnessCfg())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	evs, err := a.SendPrompt(ctx, h, "Reply with exactly the word PONG and nothing else")
	if err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	var result string
	for _, e := range evs {
		if e.Kind == eventbus.KindResult {
			result = e.Text
		}
	}
	if !strings.Contains(strings.ToUpper(result), "PONG") {
		t.Fatalf("live result did not contain PONG: %q (events: %+v)", result, evs)
	}
	t.Logf("live claude result: %q", result)
}
