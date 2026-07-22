// Package workspace makes the WORKSPACE a first-class concept (design 3.5): the
// agent's persistent home directory. A workspace holds
//
//   - instructions/  the agent's constitution: a harness-agnostic root
//     AGENTS.md plus agents/ (persona definitions) and skills/ (skill and
//     workflow definitions). A CLAUDE.md at the workspace root mirrors
//     AGENTS.md so Claude Code auto-loads it as a free bonus.
//   - memory/        a semantic, interlinked knowledge wiki: memory/INDEX.md
//     (one line per page) plus memory/pages/<slug>.md topic pages with
//     frontmatter and [[wikilink]] cross-references. See memory.go.
//   - context.md     the live handoff/session record (active threads, pending
//     confirms, decisions, blockers), the generalization of clawd's
//     state/session-context.md.
//   - work/          scratch space for the agent's working files.
//
// CWD DECISION: the harness runs with cwd = the WORKSPACE ROOT (not work/).
// This is deliberate: the instructions, memory pages, and handoff are then
// visible to the agent as ordinary files it can Read and Edit with its own
// tools, and Claude Code auto-loads the root CLAUDE.md. work/ is merely the
// suggested spot for scratch output.
//
// CONTEXT INJECTION ("index-in, pages-on-demand"): at session start agentd
// composes an injected system prompt from three parts: the root instruction set
// (instructions/AGENTS.md) + the memory INDEX (memory/INDEX.md) + the current
// handoff (context.md). Only the INDEX is injected, never the full memory
// corpus; the index tells the agent WHAT knowledge exists (one line per page)
// and the agent opens individual pages on demand by reading files under
// memory/pages/. This keeps the injection token-sane while making all
// knowledge reachable.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace is one agent home directory rooted at Root.
type Workspace struct {
	Name string // directory basename, e.g. "default"
	Root string // absolute path of the workspace root
}

// Path helpers. All content lives under Root.
func (w *Workspace) InstructionsDir() string { return filepath.Join(w.Root, "instructions") }
func (w *Workspace) AgentsMD() string        { return filepath.Join(w.InstructionsDir(), "AGENTS.md") }
func (w *Workspace) ClaudeMD() string        { return filepath.Join(w.Root, "CLAUDE.md") }
func (w *Workspace) MemoryDir() string       { return filepath.Join(w.Root, "memory") }
func (w *Workspace) PagesDir() string        { return filepath.Join(w.MemoryDir(), "pages") }
func (w *Workspace) IndexMD() string         { return filepath.Join(w.MemoryDir(), "INDEX.md") }
func (w *Workspace) ContextMD() string       { return filepath.Join(w.Root, "context.md") }
func (w *Workspace) WorkDir() string         { return filepath.Join(w.Root, "work") }

// Cwd is the working directory the harness runs in: the workspace root (see
// the CWD DECISION in the package comment).
func (w *Workspace) Cwd() string { return w.Root }

