// Package notify is agentd's generic NOTIFICATION HUB. It is the single, uniform
// wire from any producer of user-facing notifications (the scheduler's per-job
// notify policy today; future subsystems tomorrow) to every channel that can
// surface one to the user.
//
// The hub is deliberately tiny and channel-agnostic: a channel adapter that can
// deliver a notification implements the Sink capability (a single
// Notify(Notification) method), registers itself with the Hub, and the Hub fans
// each Dispatch out to all registered sinks. This is exactly the ChannelAdapter
// shape applied to the OUTBOUND-only case: the core does not know or care which
// channels exist, and adding a channel is registration, not new wiring.
//
// It is what connects scheduler.OnNotify (which had no consumer) to real
// surfaces generically: main wires scheduler.OnNotify -> Hub.Dispatch, and every
// notify-capable channel (web, telegram) receives the job notification.
package notify

import (
	"log"
	"sync"
	"time"
)

// Level classifies a notification's severity/kind for the UI to style.
type Level string

const (
	LevelInfo   Level = "info"   // informational
	LevelIssue  Level = "issue"  // something needs attention (a failed/odd run)
	LevelResult Level = "result" // a completed result the user asked to always see
)

// Notification is one user-facing message from any source, normalized so a sink
// never sees producer-specific shapes.
type Notification struct {
	Source string    `json:"source"` // job/session name, e.g. "job:morning-triage"
	Level  Level     `json:"level"`  // info | issue | result
	Text   string    `json:"text"`
	Ts     time.Time `json:"ts"`
}

// Sink is the notify capability a channel opts into. A channel that can deliver
// a notification to the user implements this single method; Notify must be safe
// for concurrent use and should not block for long (the Hub calls it off the
// dispatch path, but a wedged sink still wastes a goroutine).
type Sink interface {
	// Name identifies the sink for logging/dedup.
	Name() string
	// Notify delivers one notification. A returned error is logged, never fatal.
	Notify(n Notification) error
}

// Hub fans notifications out to every registered sink. Zero value is not usable;
// call NewHub.
type Hub struct {
	mu    sync.RWMutex
	sinks []Sink
}

// NewHub returns a ready Hub with no sinks.
func NewHub() *Hub { return &Hub{} }

// Register adds a notify-capable channel. Registering the same sink twice is
// allowed but pointless; callers register once at wiring time.
func (h *Hub) Register(s Sink) {
	if s == nil {
		return
	}
	h.mu.Lock()
	h.sinks = append(h.sinks, s)
	h.mu.Unlock()
}

// Count reports the number of registered sinks (useful for health/tests).
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sinks)
}

// Dispatch fans a notification out to every registered sink. Each sink is called
// on its own goroutine so one slow/blocking sink (e.g. a Telegram HTTP send)
// never delays the others or the caller. A sink error is logged, not fatal. If
// Ts is zero it is stamped now.
func (h *Hub) Dispatch(n Notification) {
	if n.Ts.IsZero() {
		n.Ts = time.Now().UTC()
	}
	h.mu.RLock()
	sinks := make([]Sink, len(h.sinks))
	copy(sinks, h.sinks)
	h.mu.RUnlock()
	for _, s := range sinks {
		s := s
		go func() {
			if err := s.Notify(n); err != nil {
				log.Printf("notify: sink %s failed: %v", s.Name(), err)
			}
		}()
	}
}
