package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness/claudecode"
	"github.com/kjhnns/agentd/internal/workspace"
)

// TestLiveLosslessResetNovember3 is THE MONEY TEST, live. It proves an agentd
// context RESET is LOSSLESS end to end with a real Claude Code process:
//
//  1. open a warm session in a scratch workspace,
//  2. plant a durable fact ("launch date is NOVEMBER 3") via a turn,
//  3. FORCE a context reset (checkpoint-flush -> teardown -> fresh process
//     re-hydrated from the workspace artifacts),
//  4. on the FRESH process ask "what launch date did we decide?" and require the
//     answer to contain NOVEMBER 3.
//
// The fresh process has an EMPTY context window; it can only know the date
// because the checkpoint-flush wrote it to context.md/memory and the reset
// re-hydrated the fresh process from that artifact. It also asserts the
// workspace git log gained a checkpoint commit and an artifact contains the
// fact.
//
// Skipped unless AGENTD_LIVE_CLAUDE=1 (so `go test ./...` does not burn tokens).
// AGENTD_CLAUDE_BIN overrides the claude binary.
func TestLiveLosslessResetNovember3(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_CLAUDE") != "1" {
		t.Skip("set AGENTD_LIVE_CLAUDE=1 to run the live lossless-reset money test")
	}
	bin := os.Getenv("AGENTD_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	if !workspace.GitAvailable() {
		t.Skip("git not on PATH")
	}

	store := workspace.NewStore(t.TempDir(), "default")
	bus := eventbus.New()
	mgr := NewManager(claudecode.New(bin), bus, nil)
	mgr.Workspaces = store
	mgr.SkipPermissions = true
	mgr.GitAutoCommit = true
	// Disable all AUTOMATIC triggers; this test forces the reset explicitly.
	mgr.Policy = Policy{ContextResetPressure: 0, IdleTimeout: 0, MaxTurns: 0, MaxWallclock: 0, CheckpointTimeout: 240 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	subID, events := bus.Subscribe()
	defer bus.Unsubscribe(subID)

	s, err := mgr.Create(ctx, "default", "", "", "nov3")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Teardown(s.ID)

	// send runs a turn and returns the session's result text.
	send := func(text string) string {
		done := make(chan error, 1)
		go func() { _, e := mgr.Send(ctx, s.ID, text); done <- e }()
		var result string
		sendDone := false
		for {
			select {
			case e := <-events:
				if e.SessionID == s.ID && e.Kind == eventbus.KindResult {
					result = e.Text
					if sendDone {
						return result
					}
				}
			case e := <-done:
				if e != nil {
					t.Fatalf("Send(%q): %v", text, e)
				}
				sendDone = true
				if result != "" {
					return result
				}
			case <-ctx.Done():
				t.Fatalf("timed out on turn %q", text)
			}
		}
	}

	// (2) plant the fact on the ORIGINAL process.
	r1 := send("We decided the launch date is NOVEMBER 3. Please note it for later. Reply with just OK.")
	t.Logf("plant turn result: %q", r1)

	// (3) FORCE a context reset: checkpoint-flush -> teardown -> fresh process.
	if err := mgr.Reset(ctx, s.ID, "money-test forced"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	t.Log("context reset complete: old process torn down, fresh process started from artifacts")

	// (4) ask the FRESH process for the date.
	r2 := send("What launch date did we decide on earlier? Answer with just the date.")
	t.Logf("post-reset recall result: %q", r2)

	up := strings.ToUpper(r2)
	if !strings.Contains(up, "NOVEMBER 3") && !strings.Contains(up, "NOV 3") && !strings.Contains(up, "NOV. 3") {
		t.Fatalf("LOSSY reset: fresh process did not recall the date; answer = %q", r2)
	}

	// Ground truth: an artifact (context.md or a memory page) holds the fact.
	ws, _ := store.Resolve("default")
	found := false
	if data, _ := os.ReadFile(ws.ContextMD()); strings.Contains(strings.ToUpper(string(data)), "NOVEMBER 3") {
		found = true
		t.Logf("context.md carries the fact")
	}
	if entries, _ := os.ReadDir(ws.PagesDir()); !found {
		for _, e := range entries {
			data, _ := os.ReadFile(ws.PagesDir() + "/" + e.Name())
			if strings.Contains(strings.ToUpper(string(data)), "NOVEMBER 3") {
				found = true
				t.Logf("memory page %s carries the fact", e.Name())
				break
			}
		}
	}
	if !found {
		t.Fatal("neither context.md nor any memory page contains the fact after checkpoint-flush")
	}

	// Ground truth: a checkpoint commit exists in the workspace git log.
	out, err := exec.Command("git", "-C", ws.Root, "log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	t.Logf("workspace git log:\n%s", out)
	if !strings.Contains(string(out), "checkpoint") {
		t.Fatal("no checkpoint commit in the workspace git log after reset")
	}
}
