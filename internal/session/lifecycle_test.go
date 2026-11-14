package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/runlog"
	"github.com/kjhnns/agentd/internal/workspace"
)

func TestIdleTimeoutGCReclaimsZeroTurnWithoutFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runlog.jsonl")
	rl, err := runlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), rl)
	mgr.Policy = Policy{IdleTimeout: time.Minute}
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "idle")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.lastActivity = time.Now().Add(-2 * time.Minute)
	s.mu.Unlock()
	mgr.gcSweep(context.Background())
	if _, ok := mgr.Get(s.ID); ok {
		t.Fatal("idle session remains")
	}
	if len(fa.sentTexts()) != 0 {
		t.Fatal("zero-turn process checkpointed")
	}
	if fa.teardownCount() != 1 {
		t.Fatal("process was not torn down once")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"session_reclaim"`) || !strings.Contains(string(data), `"flush_skipped":true`) {
		t.Fatalf("missing skip record: %s", data)
	}
}

// teardownGateAdapter reports the process loss after DELETE has stopped it.
type teardownGateAdapter struct {
	*fakeAdapter
	entered chan struct{}
	release chan struct{}
}

func (a *teardownGateAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	close(a.entered)
	select {
	case <-a.release:
		return harness.ErrProcessGone
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *teardownGateAdapter) Teardown(h harness.Handle) error {
	a.setStatus(harness.StatusDead)
	return a.fakeAdapter.Teardown(h)
}

func TestTeardownDuringTurnDoesNotRecover(t *testing.T) {
	a := &teardownGateAdapter{fakeAdapter: &fakeAdapter{}, entered: make(chan struct{}), release: make(chan struct{})}
	mgr := NewManager(a, eventbus.New(), nil)
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "delete")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := mgr.RunTurn(ctx, TurnRequest{SessionID: s.ID, Text: "gated", NoCollect: true})
		done <- err
	}()
	select {
	case <-a.entered:
	case <-ctx.Done():
		t.Fatal("turn did not start")
	}
	if err := mgr.Teardown(s.ID); err != nil {
		t.Fatal(err)
	}
	close(a.release)
	select {
	case err := <-done:
		if !errors.Is(err, harness.ErrProcessGone) {
			t.Fatalf("turn error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("turn did not finish")
	}
	if got := len(a.startedConfigs()); got != 1 {
		t.Fatalf("starts=%d, want 1", got)
	}
	if _, ok := mgr.Get(s.ID); ok {
		t.Fatal("deleted session remains")
	}
	if err := mgr.replaceProcess(context.Background(), s, "deleted", false); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("replacement error=%v", err)
	}
}

func TestCheckpointRotatesBeforeCommit(t *testing.T) {
	for _, reclaim := range []bool{false, true} {
		name := "reset"
		if reclaim {
			name = "reclaim"
		}
		t.Run(name, func(t *testing.T) {
			fa := &fakeAdapter{}
			mgr := NewManager(fa, eventbus.New(), nil)
			mgr.Workspaces = workspace.NewStore(t.TempDir(), "default")
			mgr.GitAutoCommit = true
			mgr.Policy = Policy{IdleTimeout: time.Minute}
			s, err := mgr.Create(context.Background(), "", "", "", "rotate")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mgr.Send(context.Background(), s.ID, "work"); err != nil {
				t.Fatal(err)
			}
			fa.onSend = func(text string) {
				if strings.Contains(text, "CHECKPOINT") {
					if err := os.WriteFile(s.ws.ContextMD(), []byte(strings.Repeat("checkpoint 界\n", 10000)), 0o644); err != nil {
						t.Error(err)
					}
				}
			}
			if reclaim {
				s.mu.Lock()
				s.lastActivity = time.Now().Add(-2 * time.Minute)
				s.mu.Unlock()
				mgr.gcSweep(context.Background())
			} else {
				if err := mgr.Reset(context.Background(), s.ID, "rotate"); err != nil {
					t.Fatal(err)
				}
				defer mgr.Teardown(s.ID)
			}
			info, err := os.Stat(s.ws.ContextMD())
			if err != nil || info.Size() > 32*1024 {
				t.Fatalf("context was not rotated: %v %v", info, err)
			}
			archives, err := filepath.Glob(filepath.Join(s.ws.Root, "context-archive", "*.md"))
			if err != nil || len(archives) != 1 {
				t.Fatalf("archives=%v err=%v", archives, err)
			}
			if dirty, err := s.ws.GitHasChanges(); err != nil || dirty {
				t.Fatalf("rotation was not committed: dirty=%v err=%v", dirty, err)
			}
		})
	}
}

// startGateAdapter holds a replacement between process start and installation.
type startGateAdapter struct {
	*echoAdapter
	startedReplacement chan harness.Handle
	release            chan struct{}
}

func (a *startGateAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	h, err := a.echoAdapter.Start(ctx, cfg)
	a.mu.Lock()
	n := len(a.started)
	a.mu.Unlock()
	if n > 1 {
		a.startedReplacement <- h
		<-a.release
	}
	return h, err
}

func TestTeardownDuringReplacementDisposesNewProcess(t *testing.T) {
	a := &startGateAdapter{echoAdapter: newEchoAdapter(), startedReplacement: make(chan harness.Handle, 1), release: make(chan struct{})}
	mgr := NewManager(a, eventbus.New(), nil)
	s, err := mgr.Create(context.Background(), "", t.TempDir(), "", "delete-during-start")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		unlock, err := mgr.lockSession(ctx, s)
		if err != nil {
			done <- err
			return
		}
		defer unlock()
		done <- mgr.replaceProcessLocked(ctx, s, "test")
	}()
	var replacement harness.Handle
	select {
	case replacement = <-a.startedReplacement:
	case <-ctx.Done():
		t.Fatal("replacement did not start")
	}
	if err := mgr.Teardown(s.ID); err != nil {
		t.Fatal(err)
	}
	close(a.release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionGone) {
			t.Fatalf("replacement error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal("replacement did not finish")
	}
	a.mu.Lock()
	alive := replacement.(*scriptedHandle).events != nil
	a.mu.Unlock()
	if alive {
		t.Fatal("replacement process survived removal")
	}
}
