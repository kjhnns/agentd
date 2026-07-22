package claudecode

import (
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
	})
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
	for _, a := range buildArgs(harness.SessionConfig{SessionID: "s2"}) {
		if a == "--append-system-prompt" {
			t.Fatal("--append-system-prompt present without a SystemPrompt")
		}
	}
}
