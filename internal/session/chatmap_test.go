package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
)

// TestRouteInboundConcurrentSameChatOneSession is the D4 regression: every
// inbound message is routed on its own goroutine, and the chat-to-session
// map used to be a plain map written from all of them (a data race, and two
// messages arriving together could each create a session). The manager-owned
// ChatMap is a synchronised get-or-create: one chat, one session.
func TestRouteInboundConcurrentSameChatOneSession(t *testing.T) {
	a := &scriptedAdapter{emit: func(id string, out chan<- eventbus.Event) {
		out <- eventbus.Event{SessionID: id, Kind: eventbus.KindResult, Text: "ok"}
	}}
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{TurnTimeout: 5 * time.Second}
	chats := NewChatMap(LaneMain)
	ch := &recordingChannel{}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := channel.InboundMsg{Channel: "telegram", UserID: "chat1", Text: "hi"}
			if err := mgr.RouteInbound(context.Background(), ch, in, chats, "", ""); err != nil {
				t.Errorf("RouteInbound: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(mgr.List()); n != 1 {
		t.Fatalf("sessions = %d, want 1 for one chat", n)
	}
	if id, ok := chats.Get("chat1"); !ok || id == "" {
		t.Fatal("chat map has no session for chat1")
	}
}

// chatMapWith is a ChatMap with one chat already bound (tests).
func chatMapWith(chat, id string) *ChatMap {
	c := NewChatMap(LaneMain)
	c.Set(chat, id)
	return c
}
