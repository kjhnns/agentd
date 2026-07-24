package session

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/runlog"
)

func newHistoryManager(t *testing.T) (*Manager, *runlog.Log) {
	t.Helper()
	rl, err := runlog.Open(filepath.Join(t.TempDir(), "runlog.jsonl"))
	if err != nil {
		t.Fatalf("runlog.Open: %v", err)
	}
	t.Cleanup(func() { rl.Close() })
	return NewManager(nil, eventbus.New(), rl), rl
}

// TestHistoryReplaySingleVisibleReply: replayed history must obey the same
// single-visible-reply rule as the live stream. The log holds a PRE-dedupe-fix
// turn (result text repeats the output text verbatim, as claude's stream-json
// does) and the replay must still surface the reply text exactly once, while
// keeping the result entry as a turn marker.
func TestHistoryReplaySingleVisibleReply(t *testing.T) {
	mgr, rl := newHistoryManager(t)

	must := func(err error) {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	must(rl.Append("input", map[string]string{"session": "s1", "text": "say pong"}))
	must(rl.Append("event", eventbus.Event{SessionID: "s1", Kind: eventbus.KindOutput, Text: "PONG"}))
	// Pre-fix record shape: duplicated final text on the result event.
	must(rl.Append("event", eventbus.Event{SessionID: "s1", Kind: eventbus.KindResult, Text: "PONG", Status: "idle"}))
	// Another session's records must not leak in.
	must(rl.Append("event", eventbus.Event{SessionID: "s2", Kind: eventbus.KindOutput, Text: "other"}))

	hist, err := mgr.History("s1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("got %d entries, want 3 (input, output, result marker): %+v", len(hist), hist)
	}
	if hist[0].Role != "user" || hist[0].Text != "say pong" {
		t.Errorf("entry0 = %+v, want the user input", hist[0])
	}
	visible := 0
	for _, en := range hist {
		if en.Text == "PONG" {
			visible++
		}
	}
	if visible != 1 {
		t.Fatalf("reply text appears %d times in replay, want exactly 1: %+v", visible, hist)
	}
	if hist[2].Kind != "result" || hist[2].Text != "" {
		t.Errorf("entry2 = %+v, want a text-less result turn marker", hist[2])
	}
}

// TestHistoryTurnCap: only the last N turns are replayed (server cap, last-50
// by default; the request can only narrow it).
func TestHistoryTurnCap(t *testing.T) {
	mgr, rl := newHistoryManager(t)
	for i := 0; i < 60; i++ {
		if err := rl.Append("input", map[string]string{"session": "s1", "text": fmt.Sprintf("turn %d", i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := rl.Append("event", eventbus.Event{SessionID: "s1", Kind: eventbus.KindOutput, Text: fmt.Sprintf("reply %d", i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	hist, err := mgr.History("s1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	users := 0
	for _, en := range hist {
		if en.Role == "user" {
			users++
		}
	}
	if users != HistoryMaxTurns {
		t.Fatalf("replayed %d turns, want cap %d", users, HistoryMaxTurns)
	}
	if hist[0].Text != "turn 10" {
		t.Errorf("oldest replayed turn = %q, want %q", hist[0].Text, "turn 10")
	}

	hist2, err := mgr.History("s1", 5)
	if err != nil {
		t.Fatalf("History(5): %v", err)
	}
	users = 0
	for _, en := range hist2 {
		if en.Role == "user" {
			users++
		}
	}
	if users != 5 {
		t.Fatalf("replayed %d turns with turns=5, want 5", users)
	}
}

// TestHistoryEmpty: no log wired, or a session with nothing recorded, yields an
// empty (non-nil) history and no error.
func TestHistoryEmpty(t *testing.T) {
	mgr := NewManager(nil, eventbus.New(), nil)
	hist, err := mgr.History("nope", 0)
	if err != nil || hist == nil || len(hist) != 0 {
		t.Fatalf("no-log history = (%v, %v), want empty slice, nil error", hist, err)
	}

	mgr2, _ := newHistoryManager(t)
	hist2, err := mgr2.History("nope", 0)
	if err != nil || hist2 == nil || len(hist2) != 0 {
		t.Fatalf("unknown-session history = (%v, %v), want empty slice, nil error", hist2, err)
	}
}
