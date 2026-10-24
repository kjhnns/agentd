package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/workspace"
)

// TestFindByWorkspaceSkipsSideAndPrefersMain: the scheduler reuses the warm
// session of a workspace. It must never land in a side session and must pick
// the main one deterministically, not by map order.
func TestFindByWorkspaceSkipsSideAndPrefersMain(t *testing.T) {
	mgr := NewManager(newEchoAdapter(), eventbus.New(), nil)
	mgr.Workspaces = workspace.NewStore(t.TempDir(), "default")
	ctx := context.Background()
	side, err := mgr.CreateInLane(ctx, LaneSide, "default", "", "", "telegram:watch-side")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := mgr.FindByWorkspace(""); ok {
		t.Fatalf("found %s (lane %s) with only a side session", s.ID, s.Lane)
	}
	other, _ := mgr.CreateInLane(ctx, LaneOther, "default", "", "", "job")
	for i := 0; i < 20; i++ {
		if s, ok := mgr.FindByWorkspace(""); !ok || s != other {
			t.Fatalf("want the other session, got %v", s)
		}
	}
	main, _ := mgr.CreateInLane(ctx, LaneMain, "default", "", "", "telegram:1")
	for i := 0; i < 20; i++ {
		if s, ok := mgr.FindByWorkspace(""); !ok || s != main {
			t.Fatalf("want the main session, got %+v", s)
		}
	}
	_ = side
}

// sendsContaining counts the harness sends whose text contains sub.
func (a *echoAdapter) sendsContaining(sub string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.sends {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func twoLanes(t *testing.T) (*Manager, *echoAdapter, *Session, *Session) {
	t.Helper()
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{IdleTimeout: time.Millisecond}
	cwd := t.TempDir()
	ctx := context.Background()
	main, err := mgr.CreateInLane(ctx, LaneMain, "", cwd, "", "telegram:1")
	if err != nil {
		t.Fatal(err)
	}
	side, err := mgr.CreateInLane(ctx, LaneSide, "", cwd, "", "telegram:watch-side")
	if err != nil {
		t.Fatal(err)
	}
	return mgr, a, main, side
}

// TestSideTurnAndMainFlushDoNotOverlap: a reset (checkpoint flush and
// autocommit) of the main session waits for a running side turn in the
// same workspace.
func TestSideTurnAndMainFlushDoNotOverlap(t *testing.T) {
	mgr, a, main, side := twoLanes(t)
	gate := a.gate("side work")
	sideDone := make(chan error, 1)
	go func() {
		_, err := mgr.SendAndCollect(context.Background(), side.ID, "side work", TurnOptions{Timeout: 5 * time.Second})
		sideDone <- err
	}()
	waitFor(t, func() bool { return a.sendsContaining("side work") == 1 })
	resetDone := make(chan error, 1)
	go func() { resetDone <- mgr.Reset(context.Background(), main.ID, "test") }()
	time.Sleep(200 * time.Millisecond)
	if n := a.sendsContaining("CHECKPOINT"); n != 0 {
		t.Fatal("the main flush ran while a side turn was in flight")
	}
	close(gate)
	if err := <-sideDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	if n := a.sendsContaining("CHECKPOINT"); n != 1 {
		t.Fatalf("flushes = %d, want 1 after the side turn", n)
	}
}

// TestSideTurnAndMainIdleReclaimDoNotOverlap: the GC does not reclaim an
// idle main session while a side turn runs in the same workspace; it tries
// again on the next sweep.
func TestSideTurnAndMainIdleReclaimDoNotOverlap(t *testing.T) {
	mgr, a, main, side := twoLanes(t)
	gate := a.gate("side work")
	sideDone := make(chan error, 1)
	go func() {
		_, err := mgr.SendAndCollect(context.Background(), side.ID, "side work", TurnOptions{Timeout: 5 * time.Second})
		sideDone <- err
	}()
	waitFor(t, func() bool { return a.sendsContaining("side work") == 1 })
	time.Sleep(5 * time.Millisecond)
	mgr.gcSweep(context.Background())
	if _, ok := mgr.Get(main.ID); !ok || a.sendsContaining("CHECKPOINT") != 0 {
		t.Fatal("the main session was reclaimed while a side turn was in flight")
	}
	close(gate)
	if err := <-sideDone; err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	mgr.gcSweep(context.Background())
	if _, ok := mgr.Get(main.ID); ok {
		t.Fatal("the idle main session was not reclaimed once the side turn ended")
	}
}

// TestMainAndSideTurnsAreSerial: two interactive turns in one workspace
// never run at the same time.
func TestMainAndSideTurnsAreSerial(t *testing.T) {
	mgr, a, main, side := twoLanes(t)
	mgr.Policy = Policy{}
	gate := a.gate("main work")
	go func() {
		_, _ = mgr.SendAndCollect(context.Background(), main.ID, "main work", TurnOptions{Timeout: 5 * time.Second})
	}()
	waitFor(t, func() bool { return a.sendsContaining("main work") == 1 })
	done := make(chan string, 1)
	go func() {
		r, _ := mgr.SendAndCollect(context.Background(), side.ID, "side q", TurnOptions{Timeout: 5 * time.Second})
		done <- r
	}()
	time.Sleep(150 * time.Millisecond)
	if a.sendsContaining("side q") != 0 {
		t.Fatal("a side turn started while a main turn ran in the same workspace")
	}
	close(gate)
	if got := <-done; got != "re:side q" {
		t.Fatalf("side turn = %q", got)
	}
}
