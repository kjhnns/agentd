package vault

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// window is a fixed one-minute (or one-hour) counter.
type window struct {
	start time.Time
	n     int
}

func (w *window) hit(now time.Time, span time.Duration, max int) bool {
	if now.Sub(w.start) >= span {
		w.start, w.n = now, 0
	}
	if w.n >= max {
		return false
	}
	w.n++
	return true
}

// limiter implements spec section 11. Behind Caddy every public request has
// the same source address, so public limits are global.
type limiter struct {
	mu        sync.Mutex
	public    window
	failures  window
	blockedTo time.Time
	pair      window
	devMin    map[string]*window
	devMutHr  map[string]*window
}

func newLimiter() *limiter {
	return &limiter{devMin: map[string]*window{}, devMutHr: map[string]*window{}}
}

// allowPublic counts one public request (300 per minute, global).
func (l *limiter) allowPublic(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.public.hit(now, time.Minute, 300)
}

// failure counts an unauthenticated request (spec 11). It reports true when
// the request must get 429: the 30th failure in a minute starts a one-minute
// block, during which every unauthenticated request gets 429 (authenticated
// ones are not affected).
func (l *limiter) failure(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.blockedTo) {
		return true
	}
	if now.Sub(l.failures.start) >= time.Minute {
		l.failures.start, l.failures.n = now, 0
	}
	l.failures.n++
	if l.failures.n >= 30 {
		l.blockedTo = now.Add(time.Minute)
		return true // the 30th failure is already answered 429
	}
	return false
}

// blocked reports an active failure block.
func (l *limiter) blocked(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return now.Before(l.blockedTo)
}

// allowDevice: 60 requests per minute; mutations 20 per hour.
func (l *limiter) allowDevice(dev string, mutation bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.devMin) > 100 {
		l.devMin, l.devMutHr = map[string]*window{}, map[string]*window{}
	}
	w := l.devMin[dev]
	if w == nil {
		w = &window{}
		l.devMin[dev] = w
	}
	if !w.hit(now, time.Minute, 60) {
		return false
	}
	if mutation {
		m := l.devMutHr[dev]
		if m == nil {
			m = &window{}
			l.devMutHr[dev] = m
		}
		return m.hit(now, time.Hour, 20)
	}
	return true
}

// allowPair: 10 pairing calls per minute in total, before any lookup.
func (l *limiter) allowPair(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pair.hit(now, time.Minute, 10)
}

// auditLog appends one JSON line per event and rotates at 10 MiB (one old
// file kept). No labels, logins, urls or values ever go in.
type auditLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

const auditMax = 10 << 20

func openAudit(path string) (*auditLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditLog{path: path, f: f}, nil
}

func (a *auditLog) write(ts int64, op, dev, item, result, listener string) {
	if a == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"ts": ts, "op": op, "dev": dev, "item": item, "result": result, "listener": listener})
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		if fi, err := a.f.Stat(); err == nil && fi.Size() > auditMax {
			a.f.Close()
			a.f = nil
			if err := os.Rename(a.path, a.path+".1"); err != nil {
				log.Printf("vaultd: audit rotation failed: %v", err)
				if err := os.Truncate(a.path, 0); err != nil { // stay within the bound
					log.Printf("vaultd: audit truncate failed: %v", err)
				}
			}
		}
	}
	if a.f == nil {
		f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Printf("vaultd: audit log unavailable: %v", err)
			return
		}
		a.f = f
	}
	if _, err := a.f.Write(append(b, '\n')); err != nil {
		log.Printf("vaultd: audit write failed: %v", err)
	} else if err := a.f.Sync(); err != nil {
		log.Printf("vaultd: audit sync failed: %v", err)
	}
}
