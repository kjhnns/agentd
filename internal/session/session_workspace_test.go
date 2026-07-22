package session

import (
	"context"
	"strings"
	"testing"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/workspace"
)

// fakeAdapter captures the SessionConfig the manager passes to Start, so tests
// can assert the workspace wiring (cwd + composed system prompt) without a
// live harness.
type fakeAdapter struct {
	started []harness.SessionConfig
	events  chan eventbus.Event
}

type fakeHandle struct{ id string }

func (f *fakeHandle) ID() string { return f.id }

func (a *fakeAdapter) Name() string { return "fake" }
func (a *fakeAdapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{}
}
func (a *fakeAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	a.started = append(a.started, cfg)
	return &fakeHandle{id: cfg.SessionID}, nil
}
func (a *fakeAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &fakeHandle{id: existing}, nil
}
func (a *fakeAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	return nil
}
func (a *fakeAdapter) Interrupt(h harness.Handle) error       { return nil }
func (a *fakeAdapter) Status(h harness.Handle) harness.Status { return harness.StatusIdle }
func (a *fakeAdapter) Teardown(h harness.Handle) error        { return nil }
func (a *fakeAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	if a.events == nil {
		a.events = make(chan eventbus.Event)
		close(a.events)
	}
	return a.events
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
		"Memory index",                                // memory/INDEX.md
		"Context handoff",                             // context.md
		"memory-conventions",                          // an INDEX entry
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
