package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/runlog"
)

const sideChat = "watch-side"

// sideManager is a manager with a run-log and one side session.
func sideManager(t *testing.T) (*Manager, *echoAdapter, *Session, *runlog.Log) {
	t.Helper()
	rl, err := runlog.Open(filepath.Join(t.TempDir(), "runlog.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rl.Close() })
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), rl)
	mgr.Policy = Policy{}
	s, err := mgr.CreateInLane(context.Background(), LaneSide, "", t.TempDir(), "", "telegram:"+sideChat)
	if err != nil {
		t.Fatal(err)
	}
	return mgr, a, s, rl
}

func sideTurn(mgr *Manager, s *Session, conv, text string) (TurnResult, error) {
	return mgr.RunTurn(context.Background(), TurnRequest{
		SessionID: s.ID, Text: text, Chat: sideChat, Conv: conv, Side: true,
		Options: TurnOptions{Timeout: 5 * time.Second},
	})
}

func (a *echoAdapter) startedConfigs() []harness.SessionConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]harness.SessionConfig(nil), a.started...)
}

func countRecords(t *testing.T, rl *runlog.Log, typ string) int {
	t.Helper()
	recs, err := runlog.Replay(rl.Path())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.Type == typ {
			n++
		}
	}
	return n
}

// TestTwoQueuedMessagesForANewConversationStartFreshOnce: the fresh-start
// decision is made inside the turn's locks, so the second message for the
// same new conversation finds it active and runs a normal turn.
func TestTwoQueuedMessagesForANewConversationStartFreshOnce(t *testing.T) {
	mgr, a, s, rl := sideManager(t)
	gate := a.gate("first for b")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := sideTurn(mgr, s, "b", "first for b")
		errs <- err
	}()
	waitFor(t, func() bool { return a.sendsContaining("first for b") == 1 })
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := sideTurn(mgr, s, "b", "second for b")
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countRecords(t, rl, "conversation_start"); n != 1 {
		t.Fatalf("conversation_start records = %d, want 1", n)
	}
	// One start at create, one fresh start; the second message did not
	// replace the process again.
	if n := len(a.startedConfigs()); n != 2 {
		t.Fatalf("process starts = %d, want 2", n)
	}
	if got := mgr.activeConversation(sideChat); got != "b" {
		t.Fatalf("active = %q", got)
	}
}

// TestRecapNeverShowsAPreviousSideConversation: a fresh start carries no
// recap, and a later reset inside the new conversation replays only the new
// conversation's turns.
func TestRecapNeverShowsAPreviousSideConversation(t *testing.T) {
	mgr, a, s, _ := sideManager(t)
	if _, err := sideTurn(mgr, s, "a", "alpha question"); err != nil {
		t.Fatal(err)
	}
	if _, err := sideTurn(mgr, s, "a", "alpha follow-up"); err != nil {
		t.Fatal(err)
	}
	if _, err := sideTurn(mgr, s, "b", "bravo question"); err != nil {
		t.Fatal(err)
	}
	starts := a.startedConfigs()
	fresh := starts[len(starts)-1].SystemPrompt
	if strings.Contains(fresh, "alpha") {
		t.Fatalf("the fresh start of b carries a's transcript:\n%s", fresh)
	}
	if err := mgr.Reset(context.Background(), s.ID, "test"); err != nil {
		t.Fatal(err)
	}
	starts = a.startedConfigs()
	after := starts[len(starts)-1].SystemPrompt
	if strings.Contains(after, "alpha") {
		t.Fatalf("the recap after a reset shows the previous conversation:\n%s", after)
	}
	if !strings.Contains(after, "bravo question") {
		t.Fatalf("the recap after a reset lost the current conversation:\n%s", after)
	}
}

