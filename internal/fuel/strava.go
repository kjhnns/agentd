package fuel

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Activity is the part of a Strava activity the snapshot needs.
type Activity struct {
	SportType  string
	MovingS    float64
	DistanceM  float64
	LocalStart time.Time // wall clock in the athlete's zone, stored as UTC fields
	LocalDate  string    // YYYY-MM-DD of the local start
}

// stravaIndex caches parsed files by path + mtime so a snapshot does not
// re-read 200 files each time.
type stravaIndex struct {
	mu     sync.Mutex
	dir    string
	byPath map[string]stravaFile
}

type stravaFile struct {
	mod time.Time
	act *Activity
}

func newStravaIndex(dir string) *stravaIndex {
	return &stravaIndex{dir: expandHome(dir), byPath: map[string]stravaFile{}}
}

// Load returns every activity and the newest file's mtime (zero if none).
func (s *stravaIndex) Load() ([]Activity, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return nil, time.Time{}
	}
	paths, _ := filepath.Glob(filepath.Join(s.dir, "*.md"))
	var out []Activity
	var newest time.Time
	live := map[string]bool{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		live[p] = true
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		sf, ok := s.byPath[p]
		if !ok || !sf.mod.Equal(fi.ModTime()) {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			sf = stravaFile{mod: fi.ModTime(), act: parseStrava(b)}
			s.byPath[p] = sf
		}
		if sf.act != nil {
			out = append(out, *sf.act)
		}
	}
	for p := range s.byPath {
		if !live[p] {
			delete(s.byPath, p)
		}
	}
	return out, newest
}

// parseStrava reads the embedded ```json block of a strava-sync file.
func parseStrava(b []byte) *Activity {
	i := bytes.Index(b, []byte("```json"))
	if i < 0 {
		return nil
	}
	rest := b[i+len("```json"):]
	j := bytes.Index(rest, []byte("```"))
	if j < 0 {
		return nil
	}
	var raw struct {
		SportType      string  `json:"sport_type"`
		Type           string  `json:"type"`
		MovingTime     float64 `json:"moving_time"`
		Distance       float64 `json:"distance"`
		StartDateLocal string  `json:"start_date_local"`
	}
	if json.Unmarshal(bytes.TrimSpace(rest[:j]), &raw) != nil {
		return nil
	}
	sport := raw.SportType
	if sport == "" {
		sport = raw.Type
	}
	// start_date_local is the LOCAL wall clock with a misleading "Z" suffix.
	ls := strings.TrimSuffix(raw.StartDateLocal, "Z")
	t, err := time.Parse("2006-01-02T15:04:05", ls)
	if err != nil {
		return nil
	}
	return &Activity{SportType: sport, MovingS: raw.MovingTime, DistanceM: raw.Distance, LocalStart: t, LocalDate: t.Format("2006-01-02")}
}

func isTrainingSport(s string) bool {
	switch s {
	case "Run", "TrailRun", "VirtualRun", "Ride", "VirtualRide", "GravelRide", "MountainBikeRide", "EBikeRide", "Swim":
		return true
	}
	return false
}

func isRun(s string) bool { return s == "Run" || s == "TrailRun" || s == "VirtualRun" }
