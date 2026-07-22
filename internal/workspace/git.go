// Git: every workspace is a LOCAL git repository, so every change to the
// constitution, the memory wiki, the handoff, and working files is traceable
// and reproducible. This matters doubly for memory: pages are RE-SYNTHESIZED
// in place (evolve, not accrete), so git history is how you see how the
// understanding of a topic changed over time.
//
// Commit policy: NOT per keystroke. Commits happen at natural boundaries:
//   - Init makes the initial scaffold commit.
//   - After each completed harness turn that modified tracked files, the
//     Session Manager auto-commits (config [workspace] git_autocommit,
//     default true) with a generated message naming the changed paths.
//   - `agentd memory add` commits the page change on its own, so a memory
//     re-synthesis lands as its own comprehensible commit.
//
// A REMOTE is optional and OFF by default: nothing is ever pushed
// automatically. [workspace] remote = "..." is recorded for a manual,
// opt-in push; local history first.
//
// Implementation: plain os/exec git (no library). Every helper degrades
// gracefully: if git is not installed or the root is not a repo, callers log
// and skip; a session never crashes over version control.
package workspace

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// gitIdentity pins a commit identity so autocommits work even where
// user.name/user.email are unset.
var gitIdentity = []string{"-c", "user.name=agentd", "-c", "user.email=agentd@localhost"}

func (w *Workspace) git(args ...string) (string, error) {
	full := append([]string{"-C", w.Root}, append(gitIdentity, args...)...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// GitAvailable reports whether a git binary is on PATH.
func GitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// IsGitRepo reports whether the workspace root is inside a git work tree.
func (w *Workspace) IsGitRepo() bool {
	out, err := w.git("rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// gitignore keeps transient noise out of history while TRACKING everything
// that matters: instructions/, memory/, context.md, and working files.
const starterGitignore = `.DS_Store
*.tmp
*.swp
`

// GitInit initializes version control for the workspace: git init (if not
// already a repo), a starter .gitignore, and an initial commit of whatever is
// currently scaffolded. Idempotent; safe to call on an existing repo.
func (w *Workspace) GitInit() error {
	if !GitAvailable() {
		return fmt.Errorf("workspace %s: git not installed; workspace left unversioned", w.Name)
	}
	if !w.IsGitRepo() {
		if _, err := w.git("init"); err != nil {
			return err
		}
	}
	if err := writeIfAbsent(w.Root+"/.gitignore", starterGitignore); err != nil {
		return err
	}
	has, err := w.GitHasChanges()
	if err != nil {
		return err
	}
	if has {
		_, err = w.GitCommitAll("workspace: scaffold starter (agentd init-workspace)")
	}
	return err
}

// GitHasChanges reports whether the work tree has uncommitted changes
// (including untracked files).
func (w *Workspace) GitHasChanges() (bool, error) {
	out, err := w.git("status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// ChangedPaths lists the paths git considers changed (staged, unstaged, or
// untracked), used to generate commit messages.
func (w *Workspace) ChangedPaths() ([]string, error) {
	out, err := w.git("status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		// Renames show as "old -> new"; keep the new path.
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+4:]
		}
		paths = append(paths, strings.Trim(p, `"`))
	}
	return paths, nil
}

// GitCommitAll stages everything and commits with msg, returning the short
// commit hash. If nothing changed it returns "" with no error.
func (w *Workspace) GitCommitAll(msg string) (string, error) {
	has, err := w.GitHasChanges()
	if err != nil {
		return "", err
	}
	if !has {
		return "", nil
	}
	if _, err := w.git("add", "-A"); err != nil {
		return "", err
	}
	if _, err := w.git("commit", "-m", msg); err != nil {
		return "", err
	}
	out, err := w.git("rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// AutoCommit is the end-of-turn hook: if the workspace is a repo with
// changes, commit them all with a generated message "<prefix>: <summary of
// changed paths>". Memory-page changes are called out by page slug so a
// re-synthesis reads clearly in `git log`. Returns the short hash, or "" if
// nothing was committed (no repo, no git, or no changes); errors are for the
// caller to LOG, never to fail a session on.
func (w *Workspace) AutoCommit(prefix string) (string, error) {
	if !GitAvailable() || !w.IsGitRepo() {
		return "", nil
	}
	paths, err := w.ChangedPaths()
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	return w.GitCommitAll(prefix + ": " + summarizeChanges(paths))
}

// summarizeChanges builds a compact, informative commit subject from changed
// paths: memory pages by slug first (history of an evolving page should be
// findable), then other notable files, then a count of the rest.
func summarizeChanges(paths []string) string {
	var memory, other []string
	for _, p := range paths {
		if strings.HasPrefix(p, "memory/pages/") && strings.HasSuffix(p, ".md") {
			slug := strings.TrimSuffix(strings.TrimPrefix(p, "memory/pages/"), ".md")
			memory = append(memory, slug)
		} else if p == "memory/INDEX.md" {
			// index churn accompanies page edits; only worth naming alone
			if len(paths) == 1 {
				other = append(other, p)
			}
		} else {
			other = append(other, p)
		}
	}
	sort.Strings(memory)
	sort.Strings(other)
	var parts []string
	if len(memory) > 0 {
		parts = append(parts, "memory: "+strings.Join(memory, ", "))
	}
	const maxNamed = 4
	if len(other) > maxNamed {
		parts = append(parts, strings.Join(other[:maxNamed], ", ")+fmt.Sprintf(" +%d more", len(other)-maxNamed))
	} else if len(other) > 0 {
		parts = append(parts, strings.Join(other, ", "))
	}
	if len(parts) == 0 {
		return "update"
	}
	return strings.Join(parts, "; ")
}
