// Package eventbus defines the normalized Event type (design section 3.4) and a
// simple in-process publish/subscribe bus. Every harness adapter emits Events in
// this shape so channels and UIs never see harness-specific output.
package eventbus

import (
	"sync"
	"time"
)

// Kind enumerates the normalized event kinds from design section 3.4.
type Kind string

const (
	KindOutput     Kind = "output"      // model text / raw pane output
	KindStatus     Kind = "status"      // idle | thinking | awaiting_input | ...
	KindNeedsInput Kind = "needs_input" // the agent is waiting for the user
	KindToolCall   Kind = "tool_call"   // a tool invocation summary
	KindResult     Kind = "result"      // the final turn result
	KindError      Kind = "error"       // an error from the harness
)

// Source records whether the event came from a structured (native) transport or
// a raw PTY tail.
type Source string

const (
	SourceNative Source = "native"
	SourcePTY    Source = "pty"
)

// Event is the normalized cross-harness event (design 3.4). Confidence is set
// only when Kind was inferred by an LLM/heuristic pass (0 means "certain").
type Event struct {
	SessionID  string    `json:"session_id"`
	TS         time.Time `json:"ts"`
	Source     Source    `json:"source"`
	Kind       Kind      `json:"kind"`
	Text       string    `json:"text,omitempty"`
	Status     string    `json:"status,omitempty"`
	Tool       string    `json:"tool,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
}

// Bus is an in-process pub/sub hub. Subscribers get a buffered channel; a slow
// subscriber drops events rather than blocking publishers (bounded backpressure).
type Bus struct {
	mu   sync.RWMutex
	subs map[int]chan Event
	next int
	buf  int
}

// New returns a ready Bus. Each subscriber channel is buffered to bufSize.
func New() *Bus {
	return &Bus{subs: make(map[int]chan Event), buf: 128}
}

// Subscribe registers a new subscriber and returns its id plus a receive-only
// channel. Call Unsubscribe with the id when done.
func (b *Bus) Subscribe() (int, <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan Event, b.buf)
	b.subs[id] = ch
	return id, ch
}

// Unsubscribe removes and closes a subscriber channel.
func (b *Bus) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish fans an event out to every subscriber. A full subscriber buffer drops
// the event for that subscriber (never blocks the publisher).
func (b *Bus) Publish(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// slow subscriber: drop rather than block.
		}
	}
}

// SubscriberCount reports the number of live subscribers (useful for health).
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