// TestResetInsideASideConversationKeepsItsEpoch: a reset (and a recovery)
// replaces the process but is not a new conversation; the epoch and the
// active conversation stay, and the next message runs a normal turn.
func TestResetInsideASideConversationKeepsItsEpoch(t *testing.T) {
	mgr, a, s, rl := sideManager(t)
	if _, err := sideTurn(mgr, s, "a", "alpha question"); err != nil {
		t.Fatal(err)
	}
	epoch := mgr.conversationEpoch(sideChat)
	if epoch != 1 {
		t.Fatalf("epoch = %d after the first conversation", epoch)
	}
	if err := mgr.Reset(context.Background(), s.ID, "test"); err != nil {
		t.Fatal(err)
	}
	unlock, _ := mgr.lockSession(context.Background(), s)
	if err := mgr.replaceProcessLocked(context.Background(), s, "dead"); err != nil {
		t.Fatal(err)
	}
	unlock()
	starts := len(a.startedConfigs())
	if _, err := sideTurn(mgr, s, "a", "alpha again"); err != nil {
		t.Fatal(err)
	}
	if got := mgr.conversationEpoch(sideChat); got != epoch {
		t.Fatalf("epoch moved on a reset: %d -> %d", epoch, got)
	}
	if n := len(a.startedConfigs()); n != starts {
		t.Fatal("a message to the active conversation started a new process")
	}
	if n := countRecords(t, rl, "conversation_start"); n != 1 {
		t.Fatalf("conversation_start records = %d", n)
	}
	// The epoch is persisted: a new manager on the same log reads it.
	mgr2 := NewManager(a, eventbus.New(), rl)
	if mgr2.conversationEpoch(sideChat) != 1 || mgr2.activeConversation(sideChat) != "a" {
		t.Fatalf("persisted epoch/active = %d/%q", mgr2.conversationEpoch(sideChat), mgr2.activeConversation(sideChat))
	}
}

// TestFreshStartPutsOnlyThePointerInTheTurn: the artifact of a continued
// conversation reaches the harness as a pointer line in front of the
// user's text; the turn record keeps the user's text and the attachment,
// and the recap of a later reset lists the artifact in its own section.
func TestFreshStartPutsOnlyThePointerInTheTurn(t *testing.T) {
	mgr, a, s, rl := sideManager(t)
	const pointer = `Attached earlier conversation "Old" (2 messages, x-y): /state/watch-artifacts/c-1.jsonl. Read it when the request needs it.`
	res, err := mgr.RunTurn(context.Background(), TurnRequest{
		SessionID: s.ID, Text: "what did we say", Chat: sideChat, Conv: "c", Side: true,
		Options: TurnOptions{Timeout: 5 * time.Second},
		OnFresh: func() (string, []TurnAttachment, error) {
			return pointer, []TurnAttachment{{Kind: "conv", ID: "c", Path: "/state/watch-artifacts/c-1.jsonl", Name: "Old"}}, nil
		},
	})
	if err != nil || !res.Committed {
		t.Fatalf("turn: %v committed=%v", err, res.Committed)
	}
	a.mu.Lock()
	sent := a.sends[len(a.sends)-1]
	a.mu.Unlock()
	if sent != pointer+"\n\nwhat did we say" {
		t.Fatalf("harness got %q", sent)
	}
	recs, _ := runlog.Replay(rl.Path())
	var tr TurnRecord
	for _, r := range recs {
		if r.Type == "turn" {
			_ = json.Unmarshal(r.Data, &tr)
		}
	}
	if tr.UserText != "what did we say" || len(tr.Attachments) != 1 || tr.Attachments[0].Path == "" || tr.ConversationEpoch != 1 || tr.Conv != "c" {
		t.Fatalf("turn record = %+v", tr)
	}
	if err := mgr.Reset(context.Background(), s.ID, "test"); err != nil {
		t.Fatal(err)
	}
	starts := a.startedConfigs()
	sp := starts[len(starts)-1].SystemPrompt
	if !strings.Contains(sp, "## Attached artifacts") || !strings.Contains(sp, "/state/watch-artifacts/c-1.jsonl") {
		t.Fatalf("recap misses the artifact section:\n%s", sp)
	}
}

