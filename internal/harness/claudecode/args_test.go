package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/harness"
)

// TestBuildArgsSystemPrompt keeps prompt text out of every argument path.
func TestBuildArgsSystemPrompt(t *testing.T) {
	prompt := "INSTRUCTIONS...\nMEMORY INDEX...\nHANDOFF..."
	for _, path := range []string{"", "/private/s1.prompt"} {
		args := buildArgs(harness.SessionConfig{SessionID: "s1", SystemPrompt: prompt, SkipPermissions: true, Model: "opus"}, path)
		joined := strings.Join(args, " ")
		if strings.Contains(joined, prompt) {
			t.Fatalf("prompt leaked: %v", args)
		}
		for _, arg := range args {
			if arg == "--append-system-prompt" {
				t.Fatal("argv prompt branch remains")
			}
		}
		if path != "" && !strings.Contains(joined, "--append-system-prompt-file "+path) {
			t.Fatal(args)
		}
		if path == "" && strings.Contains(joined, "--append-system-prompt-file") {
			t.Fatal(args)
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

func TestPromptFilesUniquePerStart(t *testing.T) {
	dir := t.TempDir()
	first, err := writePromptFile(dir, "same-session", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := writePromptFile(dir, "same-session", "second")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("process starts share a prompt file")
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second)
	if err != nil || string(data) != "second" {
		t.Fatalf("old cleanup affected new prompt: %q %v", data, err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
}

func TestPromptDirRemovesOnlyStalePrompts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"old.prompt", "active.prompt", "other.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := daemonStart.Add(-time.Hour)
		if name == "active.prompt" {
			stamp = daemonStart.Add(time.Hour)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	New("claude").WithPromptDir(dir)
	if _, err := os.Stat(filepath.Join(dir, "old.prompt")); !os.IsNotExist(err) {
		t.Fatalf("stale prompt remains: %v", err)
	}
	for _, name := range []string{"active.prompt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailedStartRemovesPrompt(t *testing.T) {
	dir := t.TempDir()
	a := New(filepath.Join(dir, "missing-cli")).WithPromptDir(dir)
	if _, err := a.Start(context.Background(), harness.SessionConfig{SessionID: "s", SystemPrompt: "private"}); err == nil {
		t.Fatal("start succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed start left files: %v %v", entries, err)
	}
}

func TestPromptFileRelativeDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Rel(cwd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := writePromptFile(dir, "session", "private")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("prompt path depends on harness cwd: %s", path)
	}
	if _, err := New("/bin/true").Start(context.Background(), harness.SessionConfig{SessionID: "s", SystemPrompt: "private"}); err == nil {
		t.Fatal("missing prompt directory accepted")
	}
}
