package session

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/workspace"
)

// fakeAdapter is an instrumented in-memory harness for the manager tests. It
// records the SessionConfig of every Start (so tests can assert workspace wiring
// AND the re-composed prompt on a reset), every Send text (so a checkpoint-flush
// is observable), and teardown count. Its reported status and context pressure
// are settable so tests can drive dead-recovery and pressure-triggered resets.
type fakeAdapter struct {
	mu       sync.Mutex
	started  []harness.SessionConfig
	sends    []string
	teardown int

	status   harness.Status
	pressure harness.ContextPressure
	// optional hook invoked on each Send (e.g. to have the checkpoint-flush
	// write to the workspace before teardown).
	onSend func(text string)
}

type fakeHandle struct {
	id     string
	events chan eventbus.Event
}

func (f *fakeHandle) ID() string { return f.id }

func (a *fakeAdapter) startedConfigs() []harness.SessionConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]harness.SessionConfig, len(a.started))
	copy(out, a.started)
	return out
}

func (a *fakeAdapter) sentTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.sends))
	copy(out, a.sends)
	return out
}

func (a *fakeAdapter) teardownCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.teardown
}

func (a *fakeAdapter) setStatus(s harness.Status) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

func (a *fakeAdapter) setPressure(p harness.ContextPressure) {
	a.mu.Lock()
	a.pressure = p
	a.mu.Unlock()
}

func (a *fakeAdapter) Name() string { return "fake" }
func (a *fakeAdapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{}
}
func (a *fakeAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	a.mu.Lock()
	a.started = append(a.started, cfg)
	a.status = harness.StatusIdle
	a.mu.Unlock()
	return &fakeHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 8)}, nil
}
func (a *fakeAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &fakeHandle{id: existing, events: make(chan eventbus.Event, 8)}, nil
}
func (a *fakeAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	a.mu.Lock()
	a.sends = append(a.sends, in.Text)
	hook := a.onSend
	a.mu.Unlock()
	if hook != nil {
		hook(in.Text)
	}
	return nil
}
func (a *fakeAdapter) Interrupt(h harness.Handle) error { return nil }
func (a *fakeAdapter) Status(h harness.Handle) harness.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status == "" {
		return harness.StatusIdle
	}
	return a.status
}
func (a *fakeAdapter) Teardown(h harness.Handle) error {
	a.mu.Lock()
	a.teardown++
	a.mu.Unlock()
	if fh, ok := h.(*fakeHandle); ok && fh.events != nil {
		close(fh.events)
	}
	return nil
}
func (a *fakeAdapter) Pressure(h harness.Handle) harness.ContextPressure {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pressure
}
func (a *fakeAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	if fh, ok := h.(*fakeHandle); ok && fh.events != nil {
		return fh.events
	}
	ch := make(chan eventbus.Event)
	close(ch)
	return ch
}

// TestCreateHomesSessionInWorkspace: with a workspace store configured,
// Manager.Create sets cwd = the workspace root and injects a composed system
// prompt containing all three parts: instruction set + memory INDEX + handoff.
func TestCreateHomesSessionInWorkspace(t *testing.T) {
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), nil)
	mgr.Workspaces = workspace.NewStore(t.TempDir(), "default")

	s, err := mgr.Create(context.Background(), "demo", "", "", "t")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Workspace != "demo" {
		t.Errorf("session workspace = %q", s.Workspace)
	}
	if len(fa.started) != 1 {
		t.Fatalf("adapter started %d times", len(fa.started))
	}
	cfg := fa.started[0]

	ws, err := mgr.Workspaces.Resolve("demo")
	if err != nil {
		t.Fatalf("workspace was not scaffolded by Create: %v", err)
	}
	if cfg.Cwd != ws.Cwd() {
		t.Errorf("cwd = %q, want workspace root %q", cfg.Cwd, ws.Cwd())
	}
	if cfg.SystemPrompt == "" {
		t.Fatal("SystemPrompt is empty; workspace injection not composed")
	}
	for _, want := range []string{
		"Agent instructions (workspace constitution)", // instructions/AGENTS.md
		"Memory index",       // memory/INDEX.md
		"Context handoff",    // context.md
		"memory-conventions", // an INDEX entry
	} {
		if !strings.Contains(cfg.SystemPrompt, want) {
			t.Errorf("injected prompt missing %q", want)
		}
	}
}

// TestSendAutoCommitsWorkspace: after a completed turn that changed workspace
// files, the manager commits the change (end-of-turn boundary).
func TestSendAutoCommitsWorkspace(t *testing.T) {
	if !workspace.GitAvailable() {
		t.Skip("git not on PATH")
	}
	fa := &fakeAdapter{}
	mgr := NewManager(fa, eventbus.New(), nil)
	mgr.Workspaces = workspace.NewStore(t.TempDir(), "default")
	mgr.GitAutoCommit = true

	s, err := mgr.Create(context.Background(), "", "", "", "t")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws, _ := mgr.Workspaces.Resolve("")
	// Simulate the agent re-synthesizing a memory page during the turn.
	if err := ws.PutPage(&workspace.Page{Slug: "learned", Title: "Learned", Hook: "h", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Send(context.Background(), s.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if has, _ := ws.GitHasChanges(); has {
		t.Error("workspace still dirty after turn; end-of-turn autocommit did not run")
	}
}

// TestCreateWithoutStoreRejectsWorkspaceName: naming a workspace without a
// store configured is an explicit error, not a silent fallback.
func TestCreateWithoutStoreRejectsWorkspaceName(t *testing.T) {
	mgr := NewManager(&fakeAdapter{}, eventbus.New(), nil)
	if _, err := mgr.Create(context.Background(), "demo", "", "", "t"); err == nil {
		t.Fatal("Create accepted a workspace name with no store configured")
	}
	// Bare mode still works.
	if _, err := mgr.Create(context.Background(), "", t.TempDir(), "", "t"); err != nil {
		t.Fatalf("bare Create: %v", err)
	}
}