// TestFailedFreshStartChangesNothing: when the export for a fresh start
// fails, the turn fails and the old conversation and epoch stay active.
func TestFailedFreshStartChangesNothing(t *testing.T) {
	mgr, a, s, rl := sideManager(t)
	if _, err := sideTurn(mgr, s, "a", "alpha question"); err != nil {
		t.Fatal(err)
	}
	starts := len(a.startedConfigs())
	_, err := mgr.RunTurn(context.Background(), TurnRequest{
		SessionID: s.ID, Text: "to b", Chat: sideChat, Conv: "b", Side: true,
		OnFresh: func() (string, []TurnAttachment, error) { return "", nil, errors.New("disk full") },
	})
	if err == nil {
		t.Fatal("the turn ran without its artifact")
	}
	if mgr.activeConversation(sideChat) != "a" || mgr.conversationEpoch(sideChat) != 1 {
		t.Fatal("a failed start changed the active conversation")
	}
	if len(a.startedConfigs()) != starts || a.sendsContaining("to b") != 0 {
		t.Fatal("a failed start replaced the process or ran the turn")
	}
	if n := countRecords(t, rl, "conversation_start"); n != 1 {
		t.Fatalf("conversation_start records = %d", n)
	}
}

// TestMainTurnsNeverStartAConversation: a main turn (no Side) never
// replaces the process, whatever its conv says.
func TestMainTurnsNeverStartAConversation(t *testing.T) {
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), nil)
	s, err := mgr.CreateInLane(context.Background(), LaneMain, "", t.TempDir(), "", "telegram:1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := mgr.RunTurn(context.Background(), TurnRequest{SessionID: s.ID, Text: "q", Chat: "1", Conv: ConvMain}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.startedConfigs()); n != 1 {
		t.Fatalf("process starts = %d", n)
	}
}

// TestFreshStartArtifactSurvivesACrashBeforeTheTurnRecord: the artifact of
// a fresh start is durable with the epoch, so the recap of a process started
// before any turn record of the epoch exists still points at it.
func TestFreshStartArtifactSurvivesACrashBeforeTheTurnRecord(t *testing.T) {
	path := writeThread(t, []func(*runlog.Log){
		func(l *runlog.Log) {
			_ = l.Append("conversation_start", conversationStart{Chat: sideChat, Conv: "c", Epoch: 4,
				Attachments: []TurnAttachment{{Kind: "conv", ID: "c", Path: "/state/watch-artifacts/c-9.jsonl", Name: "Old"}}})
		},
	})
	got, err := ThreadRecap(path, "telegram:"+sideChat, RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/state/watch-artifacts/c-9.jsonl") {
		t.Fatalf("recap = %q", got)
	}
}

// TestConversationStartCarriesEveryFirstTurnAttachment: a context_conv
// attachment of the first turn is durable with the epoch too.
func TestConversationStartCarriesEveryFirstTurnAttachment(t *testing.T) {
	mgr, _, s, rl := sideManager(t)
	_, err := mgr.RunTurn(context.Background(), TurnRequest{
		SessionID: s.ID, Text: "q", Chat: sideChat, Conv: "c", Side: true,
		Attachments: []TurnAttachment{{Kind: "conv", ID: "other", Path: "/a/other.jsonl"}},
		OnFresh: func() (string, []TurnAttachment, error) {
			return "P", []TurnAttachment{{Kind: "conv", ID: "c", Path: "/a/c.jsonl"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := runlog.Replay(rl.Path())
	for _, r := range recs {
		if r.Type == "conversation_start" {
			var cs conversationStart
			_ = json.Unmarshal(r.Data, &cs)
			if len(cs.Attachments) != 2 {
				t.Fatalf("start attachments = %+v", cs.Attachments)
			}
			return
		}
	}
	t.Fatal("no conversation_start")
}
