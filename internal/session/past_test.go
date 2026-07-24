package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
)

// TestPastSessionsListing: past sessions are enumerated from the run-log with
// label/first/last/turns/preview, and sessions currently LIVE are excluded
// (those are served by GET /sessions).
func TestPastSessionsListing(t *testing.T) {
	mgr, rl := newHistoryManager(t)
	fa := &fakeAdapter{}
	mgr.adapter = fa

	// A real LIVE session (its create is recorded in the same run-log).
	live, err := mgr.Create(context.Background(), "", t.TempDir(), "", "live-web")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A PAST session, synthesized as the daemon would have recorded it.
	must := func(err error) {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	must(rl.Append("session_create", map[string]string{"id": "old1", "harness": "claude-code", "title": "telegram:web", "workspace": "main"}))
	must(rl.Append("input", map[string]string{"session": "old1", "text": "say pong"}))
	must(rl.Append("event", eventbus.Event{SessionID: "old1", Kind: eventbus.KindOutput, Text: "PONG"}))
	must(rl.Append("event", eventbus.Event{SessionID: "old1", Kind: eventbus.KindResult}))
	must(rl.Append("outbound", map[string]string{"session": "old1", "chat": "web", "msg_id": "m1", "text": "PONG"}))

	list, err := mgr.PastSessions(0)
	if err != nil {
		t.Fatalf("PastSessions: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d past sessions, want 1 (live one excluded): %+v", len(list), list)
	}
	p := list[0]
	if p.ID != "old1" || p.Title != "telegram:web" || p.Workspace != "main" || p.Chat != "web" {
		t.Errorf("listing = %+v", p)
	}
	if p.Turns != 1 {
		t.Errorf("turns = %d, want 1", p.Turns)
	}
	if p.Preview != "say pong" {
		t.Errorf("preview = %q, want the first user input", p.Preview)
	}
	if p.FirstTS.IsZero() || p.LastTS.Before(p.FirstTS) {
		t.Errorf("timestamps wrong: first=%v last=%v", p.FirstTS, p.LastTS)
	}
	for _, got := range list {
		if got.ID == live.ID {
			t.Fatalf("live session %s leaked into the past listing", live.ID)
		}
	}

	// Torn-down sessions BECOME past.
	if err := mgr.Teardown(live.ID); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	list2, err := mgr.PastSessions(0)
	if err != nil {
		t.Fatalf("PastSessions after teardown: %v", err)
	}
	found := false
	for _, got := range list2 {
		if got.ID == live.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("torn-down session %s missing from past listing: %+v", live.ID, list2)
	}
}

// TestPastSessionsCapAndOrder: most recent activity first, capped at limit.
func TestPastSessionsCapAndOrder(t *testing.T) {
	mgr, rl := newHistoryManager(t)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("s%d", i)
		if err := rl.Append("input", map[string]string{"session": id, "text": "hello " + id}); err != nil {
			t.Fatalf("append: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // distinct record timestamps
	}
	list, err := mgr.PastSessions(2)
	if err != nil {
		t.Fatalf("PastSessions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d, want cap 2", len(list))
	}
	if list[0].ID != "s4" || list[1].ID != "s3" {
		t.Errorf("order = %s,%s want s4,s3 (most recent first)", list[0].ID, list[1].ID)
	}
}

// TestPastSessionsPreviewFallback: a session with no user input previews its
// last output instead.
func TestPastSessionsPreviewFallback(t *testing.T) {
	mgr, rl := newHistoryManager(t)
	for _, txt := range []string{"first out", "second out"} {
		if err := rl.Append("event", eventbus.Event{SessionID: "j1", Kind: eventbus.KindOutput, Text: txt}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	list, err := mgr.PastSessions(0)
	if err != nil {
		t.Fatalf("PastSessions: %v", err)
	}
	if len(list) != 1 || list[0].Preview != "second out" {
		t.Fatalf("preview fallback = %+v, want last output", list)
	}
}

// TestContinueSeedsNewSession: Continue on a past session starts a NEW live
// session titled continued:<source>, whose FIRST turn carries the compact
// transcript of the past conversation; Continue on a live session refuses.
func TestContinueSeedsNewSession(t *testing.T) {
	mgr, rl := newHistoryManager(t)
	fa := &fakeAdapter{}
	mgr.adapter = fa
	mgr.DefaultCwd = t.TempDir()

	must := func(err error) {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	must(rl.Append("input", map[string]string{"session": "old1", "text": "say pong"}))
	must(rl.Append("event", eventbus.Event{SessionID: "old1", Kind: eventbus.KindOutput, Text: "PONG"}))
	must(rl.Append("event", eventbus.Event{SessionID: "old1", Kind: eventbus.KindResult}))

	s, err := mgr.Continue(context.Background(), "old1")
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if s.Title != "continued:old1" {
		t.Errorf("title = %q, want continued:old1", s.Title)
	}
	if _, live := mgr.Get(s.ID); !live {
		t.Fatalf("continued session %s not in the live list", s.ID)
	}

	// The seed turn runs in the background; wait for the fake harness to see it.
	deadline := time.Now().Add(3 * time.Second)
	var seed string
	for time.Now().Before(deadline) {
		for _, txt := range fa.sentTexts() {
			if strings.Contains(txt, "previous conversation") {
				seed = txt
			}
		}
		if seed != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seed == "" {
		t.Fatalf("seed turn never reached the harness; sends=%v", fa.sentTexts())
	}
	for _, want := range []string{"session old1", "User: say pong", "Agent: PONG"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed missing %q", want)
		}
	}

	// A LIVE session must refuse Continue.
	if _, err := mgr.Continue(context.Background(), s.ID); err == nil {
		t.Fatal("Continue on a live session did not error")
	}
	// An id with no history must refuse too.
	if _, err := mgr.Continue(context.Background(), "ghost"); err == nil {
		t.Fatal("Continue on an unrecorded id did not error")
	}
}
