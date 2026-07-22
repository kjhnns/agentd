// File/path watcher trigger: fire a job when anything under a watched path
// changes. Dependency-free: instead of fsnotify, the watcher polls a cheap
// SIGNATURE of the path (file count + total bytes + latest mtime, recursive)
// on the job's poll interval (default 10s) and fires on any difference. The
// path appearing or disappearing is also a change. The FIRST scan at start is
// the baseline: pre-existing content does not fire.
package scheduler

import (
	"context"
	"io/fs"
	"log"
	"path/filepath"
	"time"
)

// pathSignature is the cheap change-detection fingerprint of a watched path.
type pathSignature struct {
	exists bool
	files  int
	bytes  int64
	latest time.Time
}

func scanPath(path string) pathSignature {
	var sig pathSignature
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries don't wedge the watcher
		}
		sig.exists = true
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !d.IsDir() {
			sig.files++
			sig.bytes += info.Size()
		}
		if info.ModTime().After(sig.latest) {
			sig.latest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return pathSignature{}
	}
	return sig
}

// watchLoop polls one file-trigger job's path and fires on signature change.
func (s *Scheduler) watchLoop(ctx context.Context, j *Job) {
	baseline := scanPath(j.Trigger.Path)
	t := time.NewTicker(j.Trigger.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := scanPath(j.Trigger.Path)
			if cur == baseline {
				continue
			}
			baseline = cur
			log.Printf("scheduler: job %s file trigger: change under %q", j.Name, j.Trigger.Path)
			s.appendState("job_fire", map[string]any{"job": j.Name, "trigger": "file", "path": j.Trigger.Path, "fired_at": time.Now().UTC()})
			s.mu.Lock()
			s.fireLocked(j, "file", time.Time{})
			s.mu.Unlock()
		}
	}
}
