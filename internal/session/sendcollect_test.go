package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// scriptedAdapter emits a scripted event sequence on each Send. It can emit
// AFTER Send returns (the race SendAndCollect's grace drain exists for) and can
// block forever (the turn-timeout case).
type scriptedAdapter struct {
	mu sync.Mutex
	// emit is called with the handle's event channel; it may emit before
	// returning (in-band) or spawn a goroutine that emits later.
	emit  func(id string, out chan<- eventbus.Event)
	block chan struct{} // when non-nil, Send waits on it before returning
}

type scriptedHandle struct {
	id     string
	events chan eventbus.Event
}

func (h *scriptedHandle) ID() string { return h.id }

func (a *scriptedAdapter) Name() string                       { return "scripted" }
func (a *scriptedAdapter) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (a *scriptedAdapter) Interrupt(harness.Handle) error     { return nil }
func (a *scriptedAdapter) Status(harness.Handle) harness.Status {
	return harness.StatusIdle
}
func (a *scriptedAdapter) Pressure(harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (a *scriptedAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	return &scriptedHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 16)}, nil
}
func (a *scriptedAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &scriptedHandle{id: existing, events: make(chan eventbus.Event, 16)}, nil
}
func (a *scriptedAdapter) Teardown(h harness.Handle) error {
	close(h.(*scriptedHandle).events)
	return nil
}
func (a *scriptedAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*scriptedHandle).events
}
func (a *scriptedAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	sh := h.(*scriptedHandle)
	a.mu.Lock()
	emit, block := a.emit, a.block
	a.mu.Unlock()
	if emit != nil {
		emit(sh.id, sh.events)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

var _ harness.Adapter = (*scriptedAdapter)(nil)

func newScripted(t *testing.T, a *scriptedAdapter) (*Manager, string) {
	t.Helper()
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{} // no lifecycle triggers in these tests
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Teardown(s.ID) })
	return mgr, s.ID
}

// TestSendAndCollectReturnsResultText: the plain case, a result event carrying
// its own text.
func TestSendAndCollectReturnsResultText(t *testing.T) {
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindOutput, Text: "thinking"}
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindResult, Text: "PONG"}
	}}
	mgr, id := newScripted(t, a)

	got, err := mgr.SendAndCollect(context.Background(), id, "ping", TurnOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("SendAndCollect: %v", err)
	}
	if got != "PONG" {
		t.Fatalf("result = %q, want PONG", got)
	}
}

// TestSendAndCollectFallsBackToLastOutput: the harness BLANKS a result whose
// text merely duplicates the turn's final streamed output (one visible reply on
// the bus), so the collected reply must be that last output. This is the rule
// all three former copies of the loop implemented; it is now proven once.
func TestSendAndCollectFallsBackToLastOutput(t *testing.T) {
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindOutput, Text: "first"}
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindOutput, Text: "the real answer"}
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindResult, Text: ""}
	}}
	mgr, id := newScripted(t, a)

	got, err := mgr.SendAndCollect(context.Background(), id, "q", TurnOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("SendAndCollect: %v", err)
	}
	if got != "the real answer" {
		t.Fatalf("result = %q, want the last output text", got)
	}
}

// TestSendAndCollectDrainsResultAfterSendReturns: bus delivery can lag Send
// returning, so a result published a moment later must still be collected
// (the 2s grace drain) rather than yielding an empty reply.
func TestSendAndCollectLateResultIsDrained(t *testing.T) {
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		go func() {
			time.Sleep(120 * time.Millisecond)
			out <- eventbus.Event{SessionID: id, Kind: eventbus.KindResult, Text: "late but delivered"}
		}()
	}}
	mgr, id := newScripted(t, a)

	got, err := mgr.SendAndCollect(context.Background(), id, "q", TurnOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("SendAndCollect: %v", err)
	}
	if got != "late but delivered" {
		t.Fatalf("result = %q, want the late result", got)
	}
}

// TestSendAndCollectTimeout: a turn that outruns its budget fails with an error
// wrapping ErrTurnTimeout (what the channel turns into a plain-language failure
// notice and what the API turns into a 504).
func TestSendAndCollectTimeout(t *testing.T) {
	block := make(chan struct{})
	a := &scriptedAdapter{block: block}
	mgr, id := newScripted(t, a)
	t.Cleanup(func() { close(block) })

	_, err := mgr.SendAndCollect(context.Background(), id, "q", TurnOptions{Timeout: 80 * time.Millisecond})
	if !errors.Is(err, ErrTurnTimeout) {
		t.Fatalf("err = %v, want ErrTurnTimeout", err)
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("timeout error should name the session: %v", err)
	}
}

// TestSendAndCollectZeroTimeoutHonorsContext: Timeout <= 0 means "no bound
// beyond ctx" (what the scheduler runner passes, since a job run is already
// bounded by its own context).
func TestSendAndCollectZeroTimeoutHonorsContext(t *testing.T) {
	block := make(chan struct{})
	a := &scriptedAdapter{block: block}
	mgr, id := newScripted(t, a)
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := mgr.SendAndCollect(ctx, id, "q", TurnOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
}

// TestSendAndCollectOnEventHook: the hook sees every event of the session,
// which is how RouteInbound raises the needs-input glyph and how the CLI
// streams output while the turn runs.
func TestSendAndCollectOnEventHook(t *testing.T) {
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindNeedsInput}
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindOutput, Text: "streamed"}
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindResult, Text: "done"}
	}}
	mgr, id := newScripted(t, a)

	var mu sync.Mutex
	var kinds []eventbus.Kind
	if _, err := mgr.SendAndCollect(context.Background(), id, "q", TurnOptions{
		Timeout: 5 * time.Second,
		OnEvent: func(e eventbus.Event) {
			mu.Lock()
			kinds = append(kinds, e.Kind)
			mu.Unlock()
		},
	}); err != nil {
		t.Fatalf("SendAndCollect: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []eventbus.Kind{eventbus.KindNeedsInput, eventbus.KindOutput, eventbus.KindResult}
	if len(kinds) != len(want) {
		t.Fatalf("hook saw %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("hook saw %v, want %v", kinds, want)
		}
	}
}

// TestSendAndCollectUnknownSession: an unknown id fails fast and by name,
// before any bus subscription or send.
func TestSendAndCollectUnknownSession(t *testing.T) {
	mgr := NewManager(&scriptedAdapter{}, eventbus.New(), nil)
	_, err := mgr.SendAndCollect(context.Background(), "nope", "q", TurnOptions{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

// TestTurnTimeoutAccessor: the exported budget is what callers outside the
// package (the API, and through it the CLI) bound a turn with.
func TestTurnTimeoutAccessor(t *testing.T) {
	mgr := NewManager(&scriptedAdapter{}, eventbus.New(), nil)
	if got := mgr.TurnTimeout(); got != defaultTurnTimeout {
		t.Fatalf("default TurnTimeout = %s, want %s", got, defaultTurnTimeout)
	}
	mgr.Policy.TurnTimeout = 42 * time.Second
	if got := mgr.TurnTimeout(); got != 42*time.Second {
		t.Fatalf("configured TurnTimeout = %s, want 42s", got)
	}
}
