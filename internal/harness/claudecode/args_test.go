package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjhnns/agentd/internal/harness"
)

// TestBuildArgsSystemPrompt: SessionConfig.SystemPrompt (the composed
// workspace injection) maps to --append-system-prompt with the exact value.
func TestBuildArgsSystemPrompt(t *testing.T) {
	prompt := "INSTRUCTIONS...\nMEMORY INDEX...\nHANDOFF..."
	args := buildArgs(harness.SessionConfig{
		SessionID:       "s1",
		SystemPrompt:    prompt,
		SkipPermissions: true,
		Model:           "opus",
	}, "")
	found := false
	for i, a := range args {
		if a == "--append-system-prompt" {
			if i+1 >= len(args) || args[i+1] != prompt {
				t.Fatalf("--append-system-prompt value = %q", args[i+1])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("--append-system-prompt missing from args: %v", args)
	}

	// Without a SystemPrompt the flag is absent.
	for _, a := range buildArgs(harness.SessionConfig{SessionID: "s2"}, "") {
		if a == "--append-system-prompt" {
			t.Fatal("--append-system-prompt present without a SystemPrompt")
		}
	}
}

// TestPromptFileKeepsPromptOutOfArgv: with a prompt file the prompt text is
// not in argv, the file is owner-only, and a session id cannot leave the dir.
func TestPromptFileKeepsPromptOutOfArgv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")
	prompt := "CANARY-private-history"
	path, err := writePromptFile(dir, "../../evil", prompt)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("prompt file left the dir: %s", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != prompt {
		t.Fatalf("file content = %q", b)
	}
	args := buildArgs(harness.SessionConfig{SessionID: "s1", SystemPrompt: prompt}, path)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, prompt) {
		t.Fatalf("prompt text in argv: %v", args)
	}
	if !strings.Contains(joined, "--append-system-prompt-file "+path) {
		t.Fatalf("--append-system-prompt-file missing: %v", args)
	}
}

// TestStartFailsClosedWithoutPromptDir: an unwritable prompt dir is an error,
// never a silent fallback to argv.
func TestStartFailsClosedWithoutPromptDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := New("/bin/true").WithPromptDir(filepath.Join(blocker, "sub"))
	if _, err := a.Start(context.Background(), harness.SessionConfig{SessionID: "s", SystemPrompt: "x"}); err == nil {
		t.Fatal("Start succeeded with an unwritable prompt dir")
	}
}
