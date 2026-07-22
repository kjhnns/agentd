package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/workspace"
)

// TestResetStateMachineLossless is the fake-adapter proof of the reset state
// machine AND that a reset is LOSSLESS through the workspace artifacts:
//
//  1. a turn plants a durable fact,
//  2. Reset runs a checkpoint-flush turn on the CURRENT handle (the fake, mimicking
//     the agent, writes the fact into context.md),
//  3. the old process is torn down,
//  4. a FRESH process is started for the SAME session id, whose re-composed
//     system prompt (ComposeSystemPrompt over the UPDATED context.md) now carries
//     the fact.
//
// The fresh process therefore knows "NOVEMBER 3" from the ARTIFACT, not from the
// old process's context. It also asserts the workspace git log gained a
// checkpoint commit and context.md contains the fact.
func TestResetStateMachineLossless(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not on PATH")
	}
	const fact = "We decided the launch date is NOVEMBER 3."

	store := workspace.NewStore(t.TempDir(), "default")
	fa := &fakeAdapter{}
	// The fake mimics the agent: on the CHECKPOINT turn it persists the fact to
	// context.md (what a real agent is instructed to do).
	fa.onSend = func(text string) {
		if strings.Contains(text, "CHECKPOINT") {
			ws, err := store.Resolve("default")
			if err != nil {
				return
			}
			data, _ := os.ReadFile(ws.ContextMD())
			_ = os.WriteFile(ws.ContextMD(), append(data, []byte("\n\n## Decisions\n\n"+fact+"\n")...), 0o644)
		}
	}

	mgr := NewManager(fa, eventbus.New(), nil)
	mgr.Workspaces = store
	mgr.GitAutoCommit = true

	ctx := context.Background()
	s, err := mgr.Create(ctx, "default", "", "", "lossless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// (1) plant the fact via a normal turn (triggers disabled so it doesn't
	// auto-reset here).
	mgr.Policy = Policy{} // all triggers off for the planting turn
	if _, err := mgr.Send(ctx, s.ID, fact+" Note it."); err != nil {
		t.Fatalf("planting Send: %v", err)
	}

	teardownsBefore := fa.teardownCount()

	// Force the reset explicitly (stands in for a low pressure threshold).
	if err := mgr.Reset(ctx, s.ID, "test-forced"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// A checkpoint-flush turn ran on the old handle.
	sends := fa.sentTexts()
	if len(sends) == 0 || !strings.Contains(sends[len(sends)-1], "CHECKPOINT") {
		t.Fatalf("last Send was not the checkpoint-flush; sends=%v", sends)
	}

	// The old process was torn down exactly once by the reset.
	if got := fa.teardownCount(); got != teardownsBefore+1 {
		t.Fatalf("teardown count = %d, want %d (one teardown on reset)", got, teardownsBefore+1)
	}

	// A FRESH process was started for the SAME id, and its re-composed prompt
	// carries the fact from the updated artifact.
	cfgs := fa.startedConfigs()
	if len(cfgs) != 2 {
		t.Fatalf("Start called %d times, want 2 (initial + fresh)", len(cfgs))
	}
	fresh := cfgs[1]
	if fresh.SessionID != s.ID {
		t.Fatalf("fresh session id = %q, want continuity %q", fresh.SessionID, s.ID)
	}
	if !strings.Contains(fresh.SystemPrompt, "NOVEMBER 3") {
		t.Fatalf("fresh process prompt did not re-hydrate the fact from context.md; prompt:\n%s", fresh.SystemPrompt)
	}

	// Ground truth: context.md on disk contains the fact.
	ws, _ := store.Resolve("default")
	data, _ := os.ReadFile(ws.ContextMD())
	if !strings.Contains(string(data), "NOVEMBER 3") {
		t.Fatalf("context.md missing the fact after checkpoint-flush:\n%s", data)
	}

	// Ground truth: the workspace git log gained a checkpoint commit.
	out, err := exec.Command("git", "-C", ws.Root, "log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "checkpoint") {
		t.Fatalf("git log has no checkpoint commit:\n%s", out)
	}
	t.Logf("git log after reset:\n%s", out)

	// The session id/title is continuous; the process is new.
	if s2, ok := mgr.Get(s.ID); !ok || s2.Title != "lossless" {
		t.Fatal("session identity not preserved across reset")
	}
}

// TestEvalTriggersBackstops: the hard backstops (max_turns, max_wallclock) and
// the pressure threshold each trip evalTriggers with an explanatory reason.
func TestEvalTriggersBackstops(t *testing.T) {
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), nil)
	s := &Session{ID: "x", adapter: fa, handle: &fakeHandle{id: "x"}, handleStart: time.Now()}

	// max_turns backstop.
	mgr.Policy = Policy{MaxTurns: 1}
	s.turns = 1
	if tripped, reason := mgr.evalTriggers(s); !tripped || !strings.Contains(reason, "max_turns") {
		t.Fatalf("max_turns backstop = (%v,%q)", tripped, reason)
	}

	// max_wallclock backstop.
	mgr.Policy = Policy{MaxWallclock: time.Nanosecond}
	s.turns = 0
	s.handleStart = time.Now().Add(-time.Hour)
	if tripped, reason := mgr.evalTriggers(s); !tripped || !strings.Contains(reason, "max_wallclock") {
		t.Fatalf("max_wallclock backstop = (%v,%q)", tripped, reason)
	}

	// pressure threshold.
	mgr.Policy = Policy{ContextResetPressure: 0.75}
	s.handleStart = time.Now()
	fa.setPressure(harness.ContextPressure{Fraction: 0.9, Source: harness.PressureReal})
	if tripped, reason := mgr.evalTriggers(s); !tripped || !strings.Contains(reason, "pressure") {
		t.Fatalf("pressure trigger = (%v,%q)", tripped, reason)
	}

	// below threshold: no trigger.
	fa.setPressure(harness.ContextPressure{Fraction: 0.5, Source: harness.PressureReal})
	if tripped, _ := mgr.evalTriggers(s); tripped {
		t.Fatal("pressure below threshold should not trip")
	}
}

