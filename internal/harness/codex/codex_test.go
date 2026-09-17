package codex

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
)

func fakeCLI(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
input=$(cat)
case "$input" in
 wait) printf '%s\n' '{"type":"turn.started"}'; exec sleep 30 ;;
 fail) printf '%s\n' '{"type":"turn.failed","error":{"message":"quota exceeded"}}'; exit 1 ;;
 crash) echo 'startup failed' >&2; exit 2 ;;
 malformed) echo 'not JSON'; exit 0 ;;
 incomplete) printf '%s\n' '{"type":"thread.started","thread_id":"test-thread"}'; exit 0 ;;
esac
printf '%s\n' '{"type":"thread.started","thread_id":"test-thread"}' '{"type":"turn.started"}'
case " $* " in
 *" resume test-thread "*) printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"remembered"}}' ;;
 *) printf '%s\n' '{"type":"item.started","item":{"type":"command_execution","command":"pwd"}}' '{"type":"item.completed","item":{"type":"agent_message","text":"first"}}' ;;
esac
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":900000}}'
`
	if err := os.WriteFile(p, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func drain(h harness.Handle, a *Adapter) []eventbus.Event {
	var out []eventbus.Event
	for {
		select {
		case e := <-a.Events(h):
			out = append(out, e)
		default:
			return out
		}
	}
}
func TestContinuityAndLifecycle(t *testing.T) {
	a := New(fakeCLI(t))
	h, err := a.Start(context.Background(), harness.SessionConfig{SessionID: "s", Cwd: t.TempDir(), SystemPrompt: "workspace instructions"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Teardown(h)
	for i, want := range []string{"first", "remembered"} {
		if err = a.Send(context.Background(), h, harness.Input{Text: "hello"}); err != nil {
			t.Fatal(err)
		}
		evs := drain(h, a)
		results := 0
		text := ""
		for _, e := range evs {
			if e.SessionID != "s" || e.TS.IsZero() {
				t.Fatalf("missing envelope: %+v", e)
			}
			if e.Kind == eventbus.KindOutput {
				text = e.Text
			}
			if e.Kind == eventbus.KindResult {
				results++
				if e.Text != "" {
					t.Fatal("duplicate result text")
				}
			}
		}
		if text != want || results != 1 {
			t.Fatalf("turn %d: %+v", i, evs)
		}
	}
	if a.Status(h) != harness.StatusIdle {
		t.Fatal(a.Status(h))
	}
	if a.Pressure(h).Source != harness.PressureProxy {
		t.Fatal("aggregate usage must not become real pressure")
	}
	if err = a.Teardown(h); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-a.Events(h); ok {
		t.Fatal("events still open")
	}
	if err = a.Send(context.Background(), h, harness.Input{Text: "hello"}); !errors.Is(err, harness.ErrProcessGone) {
		t.Fatal(err)
	}
}
func TestErrorsAndRecovery(t *testing.T) {
	for _, input := range []string{"fail", "crash", "malformed", "incomplete"} {
		t.Run(input, func(t *testing.T) {
			a := New(fakeCLI(t))
			h, _ := a.Start(context.Background(), harness.SessionConfig{SessionID: "s"})
			defer a.Teardown(h)
			if err := a.Send(context.Background(), h, harness.Input{Text: input}); err == nil {
				t.Fatal("expected error")
			}
			if a.Status(h) != harness.StatusError {
				t.Fatal(a.Status(h))
			}
			for _, e := range drain(h, a) {
				if e.Kind == eventbus.KindResult {
					t.Fatal("false success")
				}
			}
			if err := a.Send(context.Background(), h, harness.Input{Text: "hello"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCancellation(t *testing.T) {
	for _, action := range []string{"interrupt", "teardown", "context"} {
		t.Run(action, func(t *testing.T) {
			a := New(fakeCLI(t))
			h, _ := a.Start(context.Background(), harness.SessionConfig{SessionID: "s"})
			defer a.Teardown(h)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.Send(ctx, h, harness.Input{Text: "wait"}) }()
			select {
			case <-a.Events(h):
			case <-time.After(3 * time.Second):
				t.Fatal("did not start")
			}
			switch action {
			case "interrupt":
				a.Interrupt(h)
			case "teardown":
				a.Teardown(h)
			case "context":
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation stuck")
			}
		})
	}
}
func TestArgs(t *testing.T) {
	cfg := harness.SessionConfig{Model: "example-model", SystemPrompt: "line one\n\"two\"", SkipPermissions: true, PermissionMode: harness.PermissionPrompt}
	args := strings.Join(buildArgs(cfg, "thread-123"), " ")
	for _, want := range []string{"--sandbox read-only", "approval_policy=\"never\"", "--model example-model", "developer_instructions=\"line one\\n\\\"two\\\"\"", "resume thread-123 -"} {
		if !strings.Contains(args, want) {
			t.Fatalf("%q missing in %s", want, args)
		}
	}
	if strings.Contains(args, "bypass") {
		t.Fatal("permission override ignored")
	}
	cfg.PermissionMode = harness.PermissionSkip
	if !strings.Contains(strings.Join(buildArgs(cfg, ""), " "), "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatal("skip ignored")
	}
}
func TestLiveContinuity(t *testing.T) {
	if os.Getenv("AGENTD_LIVE_CODEX") != "1" {
		t.Skip("set AGENTD_LIVE_CODEX=1 to exercise the installed authenticated CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	a := New("")
	h, err := a.Start(ctx, harness.SessionConfig{SessionID: "live-codex", Cwd: t.TempDir(), SystemPrompt: "The workspace codeword is ORCHID. Answer briefly. Do not use tools."})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Teardown(h)
	for _, turn := range []struct{ prompt, want string }{
		{"What is the workspace codeword? Also remember the number 7319 for my next message.", "ORCHID"},
		{"What number did I ask you to remember? Reply with only the number.", "7319"},
	} {
		if err = a.Send(ctx, h, harness.Input{Text: turn.prompt}); err != nil {
			t.Fatal(err)
		}
		text := ""
		for _, e := range drain(h, a) {
			if e.Kind == eventbus.KindOutput {
				text += e.Text
			}
		}
		if !strings.Contains(text, turn.want) {
			t.Fatalf("want %q, got %q", turn.want, text)
		}
		t.Logf("reply: %s", text)
	}
}
