// Memory: the workspace's semantic, interlinked knowledge wiki.
//
// Layout (under <workspace>/memory/):
//
//	INDEX.md          always-injected entrypoint: ONE line per page
//	                  ("- [Title](pages/<slug>.md) - hook"), the MEMORY.md
//	                  analogue. Rebuilt from page frontmatter.
//	pages/<slug>.md   topic pages with lightweight frontmatter (title, hook,
//	                  tags, updated) and a body that is RE-SYNTHESIZED as new
//	                  information arrives (EVOLVE, not accrete). Bodies use
//	                  [[slug]] wikilinks to other pages, forming a navigable
//	                  graph the agent deepens over time.
//
// v1 is AGENT-DRIVEN: the injected instruction set (starter AGENTS.md) tells
// the agent the convention (when to create vs update a page, keep pages
// re-synthesized not appended, use [[wikilinks]], keep INDEX current) and the
// agent writes the files with its own tools. These Go helpers plus the
// `agentd memory` CLI give the convention teeth: index rebuild from
// frontmatter and dangling-wikilink detection. Server-driven auto-extraction
// (the server mining transcripts into pages) is explicitly DEFERRED.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Page is one memory topic page: frontmatter + body.
type Page struct {
	Slug    string   // filename without .md
	Title   string   // frontmatter: title
	Hook    string   // frontmatter: hook (one-line description for the INDEX)
	Tags    []string // frontmatter: tags (comma-separated)
	Updated string   // frontmatter: updated (YYYY-MM-DD)
	Body    string   // markdown body after the frontmatter
}

// ParsePage parses a page file's content (frontmatter + body). Files without
// frontmatter yield a Page with Title derived from the slug.
func ParsePage(slug string, content []byte) *Page {
	p := &Page{Slug: slug, Title: slug}
	s := string(content)
	body := s
	if strings.HasPrefix(s, "---\n") {
		if end := strings.Index(s[4:], "\n---"); end >= 0 {
			fm := s[4 : 4+end]
			rest := s[4+end+4:]
			body = strings.TrimPrefix(rest, "\n")
			for _, line := range strings.Split(fm, "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				v = strings.TrimSpace(v)
				switch strings.TrimSpace(k) {
				case "title":
					if v != "" {
						p.Title = v
					}
				case "hook":
					p.Hook = v
				case "updated":
					p.Updated = v
				case "tags":
					for _, t := range strings.Split(v, ",") {
						if t = strings.TrimSpace(t); t != "" {
							p.Tags = append(p.Tags, t)
						}
					}
				}
			}
		}
	}
	p.Body = strings.TrimSpace(body)
	return p
}

// Render serializes the page back to frontmatter + body.
func (p *Page) Render() string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", p.Title)
	fmt.Fprintf(&b, "hook: %s\n", p.Hook)
	if len(p.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(p.Tags, ", "))
	}
	if p.Updated != "" {
		fmt.Fprintf(&b, "updated: %s\n", p.Updated)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(p.Body))
	b.WriteString("\n")
	return b.String()
}

// Pages loads every memory page, sorted by slug.
func (w *Workspace) Pages() ([]*Page, error) {
	entries, err := os.ReadDir(w.PagesDir())
	if err != nil {
		return nil, err
	}
	var pages []*Page
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(w.PagesDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		pages = append(pages, ParsePage(strings.TrimSuffix(e.Name(), ".md"), data))
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	return pages, nil
}

// GetPage loads one page by slug.
func (w *Workspace) GetPage(slug string) (*Page, error) {
	data, err := os.ReadFile(filepath.Join(w.PagesDir(), slug+".md"))
	if err != nil {
		return nil, err
	}
	return ParsePage(slug, data), nil
}

// PutPage writes (creates or updates) a page and refreshes the INDEX line for
// it by rebuilding the index from frontmatter. Updated is stamped if empty.
func (w *Workspace) PutPage(p *Page) error {
	if p.Slug == "" {
		return fmt.Errorf("memory: page slug required")
	}
	if p.Updated == "" {
		p.Updated = time.Now().UTC().Format("2006-01-02")
	}
	if err := os.MkdirAll(w.PagesDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.PagesDir(), p.Slug+".md"), []byte(p.Render()), 0o644); err != nil {
		return err
	}
	_, err := w.RebuildIndex()
	return err
}

// RebuildIndex regenerates memory/INDEX.md from the pages' frontmatter (one
// line per page: title + hook + relative link) and writes it. Returns the new
// content. This is the `agentd memory index` code path and the INDEX's single
// source of truth: the pages themselves.
func (w *Workspace) RebuildIndex() (string, error) {
	pages, err := w.Pages()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Memory index\n\n")
	b.WriteString("One line per page. Open memory/pages/<slug>.md to read a page; only this index is injected at session start.\n\n")
	for _, p := range pages {
		hook := p.Hook
		if hook == "" {
			hook = "(no hook set)"
		}
		fmt.Fprintf(&b, "- [%s](pages/%s.md) - %s\n", p.Title, p.Slug, hook)
	}
	if len(pages) == 0 {
		b.WriteString("(no pages yet)\n")
	}
	content := b.String()
	if err := os.MkdirAll(w.MemoryDir(), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(w.IndexMD(), []byte(content), 0o644); err != nil {
		return "", err
	}
	return content, nil
}

// wikilinkRe matches [[target]] and [[target|label]].
var wikilinkRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)

// ExtractWikilinks returns the targets of all [[wikilinks]] in a body.
func ExtractWikilinks(body string) []string {
	var out []string
	for _, m := range wikilinkRe.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// Dangling is a wikilink whose target resolves to no page.
type Dangling struct {
	FromSlug string // page containing the link
	Target   string // the unresolved [[target]]
}

// CheckLinks resolves every [[wikilink]] across all pages. A link resolves if
// its target matches a page slug or a page title (case-insensitive). Returns
// the per-page outgoing links and the dangling ones.
func (w *Workspace) CheckLinks() (links map[string][]string, dangling []Dangling, err error) {
	pages, err := w.Pages()
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, p := range pages {
		known[strings.ToLower(p.Slug)] = true
		known[strings.ToLower(p.Title)] = true
	}
	links = map[string][]string{}
	for _, p := range pages {
		for _, t := range ExtractWikilinks(p.Body) {
			links[p.Slug] = append(links[p.Slug], t)
			if !known[strings.ToLower(t)] {
				dangling = append(dangling, Dangling{FromSlug: p.Slug, Target: t})
			}
		}
	}
	return links, dangling, nil
}
