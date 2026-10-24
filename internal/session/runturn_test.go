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

// echoAdapter answers every turn with "re:<text>". A turn whose text is in
// gates waits for that gate before it answers, so a test can hold one turn
// in flight while another queues behind it.
type echoAdapter struct {
	mu          sync.Mutex
	gates       map[string]chan struct{}
	started     []harness.SessionConfig
	interrupted int
	sends       []string
	// keepOpen leaves a torn-down handle's event channel open, as a pump of
	// a replaced process that still drains would see it.
	keepOpen bool
}

func newEchoAdapter() *echoAdapter { return &echoAdapter{gates: map[string]chan struct{}{}} }

func (a *echoAdapter) gate(text string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := make(chan struct{})
	a.gates[text] = g
	return g
}

func (a *echoAdapter) Name() string                       { return "echo" }
func (a *echoAdapter) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (a *echoAdapter) Interrupt(h harness.Handle) error {
	a.mu.Lock()
	a.interrupted++
	a.mu.Unlock()
	return nil
}
func (a *echoAdapter) Status(harness.Handle) harness.Status { return harness.StatusIdle }
func (a *echoAdapter) Pressure(harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (a *echoAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	a.mu.Lock()
	a.started = append(a.started, cfg)
	a.mu.Unlock()
	return &scriptedHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 64)}, nil
}
func (a *echoAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &scriptedHandle{id: existing, events: make(chan eventbus.Event, 64)}, nil
}
func (a *echoAdapter) Teardown(h harness.Handle) error {
	sh := h.(*scriptedHandle)
	a.mu.Lock()
	defer a.mu.Unlock()
	if sh.events != nil && !a.keepOpen {
		close(sh.events)
		sh.events = nil
	}
	if a.keepOpen {
		sh.dead = true
	}
	return nil
}
func (a *echoAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return h.(*scriptedHandle).events
}
func (a *echoAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	sh := h.(*scriptedHandle)
	a.mu.Lock()
	g := a.gates[in.Text]
	a.sends = append(a.sends, in.Text)
	a.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.mu.Lock()
	out := sh.events
	a.mu.Unlock()
	if out != nil {
		out <- eventbus.Event{SessionID: sh.id, Kind: eventbus.KindResult, Text: "re:" + in.Text}
	}
	return nil
}

// TestQueuedTurnCollectsItsOwnResult is the D1 regression. A turn queued
// behind another one on the same session (Telegram and the wrist at the same
// time) used to subscribe to the bus before it owned the session and filter
// only by session id, so it collected the EARLIER turn's result. Its queue
// time also counted against its own quiet timeout.
func TestQueuedTurnCollectsItsOwnResult(t *testing.T) {
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{}
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	if err != nil {
		t.Fatal(err)
	}
	gateA := a.gate("A")

	resA := make(chan string, 1)
	go func() {
		r, err := mgr.SendAndCollect(context.Background(), s.ID, "A", TurnOptions{Timeout: 5 * time.Second})
		if err != nil {
			t.Errorf("A: %v", err)
		}
		resA <- r
	}()
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.sends) == 1 })

	resB := make(chan string, 1)
	errB := make(chan error, 1)
	go func() {
		// B's quiet window is shorter than the time it waits in the queue:
		// queue time must not count against it.
		r, err := mgr.SendAndCollect(context.Background(), s.ID, "B", TurnOptions{Timeout: 300 * time.Millisecond})
		errB <- err
		resB <- r
	}()
	time.Sleep(500 * time.Millisecond)
	close(gateA)

	if got := <-resA; got != "re:A" {
		t.Fatalf("A collected %q, want re:A", got)
	}
	if err := <-errB; err != nil {
		t.Fatalf("B failed: %v (queue time counted against its timeout)", err)
	}
	if got := <-resB; got != "re:B" {
		t.Fatalf("B collected %q, want re:B (it took the earlier turn's result)", got)
	}
}

// TestAbandonedTurnIsInterruptedBeforeRelease: a turn that times out is
// interrupted and its process replaced BEFORE the next turn may run, so the
// next turn never shares a process with the abandoned one.
func TestAbandonedTurnIsInterruptedBeforeRelease(t *testing.T) {
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{}
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	if err != nil {
		t.Fatal(err)
	}
	_ = a.gate("hang") // never released

	errA := make(chan error, 1)
	go func() {
		_, err := mgr.SendAndCollect(context.Background(), s.ID, "hang", TurnOptions{Timeout: 100 * time.Millisecond})
		errA <- err
	}()
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.sends) == 1 })
	got, err := mgr.SendAndCollect(context.Background(), s.ID, "next", TurnOptions{Timeout: 5 * time.Second})
	if err != nil || got != "re:next" {
		t.Fatalf("next turn = %q, %v", got, err)
	}
	if e := <-errA; !errors.Is(e, ErrTurnTimeout) {
		t.Fatalf("hung turn error = %v, want a timeout", e)
	}
	a.mu.Lock()
	interrupted, starts := a.interrupted, len(a.started)
	a.mu.Unlock()
	if interrupted != 1 {
		t.Fatalf("interrupts = %d, want 1", interrupted)
	}
	if starts != 2 {
		t.Fatalf("process starts = %d, want 2 (the abandoned process is replaced)", starts)
	}
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	if gen != 1 {
		t.Fatalf("generation = %d, want 1", gen)
	}
}

// TestOldGenerationEventsAreNotCollected: an event from the pump of a
// replaced process carries no turn id, so a running turn ignores it.
func TestOldGenerationEventsAreNotCollected(t *testing.T) {
	a := newEchoAdapter()
	bus := eventbus.New()
	mgr := NewManager(a, bus, nil)
	mgr.Policy = Policy{}
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	oldHandle := s.handle.(*scriptedHandle)
	s.mu.Unlock()
	// Replace the process but keep the old event channel open, as a pump
	// that still drains would.
	a.mu.Lock()
	oldEvents := oldHandle.events
	a.keepOpen = true
	a.mu.Unlock()
	unlock, _ := mgr.lockSession(context.Background(), s)
	if err := mgr.replaceProcessLocked(context.Background(), s, "test"); err != nil {
		t.Fatal(err)
	}
	unlock()
	gate := a.gate("q")
	res := make(chan string, 1)
	var seenMu sync.Mutex
	var seen []string
	go func() {
		r, _ := mgr.SendAndCollect(context.Background(), s.ID, "q", TurnOptions{Timeout: 5 * time.Second, OnEvent: func(e eventbus.Event) {
			seenMu.Lock()
			seen = append(seen, e.Text)
			seenMu.Unlock()
		}})
		res <- r
	}()
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.sends) == 1 })
	oldEvents <- eventbus.Event{SessionID: s.ID, Kind: eventbus.KindResult, Text: "stale"}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if got := <-res; got != "re:q" {
		t.Fatalf("turn collected %q, want re:q", got)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	for _, txt := range seen {
		if txt == "stale" {
			t.Fatalf("the turn saw an event of the replaced process: %v", seen)
		}
	}
	close(oldEvents)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var _ = strings.Contains
