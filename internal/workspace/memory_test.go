package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testWorkspace(t *testing.T) *Workspace {
	t.Helper()
	w, err := Init(filepath.Join(t.TempDir(), "mem"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return w
}

func writePage(t *testing.T, w *Workspace, slug, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.PagesDir(), slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParsePageFrontmatter(t *testing.T) {
	p := ParsePage("vendor-quirks", []byte("---\ntitle: Vendor quirks\nhook: API returns 429 on Mondays\ntags: api, vendor\nupdated: 2026-07-22\n---\n\nBody with a [[billing]] link.\n"))
	if p.Title != "Vendor quirks" || p.Hook != "API returns 429 on Mondays" || p.Updated != "2026-07-22" {
		t.Errorf("frontmatter parse: %+v", p)
	}
	if len(p.Tags) != 2 || p.Tags[0] != "api" {
		t.Errorf("tags parse: %v", p.Tags)
	}
	if !strings.Contains(p.Body, "[[billing]]") || strings.Contains(p.Body, "title:") {
		t.Errorf("body parse: %q", p.Body)
	}
	// Roundtrip through Render.
	p2 := ParsePage("vendor-quirks", []byte(p.Render()))
	if p2.Title != p.Title || p2.Hook != p.Hook || p2.Body != p.Body {
		t.Errorf("render/parse roundtrip mismatch: %+v vs %+v", p2, p)
	}
}

// TestRebuildIndexFromFrontmatter: INDEX.md is regenerated from the pages'
// frontmatter, one line per page.
func TestRebuildIndexFromFrontmatter(t *testing.T) {
	w := testWorkspace(t)
	writePage(t, w, "zebra", "---\ntitle: Zebra topic\nhook: stripes are load-bearing\n---\n\nbody\n")
	writePage(t, w, "apple", "---\ntitle: Apple topic\nhook: keeps doctors away\n---\n\nbody\n")

	content, err := w.RebuildIndex()
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	for _, want := range []string{
		"- [Apple topic](pages/apple.md) - keeps doctors away",
		"- [Zebra topic](pages/zebra.md) - stripes are load-bearing",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("index missing line %q\nindex:\n%s", want, content)
		}
	}
	// Sorted: apple before zebra.
	if strings.Index(content, "apple.md") > strings.Index(content, "zebra.md") {
		t.Error("index not sorted by slug")
	}
	// Written to disk.
	disk, err := os.ReadFile(w.IndexMD())
	if err != nil || string(disk) != content {
		t.Errorf("INDEX.md on disk does not match returned content (err=%v)", err)
	}
}

// TestCheckLinksFlagsDangling: resolver accepts slug and title targets and
// flags unresolved ones.
func TestCheckLinksFlagsDangling(t *testing.T) {
	w := testWorkspace(t)
	writePage(t, w, "alpha", "---\ntitle: Alpha\nhook: a\n---\n\nSee [[beta]] and [[Gamma Topic]] and [[no-such-page]].\n")
	writePage(t, w, "beta", "---\ntitle: Beta\nhook: b\n---\n\nBack to [[alpha]].\n")
	writePage(t, w, "gamma", "---\ntitle: Gamma Topic\nhook: g\n---\n\nno links\n")

	links, dangling, err := w.CheckLinks()
	if err != nil {
		t.Fatalf("CheckLinks: %v", err)
	}
	if got := links["alpha"]; len(got) != 3 {
		t.Errorf("alpha links = %v", got)
	}
	if len(dangling) != 1 || dangling[0].Target != "no-such-page" || dangling[0].FromSlug != "alpha" {
		t.Errorf("dangling = %+v, want exactly [[no-such-page]] from alpha", dangling)
	}
}

func TestPutGetPage(t *testing.T) {
	w := testWorkspace(t)
	if err := w.PutPage(&Page{Slug: "notes", Title: "Notes", Hook: "test hook", Body: "hello [[memory-conventions]]"}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	p, err := w.GetPage("notes")
	if err != nil {
		t.Fatalf("GetPage: %v", err)
	}
	if p.Title != "Notes" || p.Updated == "" || !strings.Contains(p.Body, "hello") {
		t.Errorf("GetPage: %+v", p)
	}
	// PutPage refreshed the INDEX.
	idx, _ := os.ReadFile(w.IndexMD())
	if !strings.Contains(string(idx), "(pages/notes.md) - test hook") {
		t.Errorf("PutPage did not refresh INDEX.md:\n%s", idx)
	}
}

func TestExtractWikilinks(t *testing.T) {
	got := ExtractWikilinks("a [[one]] b [[two|labeled]] c [[ three ]] not [single]")
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d = %q, want %q", i, got[i], want[i])
		}
	}
}