// Load opens an existing workspace rooted at root and validates the layout.
// The minimum viable workspace is instructions/AGENTS.md + memory/INDEX.md;
// context.md and work/ are created lazily if absent so old/partial workspaces
// heal on load.
func Load(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	w := &Workspace{Name: filepath.Base(abs), Root: abs}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("workspace: %s is not a directory", abs)
	}
	if _, err := os.Stat(w.AgentsMD()); err != nil {
		return nil, fmt.Errorf("workspace %s: missing instructions/AGENTS.md (run `agentd init-workspace %s`)", abs, w.Name)
	}
	if _, err := os.Stat(w.IndexMD()); err != nil {
		return nil, fmt.Errorf("workspace %s: missing memory/INDEX.md (run `agentd init-workspace %s`)", abs, w.Name)
	}
	// Heal optional pieces.
	if err := os.MkdirAll(w.PagesDir(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(w.WorkDir(), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(w.ContextMD()); err != nil {
		if err := os.WriteFile(w.ContextMD(), []byte(starterContext), 0o644); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// Init scaffolds a workspace at root with the starter instruction set, an
// example memory wiki, and an empty handoff, so a fresh install is immediately
// usable. Init is idempotent and non-destructive: existing files are never
// overwritten; only missing pieces are created.
func Init(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	w := &Workspace{Name: filepath.Base(abs), Root: abs}
	for _, d := range []string{
		w.InstructionsDir(),
		filepath.Join(w.InstructionsDir(), "agents"),
		filepath.Join(w.InstructionsDir(), "skills"),
		w.PagesDir(),
		w.WorkDir(),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := writeIfAbsent(w.AgentsMD(), starterAgentsMD); err != nil {
		return nil, err
	}
	// CLAUDE.md mirror: prefer a symlink to instructions/AGENTS.md so the two
	// can never drift; fall back to a copy where symlinks are unavailable.
	if _, err := os.Lstat(w.ClaudeMD()); err != nil {
		if err := os.Symlink(filepath.Join("instructions", "AGENTS.md"), w.ClaudeMD()); err != nil {
			if err := writeIfAbsent(w.ClaudeMD(), starterAgentsMD); err != nil {
				return nil, err
			}
		}
	}
	if err := writeIfAbsent(w.ContextMD(), starterContext); err != nil {
		return nil, err
	}
	// Example memory pages demonstrating frontmatter + the [[wikilink]] web.
	for slug, content := range starterPages {
		if err := writeIfAbsent(filepath.Join(w.PagesDir(), slug+".md"), content); err != nil {
			return nil, err
		}
	}
	// INDEX.md is generated from the pages' frontmatter (same code path as
	// `agentd memory index`), unless one already exists.
	if _, err := os.Stat(w.IndexMD()); err != nil {
		if _, err := w.RebuildIndex(); err != nil {
			return nil, err
		}
	}
	// Version control: every workspace is a git repo (see git.go). If git is
	// not installed the workspace still works, just unversioned.
	if GitAvailable() {
		if err := w.GitInit(); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func writeIfAbsent(path, content string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// maxSectionBytes caps each injected section so a runaway instruction file or
// handoff cannot blow the context budget. The memory CORPUS is never injected
// at all (index-in, pages-on-demand); this cap guards the three small parts.
const maxSectionBytes = 32 * 1024

// ComposeSystemPrompt builds the injected system prompt for a session started
// in this workspace: root instructions + memory INDEX + current handoff. It is
// passed to the harness via SessionConfig.SystemPrompt (--append-system-prompt
// for Claude Code). Memory PAGES are deliberately not included; the agent opens
// them on demand (see package comment).
func (w *Workspace) ComposeSystemPrompt() (string, error) {
	instr, err := readSection(w.AgentsMD())
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", w.Name, err)
	}
	index, err := readSection(w.IndexMD())
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", w.Name, err)
	}
	handoff, err := readSection(w.ContextMD())
	if err != nil {
		// A missing handoff is not fatal; treat as empty.
		handoff = "(no handoff recorded)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Workspace: %s\n\n", w.Name)
	b.WriteString("Your persistent home directory is the current working directory. ")
	b.WriteString("Instructions live in instructions/, long-term memory in memory/pages/ (indexed below), the session handoff in context.md.\n\n")
	b.WriteString("## Instructions (instructions/AGENTS.md)\n\n")
	b.WriteString(instr)
	b.WriteString("\n\n## Memory index (memory/INDEX.md)\n\n")
	b.WriteString("Only this index is injected, not the pages. Read memory/pages/<slug>.md for any entry that looks relevant to the task at hand.\n\n")
	b.WriteString(index)
	b.WriteString("\n\n## Current handoff (context.md)\n\n")
	b.WriteString(handoff)
	b.WriteString("\n")
	return b.String(), nil
}

func readSection(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if len(s) > maxSectionBytes {
		s = s[:maxSectionBytes] + "\n[truncated: section exceeds injection budget]"
	}
	return s, nil
}

// ---- Store: named workspaces under one root dir ----

// Store resolves workspace names to directories under Root
// (default ~/.agentd/workspaces), with a configurable default name.
type Store struct {
	Root    string // dir containing one subdir per workspace
	Default string // workspace used when a session names none; "default" if empty
}

// DefaultRoot returns ~/.agentd/workspaces.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".agentd", "workspaces")
	}
	return filepath.Join(home, ".agentd", "workspaces")
}

// NewStore builds a Store, applying defaults for empty fields.
func NewStore(root, def string) *Store {
	if root == "" {
		root = DefaultRoot()
	}
	if def == "" {
		def = "default"
	}
	return &Store{Root: root, Default: def}
}

// Path returns the directory for a named workspace ("" means the default).
func (s *Store) Path(name string) string {
	if name == "" {
		name = s.Default
	}
	return filepath.Join(s.Root, name)
}

// Resolve loads a named workspace ("" means the default). It fails if the
// workspace does not exist.
func (s *Store) Resolve(name string) (*Workspace, error) {
	return Load(s.Path(name))
}

// Ensure loads a named workspace, scaffolding it first if absent, so the
// default path "just works" on a fresh install.
func (s *Store) Ensure(name string) (*Workspace, error) {
	p := s.Path(name)
	if _, err := os.Stat(filepath.Join(p, "instructions", "AGENTS.md")); err != nil {
		if _, err := Init(p); err != nil {
			return nil, err
		}
	}
	return Load(p)
}
