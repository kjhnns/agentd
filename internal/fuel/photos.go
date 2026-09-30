package fuel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// photoStore keeps the stripped JPEG copies (0600) for 30 days. A photo id
// resolves only through the stored metadata, never through a path the
// client names.
type photoStore struct {
	mu   sync.Mutex
	dir  string
	idx  string
	meta map[string]photoMeta
	now  func() time.Time
}

type photoMeta struct {
	ID   string    `json:"id"`
	File string    `json:"file"`
	At   time.Time `json:"at"`
}

func openPhotoStore(dir string, now func() time.Time) (*photoStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := &photoStore{dir: dir, idx: filepath.Join(dir, "index.jsonl"), meta: map[string]photoMeta{}, now: now}
	err := loadLines(p.idx, false, func(b []byte) error {
		var m photoMeta
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if m.File == "" {
			delete(p.meta, m.ID) // tombstone
		} else {
			p.meta[m.ID] = m
		}
		return nil
	})
	return p, err
}

func (p *photoStore) save(jpeg []byte) (string, error) {
	id := newID("ph_")
	file := id + ".jpg"
	if err := os.WriteFile(filepath.Join(p.dir, file), jpeg, 0o600); err != nil {
		return "", err
	}
	m := photoMeta{ID: id, File: file, At: p.now()}
	if err := appendJSONLine(p.idx, m); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.meta[id] = m
	p.mu.Unlock()
	return id, nil
}

// path returns the stored file for an id, or "" when unknown or pruned.
func (p *photoStore) path(id string) string {
	p.mu.Lock()
	m, ok := p.meta[id]
	p.mu.Unlock()
	if !ok {
		return ""
	}
	fp := filepath.Join(p.dir, filepath.Base(m.File))
	if _, err := os.Stat(fp); err != nil {
		return ""
	}
	return fp
}

func (p *photoStore) prune(maxAge time.Duration) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for id, m := range p.meta {
		if p.now().Sub(m.At) > maxAge {
			_ = os.Remove(filepath.Join(p.dir, filepath.Base(m.File)))
			delete(p.meta, id)
			_ = appendJSONLine(p.idx, photoMeta{ID: id})
			n++
		}
	}
	return n
}

// remove deletes photos that belong to a request that was not committed.
func (p *photoStore) remove(ids []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		if m, ok := p.meta[id]; ok {
			_ = os.Remove(filepath.Join(p.dir, filepath.Base(m.File)))
			delete(p.meta, id)
			_ = appendJSONLine(p.idx, photoMeta{ID: id})
		}
	}
}
