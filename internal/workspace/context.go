package workspace

import (
	"os"
	"path/filepath"
	"time"
)

// RotateContext archives old complete handoff lines before trimming the live file.
// The caller must serialize workspace writes.
func (w *Workspace) RotateContext() error {
	data, err := os.ReadFile(w.ContextMD())
	if err != nil || len(data) <= 2*maxSectionBytes {
		return err
	}
	cut := sectionBoundary(data)
	if cut == 0 {
		// No line boundary inside the budget: leave the file alone.
		return nil
	}
	dir := filepath.Join(w.Root, "context-archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, time.Now().Format("2006-01")+".md"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data[cut:]); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// Persist the archive entry and directory before removing any live bytes.
	for _, path := range []string{dir, w.Root} {
		d, err := os.Open(path)
		if err != nil {
			return err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return err
		}
	}
	return os.Truncate(w.ContextMD(), int64(cut))
}