// TestMaxTurnsBackstopResetsViaSend: with max_turns=1 a single completed turn
// through Send triggers a background reset (fresh process for the same id).
func TestMaxTurnsBackstopResetsViaSend(t *testing.T) {
	store := workspace.NewStore(t.TempDir(), "default")
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), nil)
	mgr.Workspaces = store
	mgr.GitAutoCommit = false
	mgr.Policy = Policy{MaxTurns: 1} // only the turns backstop is armed

	ctx := context.Background()
	s, err := mgr.Create(ctx, "default", "", "", "backstop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := mgr.Send(ctx, s.ID, "one turn"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The reset runs in the background; wait for the fresh Start.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(fa.startedConfigs()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(fa.startedConfigs()); got < 2 {
		t.Fatalf("max_turns backstop did not reset via Send: Start called %d times", got)
	}
	// The checkpoint-flush turn ran before the fresh start.
	if sends := fa.sentTexts(); len(sends) < 2 || !strings.Contains(sends[len(sends)-1], "CHECKPOINT") {
		t.Fatalf("expected a checkpoint-flush turn before reset; sends=%v", fa.sentTexts())
	}
}

// TestIdleTimeoutGCReclaims: a session idle past idle_timeout is checkpoint-
// flushed and reclaimed (process torn down, session removed) by the sweeper.
func TestIdleTimeoutGCReclaims(t *testing.T) {
	store := workspace.NewStore(t.TempDir(), "default")
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), nil)
	mgr.Workspaces = store
	mgr.GitAutoCommit = false
	mgr.Policy = Policy{IdleTimeout: time.Minute} // pressure/turns/wallclock off

	ctx := context.Background()
	s, err := mgr.Create(ctx, "default", "", "", "idle")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Age the session past the idle timeout.
	s.mu.Lock()
	s.lastActivity = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()

	mgr.gcSweep(ctx)

	if _, ok := mgr.Get(s.ID); ok {
		t.Fatal("idle session was not reclaimed (still present after gcSweep)")
	}
	if sends := fa.sentTexts(); len(sends) == 0 || !strings.Contains(sends[len(sends)-1], "CHECKPOINT") {
		t.Fatalf("idle reclaim did not checkpoint-flush; sends=%v", fa.sentTexts())
	}
	if fa.teardownCount() < 1 {
		t.Fatal("idle reclaim did not tear down the process")
	}
}

// TestGCRecoversDeadSession: a session whose process reports Dead is recovered
// in place (fresh process, same id) by the sweeper.
func TestGCRecoversDeadSession(t *testing.T) {
	store := workspace.NewStore(t.TempDir(), "default")
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), nil)
	mgr.Workspaces = store
	mgr.GitAutoCommit = false
	mgr.Policy = Policy{IdleTimeout: 0} // idle off; only dead-recovery under test

	ctx := context.Background()
	s, err := mgr.Create(ctx, "default", "", "", "dead")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fa.setStatus(harness.StatusDead)

	mgr.gcSweep(ctx)

	// A fresh process was started (recovery), status back to a live state.
	if got := len(fa.startedConfigs()); got != 2 {
		t.Fatalf("dead-recovery Start count = %d, want 2", got)
	}
	if s2, ok := mgr.Get(s.ID); !ok {
		t.Fatal("recovered session missing from manager")
	} else {
		s2.mu.Lock()
		st := s2.status
		s2.mu.Unlock()
		if st == harness.StatusDead {
			t.Fatal("recovered session still marked dead")
		}
	}
}
