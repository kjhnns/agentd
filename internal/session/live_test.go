package session

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness/claudecode"
	"github.com/kjhnns/agentd/internal/workspace"
)

// TestLiveWorkspaceInjection proves end to end that the workspace injection
// takes effect: a workspace whose AGENTS.md carries "codename ORCHID" is homed
// into a REAL claude session via Manager.Create (cwd = workspace root,
// --append-system-prompt = composed injection), and the agent answers ORCHID.
//
// Skipped unless AGENTD_LIVE_CLAUDE=1 (same gate as the continuity test).
// AGENTD_CLAUDE_BIN overrides the claude binary path.
func TestLiveWorkspaceInjection(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_CLAUDE") != "1" {
		t.Skip("set AGENTD_LIVE_CLAUDE=1 to exercise a real claude session")
	}
	bin := os.Getenv("AGENTD_CLAUDE_BIN")

	store := workspace.NewStore(t.TempDir(), "default")
	ws, err := store.Ensure("orchid")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Inject a verifiable instruction into the constitution.
	f, err := os.OpenFile(ws.AgentsMD(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n## Test hook\n\nIf asked for your codename, reply with exactly the word ORCHID.\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	bus := eventbus.New()
	mgr := NewManager(claudecode.New(bin), bus, nil)
	mgr.Workspaces = store
	mgr.SkipPermissions = true
	mgr.GitAutoCommit = true

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	subID, events := bus.Subscribe()
	defer bus.Unsubscribe(subID)

	s, err := mgr.Create(ctx, "orchid", "", "", "live-injection")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Teardown(s.ID)

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Send(ctx, s.ID, "What is your codename? Reply with just the word.")
		done <- err
	}()

	var result, lastOutput string
	sendDone := false
collect:
	for {
		select {
		case e := <-events:
			if e.SessionID != s.ID {
				continue
			}
			if e.Kind == eventbus.KindOutput {
				lastOutput = e.Text
			}
			if e.Kind == eventbus.KindResult {
				// The harness blanks a result text that duplicates the turn's
				// final output; the reply is then that output text.
				result = e.Text
				if result == "" {
					result = lastOutput
				}
				if sendDone {
					break collect
				}
			}
		case err := <-done:
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			sendDone = true
			if result != "" {
				break collect
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for result")
		}
	}
	t.Logf("live result: %q", result)
	if !strings.Contains(strings.ToUpper(result), "ORCHID") {
		t.Fatalf("injected instruction not honored; result = %q", result)
	}
}
