package session

import (
	"context"
	"sync"

	"github.com/kjhnns/agentd/internal/channel"
)

// Lane classifies a session for the per-workspace interactive lock and for
// FindByWorkspace. A channel session (Telegram, the wrist, the web UI) is
// LaneMain. LaneSide is reserved for the side session of iOS conversations
// (a later release). LaneOther is every session the scheduler or the API
// creates; it keeps the old behaviour outside the interactive lock.
type Lane string

const (
	LaneMain  Lane = "main"
	LaneSide  Lane = "side"
	LaneOther Lane = "other"
)

// interactive reports whether sessions of this lane take the per-workspace
// interactive lock.
func (l Lane) interactive() bool { return l == LaneMain || l == LaneSide }

// ChatMap maps a channel chat id to its live session id. It is owned by the
// wiring (one per channel, or one shared by Telegram and the wrist) and is
// safe for the one goroutine per inbound message that RouteInbound runs on.
// Get-or-create runs under the map's lock, so two messages of one chat that
// arrive together share one session instead of each creating one.
type ChatMap struct {
	mu   sync.Mutex
	lane Lane
	m    map[string]string
}

// NewChatMap returns an empty map whose sessions are created in lane.
func NewChatMap(lane Lane) *ChatMap {
	if lane == "" {
		lane = LaneMain
	}
	return &ChatMap{lane: lane, m: map[string]string{}}
}

// Get returns the session id recorded for chat.
func (c *ChatMap) Get(chat string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.m[chat]
	return id, ok
}

// Set records the session id for chat (wiring and tests).
func (c *ChatMap) Set(chat, id string) {
	c.mu.Lock()
	c.m[chat] = id
	c.mu.Unlock()
}

// Lane is the lane new sessions of this map are created in.
func (c *ChatMap) Lane() Lane { return c.lane }

// sessionForChat returns the live session of chat, creating one when there is
// none or the recorded one was reclaimed. The whole check-and-create runs
// under the map's lock.
func (m *Manager) sessionForChat(ctx context.Context, chats *ChatMap, chat, cwd, model string) (string, error) {
	chats.mu.Lock()
	defer chats.mu.Unlock()
	if id, ok := chats.m[chat]; ok {
		// A session the idle-GC reclaimed (process freed) no longer exists; the
		// next inbound lazily re-hydrates a FRESH one from the workspace
		// artifacts (context.md + memory), which is exactly the reclaim contract.
		if _, alive := m.Get(id); alive {
			return id, nil
		}
	}
	s, err := m.CreateInLane(ctx, chats.lane, "", cwd, model, "telegram:"+chat)
	if err != nil {
		return "", err
	}
	chats.m[chat] = s.ID
	return s.ID, nil
}

// DispatchInOrder handles the inbound messages one after the other, in queue
// order, on the caller's goroutine. The wrist uses it: its messages share one
// session and are serial anyway, a restart re-enqueues queued requests in
// order (so a follow-up never runs before the request it refers to), and a
// message waits in the bounded inbound queue rather than in an unbounded
// chain of goroutines, so admission stays bounded. It returns when in is
// closed.
func DispatchInOrder(in <-chan channel.InboundMsg, handle func(channel.InboundMsg)) {
	for msg := range in {
		handle(msg)
	}
}
