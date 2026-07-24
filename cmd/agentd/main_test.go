package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWorkspaceTarget covers the memory CLI's root/workspace
// resolution: explicit flags win, an explicit or $AGENTD_CONFIG config.toml
// fills empty values from [workspace] root/default, and with no config at all
// the values stay empty (workspace.NewStore then applies its own defaults).
func TestResolveWorkspaceTarget(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "[workspace]\nroot = \"" + filepath.Join(dir, "workspaces") + "\"\ndefault = \"main\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("explicit flags win over config", func(t *testing.T) {
		r, n := resolveWorkspaceTarget("/flag/root", "flagws", cfgPath)
		if r != "/flag/root" || n != "flagws" {
			t.Fatalf("got %q %q, want flag values", r, n)
		}
	})

	t.Run("empty flags resolve from -config", func(t *testing.T) {
		r, n := resolveWorkspaceTarget("", "", cfgPath)
		if r != filepath.Join(dir, "workspaces") || n != "main" {
			t.Fatalf("got %q %q, want config values", r, n)
		}
	})

	t.Run("partial flag keeps flag, fills rest from config", func(t *testing.T) {
		r, n := resolveWorkspaceTarget("/flag/root", "", cfgPath)
		if r != "/flag/root" || n != "main" {
			t.Fatalf("got %q %q, want /flag/root main", r, n)
		}
	})

	t.Run("AGENTD_CONFIG env is honored", func(t *testing.T) {
		t.Setenv("AGENTD_CONFIG", cfgPath)
		r, n := resolveWorkspaceTarget("", "", "")
		if r != filepath.Join(dir, "workspaces") || n != "main" {
			t.Fatalf("got %q %q, want config values via env", r, n)
		}
	})

	t.Run("no config anywhere leaves values empty for NewStore defaults", func(t *testing.T) {
		t.Setenv("AGENTD_CONFIG", "")
		t.Setenv("HOME", dir) // no ~/.agentd/config.toml in the fake home
		r, n := resolveWorkspaceTarget("", "", "")
		if r != "" || n != "" {
			t.Fatalf("got %q %q, want empty empty", r, n)
		}
	})

	t.Run("unreadable config falls back to given values", func(t *testing.T) {
		r, n := resolveWorkspaceTarget("", "ws", filepath.Join(dir, "missing.toml"))
		if r != "" || n != "ws" {
			t.Fatalf("got %q %q, want empty ws", r, n)
		}
	})
}
