package workspace

import (
	"bytes"
	"log"
	"os"
	"os/exec"
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
	// Strip version control and load a new value with no cached repo result.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	w = &Workspace{Name: w.Name, Root: w.Root}
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

func TestAutoCommitSkipsNestedEmptyRepo(t *testing.T) {
	w, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(w.WorkDir(), "dir with spaces", "empty repo")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	nw := &Workspace{Root: nested}
	if _, err := nw.git("init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.ContextMD(), []byte("updated handoff"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := w.AutoCommit("turn")
	if err != nil || sha == "" {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	out, err := w.git("show", "HEAD:context.md")
	if err != nil || out != "updated handoff" {
		t.Fatalf("committed content=%q err=%v", out, err)
	}
	out, err = w.git("ls-files", "--stage", "work")
	if err != nil || out != "" {
		t.Fatalf("nested repo staged: %q, %v", out, err)
	}
	if sha, err := w.AutoCommit("only nested repo remains"); err != nil || sha != "" {
		t.Fatalf("no-op sha=%q err=%v", sha, err)
	}
}

func TestAutoCommitCachesRepoAndCommitsPartialAdd(t *testing.T) {
	w, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !w.IsGitRepo() {
		t.Fatal("not a repo")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    status|--is-inside-work-tree) echo "$arg" >> "$J2_GIT_CALLS" ;;
    add) adding=1 ;;
  esac
done
if [ "$adding" = 1 ] && [ "$J2_SKIP_ADD" = 1 ]; then
  echo "error: unable to index file 'bad-path'" >&2
  exit 1
fi
"$J2_REAL_GIT" "$@"
result=$?
if [ "$adding" = 1 ] && [ "$result" = 0 ]; then
  echo "error: unable to index file 'bad-path'" >&2
  exit 1
fi
exit "$result"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("J2_REAL_GIT", realGit)
	t.Setenv("J2_GIT_CALLS", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(w.ContextMD(), []byte("staged despite warning"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warning bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&warning)
	t.Cleanup(func() { log.SetOutput(previous) })
	sha, err := w.AutoCommit("partial")
	if err != nil || sha == "" {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "status\n" {
		t.Fatalf("expected one status and cached repo check: %q %v", data, err)
	}
	if !strings.Contains(warning.String(), "staging warning") || !strings.Contains(warning.String(), "bad-path") {
		t.Fatalf("missing warning: %s", warning.String())
	}
	out, err := w.git("show", "HEAD:context.md")
	if err != nil || out != "staged despite warning" {
		t.Fatalf("committed=%q err=%v", out, err)
	}
	t.Setenv("J2_SKIP_ADD", "1")
	if err := os.WriteFile(w.ContextMD(), []byte("cannot stage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sha, err := w.AutoCommit("nothing staged"); sha != "" || err == nil || !strings.Contains(err.Error(), "bad-path") {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
}

func TestAutoCommitKeepsRegisteredSubmodule(t *testing.T) {
	w, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.git("-c", "protocol.file.allow=always", "submodule", "add", source.Root, "work/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AutoCommit("register submodule"); err != nil {
		t.Fatal(err)
	}
	sub := &Workspace{Root: filepath.Join(w.Root, "work", "sub")}
	if err := os.WriteFile(filepath.Join(sub.Root, "new.txt"), []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.GitCommitAll("submodule update"); err != nil {
		t.Fatal(err)
	}
	if sha, err := w.AutoCommit("update submodule"); err != nil || sha == "" {
		t.Fatalf("submodule commit=%q err=%v", sha, err)
	}
	if dirty, err := w.GitHasChanges(); err != nil || dirty {
		t.Fatalf("submodule update not staged: dirty=%v err=%v", dirty, err)
	}
}

func TestIsGitRepoDoesNotCacheNegative(t *testing.T) {
	w := &Workspace{Root: t.TempDir()}
	if w.IsGitRepo() {
		t.Fatal("empty directory is a repo")
	}
	if _, err := w.git("init"); err != nil {
		t.Fatal(err)
	}
	if !w.IsGitRepo() {
		t.Fatal("negative result was cached")
	}
}
