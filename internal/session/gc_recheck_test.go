package session

import (
	"context"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// TestGCDoesNotReclaimASessionThatJustTookATurn is the D1b regression: the
// sweep read lastActivity BEFORE it took the lock, so a turn that ran in
// between (and made the session busy again) did not stop the reclaim. The
// sweep now re-checks under the lock.
func TestGCDoesNotReclaimASessionThatJustTookATurn(t *testing.T) {
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{IdleTimeout: 50 * time.Millisecond}
	s, err := mgr.CreateInLane(context.Background(), LaneMain, "", t.TempDir(), "", "telegram:1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // idle
	mgr.gcBeforeLock = func(gs *Session) {
		// A message arrives and its turn completes between the read and the
		// lock.
		if _, err := mgr.SendAndCollect(context.Background(), gs.ID, "hello", TurnOptions{Timeout: time.Second}); err != nil {
			t.Errorf("turn: %v", err)
		}
	}
	mgr.gcSweep(context.Background())
	if _, ok := mgr.Get(s.ID); !ok {
		t.Fatal("the GC reclaimed a session whose turn had just finished")
	}
	if n := a.sendsContaining("CHECKPOINT"); n != 0 {
		t.Fatalf("checkpoint flushes = %d, want 0", n)
	}
}

// deadAdapter reports every handle dead until told otherwise.
type deadAdapter struct {
	*echoAdapter
	dead map[harness.Handle]bool
}

func (d *deadAdapter) Status(h harness.Handle) harness.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead[h] {
		return harness.StatusDead
	}
	return harness.StatusIdle
}

// TestGCDoesNotRecoverTwice: when the process was already replaced between
// the read and the lock (a new generation), the sweep must not tear down the
// fresh process again.
func TestGCDoesNotRecoverTwice(t *testing.T) {
	a := &deadAdapter{echoAdapter: newEchoAdapter(), dead: map[harness.Handle]bool{}}
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{}
	s, err := mgr.CreateInLane(context.Background(), LaneMain, "", t.TempDir(), "", "telegram:1")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	a.mu.Lock()
	a.dead[s.handle] = true
	a.mu.Unlock()
	s.mu.Unlock()
	mgr.gcBeforeLock = func(gs *Session) {
		unlock, _ := mgr.lockSession(context.Background(), gs)
		if err := mgr.replaceProcessLocked(context.Background(), gs, "dead"); err != nil {
			t.Error(err)
		}
		unlock()
	}
	mgr.gcSweep(context.Background())
	a.mu.Lock()
	starts := len(a.started)
	a.mu.Unlock()
	if starts != 2 {
		t.Fatalf("process starts = %d, want 2 (create + one recovery)", starts)
	}
}
