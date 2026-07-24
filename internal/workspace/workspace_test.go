package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitLoadRoundtrip: Init scaffolds a usable workspace; Load validates it.
func TestInitLoadRoundtrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws1")
	w, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, p := range []string{
		w.AgentsMD(), w.ClaudeMD(), w.IndexMD(), w.ContextMD(),
		filepath.Join(w.PagesDir(), "memory-conventions.md"),
		filepath.Join(w.PagesDir(), "workspace-layout.md"),
	} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("missing scaffolded file %s: %v", p, err)
		}
	}
	if fi, err := os.Stat(w.WorkDir()); err != nil || !fi.IsDir() {
		t.Errorf("missing work/ dir: %v", err)
	}

	// CLAUDE.md mirrors AGENTS.md content (symlink or copy).
	claude, err := os.ReadFile(w.ClaudeMD())
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	agents, _ := os.ReadFile(w.AgentsMD())
	if string(claude) != string(agents) {
		t.Error("CLAUDE.md does not mirror instructions/AGENTS.md")
	}

	w2, err := Load(root)
	if err != nil {
		t.Fatalf("Load after Init: %v", err)
	}
	if w2.Name != "ws1" || w2.Cwd() != w.Root {
		t.Errorf("Load roundtrip mismatch: name=%q cwd=%q", w2.Name, w2.Cwd())
	}

	// Init is idempotent and non-destructive.
	if err := os.WriteFile(w.AgentsMD(), []byte("CUSTOMIZED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	got, _ := os.ReadFile(w.AgentsMD())
	if !strings.Contains(string(got), "CUSTOMIZED") {
		t.Error("re-Init overwrote an existing AGENTS.md")
	}
}

func TestLoadRejectsNonWorkspace(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load accepted an empty dir with no AGENTS.md")
	}
}

// TestComposeSystemPrompt: the injection contains all three parts (instruction
// set + memory INDEX + handoff) and nothing from the page bodies (index-in,
// pages-on-demand).
func TestComposeSystemPrompt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wsp")
	w, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Make each part uniquely identifiable.
	if err := os.WriteFile(w.AgentsMD(), []byte("INSTRUCTION-MARKER: if asked your codename reply ORCHID"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.ContextMD(), []byte("HANDOFF-MARKER: waiting on the vendor reply"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := w.ComposeSystemPrompt()
	if err != nil {
		t.Fatalf("ComposeSystemPrompt: %v", err)
	}
	for _, want := range []string{
		"INSTRUCTION-MARKER", // instructions/AGENTS.md
		"[Memory conventions](pages/memory-conventions.md)", // INDEX line
		"HANDOFF-MARKER", // context.md
	} {
		if !strings.Contains(got, want) {
			t.Errorf("composed prompt missing %q", want)
		}
	}
	// Page BODIES must not be injected.
	if strings.Contains(got, "EVOLVES rather than accretes") {
		t.Error("composed prompt contains a memory page body; only the INDEX should be injected")
	}
}

func TestStoreEnsureAndResolve(t *testing.T) {
	st := NewStore(t.TempDir(), "")
	if st.Default != "default" {
		t.Fatalf("default workspace name = %q", st.Default)
	}
	// Resolve fails before Ensure...
	if _, err := st.Resolve("alpha"); err == nil {
		t.Fatal("Resolve found a workspace that does not exist")
	}
	// ...Ensure scaffolds it...
	w, err := st.Ensure("alpha")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if w.Name != "alpha" {
		t.Errorf("Ensure name = %q", w.Name)
	}
	// ...and Resolve then works, as does Ensure("") for the default.
	if _, err := st.Resolve("alpha"); err != nil {
		t.Fatalf("Resolve after Ensure: %v", err)
	}
	if w, err := st.Ensure(""); err != nil || w.Name != "default" {
		t.Fatalf("Ensure default: %v (name=%q)", err, w.Name)
	}
}
