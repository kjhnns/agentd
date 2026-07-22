package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitInitChangeAutocommit: Init creates a repo with an initial commit;
// a later change plus AutoCommit yields a second commit with a message naming
// the changed memory page.
func TestGitInitChangeAutocommit(t *testing.T) {
	if !GitAvailable() {
		t.Skip("git not on PATH")
	}
	w, err := Init(filepath.Join(t.TempDir(), "gitws"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !w.IsGitRepo() {
		t.Fatal("Init did not create a git repo")
	}
	// Initial commit exists and the tree is clean.
	out, err := w.git("log", "--oneline")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(out, "workspace: scaffold starter") {
		t.Errorf("missing initial scaffold commit; log:\n%s", out)
	}
	if has, _ := w.GitHasChanges(); has {
		st, _ := w.git("status", "--porcelain")
		t.Errorf("tree not clean after Init:\n%s", st)
	}

	// No-change AutoCommit is a no-op.
	if sha, err := w.AutoCommit("turn x"); err != nil || sha != "" {
		t.Errorf("no-op AutoCommit: sha=%q err=%v", sha, err)
	}

	// Re-synthesize a memory page, then autocommit at the turn boundary.
	if err := w.PutPage(&Page{Slug: "vendor-quirks", Title: "Vendor quirks", Hook: "429 on Mondays", Body: "evolved understanding"}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	sha, err := w.AutoCommit("turn abc123")
	if err != nil {
		t.Fatalf("AutoCommit: %v", err)
	}
	if sha == "" {
		t.Fatal("AutoCommit committed nothing despite changes")
	}
	log2, _ := w.git("log", "--oneline")
	if got := strings.Count(strings.TrimSpace(log2), "\n") + 1; got != 2 {
		t.Errorf("want 2 commits, got %d:\n%s", got, log2)
	}
	subject, _ := w.git("log", "-1", "--pretty=%s")
	if !strings.Contains(subject, "turn abc123") || !strings.Contains(subject, "memory: vendor-quirks") {
		t.Errorf("autocommit subject not informative: %q", subject)
	}
	show, _ := w.git("show", "--stat", "--pretty=%s", "HEAD")
	if !strings.Contains(show, "memory/pages/vendor-quirks.md") {
		t.Errorf("HEAD diff does not include the memory page:\n%s", show)
	}
}

// TestAutoCommitGracefulWithoutRepo: a workspace that is not a repo never
// errors on the autocommit path.
func TestAutoCommitGracefulWithoutRepo(t *testing.T) {
	if !GitAvailable() {
		t.Skip("git not on PATH")
	}
	root := filepath.Join(t.TempDir(), "norepo")
	w, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Strip version control off the workspace.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if sha, err := w.AutoCommit("turn x"); err != nil || sha != "" {
		t.Errorf("AutoCommit on non-repo: sha=%q err=%v (want silent no-op)", sha, err)
	}
}

func TestSummarizeChanges(t *testing.T) {
	got := summarizeChanges([]string{"memory/pages/beta.md", "memory/INDEX.md", "context.md", "memory/pages/alpha.md"})
	if !strings.Contains(got, "memory: alpha, beta") || !strings.Contains(got, "context.md") {
		t.Errorf("summarizeChanges = %q", got)
	}
}
