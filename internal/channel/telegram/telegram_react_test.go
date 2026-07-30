package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/session"
)

// ---- fixture: a Bot API stub that records every setMessageReaction ----

type reactCall struct {
	ChatID    string `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Reaction  []struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	} `json:"reaction"`
}

type botStub struct {
	mu     sync.Mutex
	calls  []reactCall
	sends  int
	reject bool // answer setMessageReaction with Telegram's REACTION_INVALID
	srv    *httptest.Server
}

func newBotStub(t *testing.T) *botStub {
	t.Helper()
	b := &botStub{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/setMessageReaction"):
			var c reactCall
			if err := json.Unmarshal(body, &c); err != nil {
				t.Errorf("setMessageReaction body not json: %s", body)
			}
			b.mu.Lock()
			b.calls = append(b.calls, c)
			reject := b.reject
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if reject {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: REACTION_INVALID"}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			b.mu.Lock()
			b.sends++
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":{"message_id":900}}`))
		default:
			t.Errorf("unexpected Bot API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// emojis returns the reaction emoji recorded so far, in call order.
func (b *botStub) emojis() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.calls))
	for _, c := range b.calls {
		if len(c.Reaction) > 0 {
			out = append(out, c.Reaction[0].Emoji)
		} else {
			out = append(out, "")
		}
	}
	return out
}

// waitEmojis polls until the recorded chain equals want, or the deadline passes.
// Reactions are queued onto a worker goroutine, so they are observed, not awaited.
func (b *botStub) waitEmojis(t *testing.T, want []string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = b.emojis()
		if len(got) >= len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- echo harness so RouteInbound can drive a real turn without claude ----

type echoHandle struct {
	id     string
	events chan eventbus.Event
}

func (h *echoHandle) ID() string { return h.id }

type echoAdapter struct{ needsInput bool }

func (echoAdapter) Name() string                         { return "echo" }
func (echoAdapter) Capabilities() harness.Capabilities   { return harness.Capabilities{} }
func (echoAdapter) Interrupt(harness.Handle) error       { return nil }
func (echoAdapter) Status(harness.Handle) harness.Status { return harness.StatusIdle }
func (echoAdapter) Pressure(harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (echoAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	return &echoHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 16)}, nil
}
func (echoAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &echoHandle{id: existing, events: make(chan eventbus.Event, 16)}, nil
}
func (echoAdapter) Teardown(h harness.Handle) error {
	close(h.(*echoHandle).events)
	return nil
}
func (echoAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*echoHandle).events
}
func (e echoAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	bh := h.(*echoHandle)
	if e.needsInput {
		bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindNeedsInput, Text: "which one?"}
	}
	bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindResult, Text: "reply: " + in.Text}
	return nil
}

var _ harness.Adapter = echoAdapter{}

// textUpdate builds a realistic text update (message_id / date / from present).
func textUpdate(updateID, chatID, msgID, date, fromID int64, text string) tgUpdate {
	m := &tgMessage{MessageID: msgID, Date: date, Text: text}
	m.Chat.ID = chatID
	m.From = &struct {
		ID int64 `json:"id"`
	}{ID: fromID}
	return tgUpdate{UpdateID: updateID, Message: m}
}

// ---- tests ----

// TestInboundCapturesProviderMetadata: the adapter no longer drops the provider
// message id (the react target), the send time, or the sender id.
func TestInboundCapturesProviderMetadata(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"}).WithReactions(false)
	a.base = b.srv.URL

	a.handleUpdate(textUpdate(1, 111, 6543, 1750000000, 123456789, "hi"))

	select {
	case m := <-a.Inbound():
		if m.MsgID != "6543" {
			t.Errorf("MsgID = %q, want 6543", m.MsgID)
		}
		if m.TS != 1750000000 {
			t.Errorf("TS = %d, want 1750000000", m.TS)
		}
		if m.Sender != "123456789" {
			t.Errorf("Sender = %q, want 123456789", m.Sender)
		}
	default:
		t.Fatal("expected an inbound message")
	}
}

// TestReceiptReactionOnText: an allowlisted text message gets 👀 immediately,
// with the chat id and message id from the update.
func TestReceiptReactionOnText(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	a.handleUpdate(textUpdate(1, 111, 6543, 1750000000, 123456789, "hi"))

	got := b.waitEmojis(t, []string{channel.ReactionReceived})
	if !sameStrings(got, []string{channel.ReactionReceived}) {
		t.Fatalf("reactions = %v, want [%s]", got, channel.ReactionReceived)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls[0].ChatID != "111" || b.calls[0].MessageID != 6543 {
		t.Errorf("reaction target = %s/%d, want 111/6543", b.calls[0].ChatID, b.calls[0].MessageID)
	}
	if b.calls[0].Reaction[0].Type != "emoji" {
		t.Errorf("reaction type = %q, want emoji", b.calls[0].Reaction[0].Type)
	}
}

// TestReceiptReactionOnMedia: a photo message also gets the receipt reaction,
// fired BEFORE the (slow) media pipeline runs. Media is disabled here, so the
// adapter declines by text, but the 👀 must already be on the message.
func TestReceiptReactionOnMedia(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	m := &tgMessage{MessageID: 77, Date: 1750000001, Photo: []tgPhotoSize{{FileID: "f1", FileSize: 10}}}
	m.Chat.ID = 111
	a.handleUpdate(tgUpdate{UpdateID: 1, Message: m})

	got := b.waitEmojis(t, []string{channel.ReactionReceived})
	if !sameStrings(got, []string{channel.ReactionReceived}) {
		t.Fatalf("reactions = %v, want [%s]", got, channel.ReactionReceived)
	}
}

// TestReactionsDisabledMakesNoCalls: reactions=false parks the whole chain,
// end to end (receipt AND the turn lifecycle).
func TestReactionsDisabledMakesNoCalls(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"}).WithReactions(false)
	a.base = b.srv.URL

	runTurn(t, a, echoAdapter{}, textUpdate(1, 111, 6543, 1750000000, 123456789, "hi"))

	time.Sleep(150 * time.Millisecond)
	if got := b.emojis(); len(got) != 0 {
		t.Fatalf("reactions with reactions=false: %v, want none", got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sends != 1 {
		t.Errorf("sendMessage calls = %d, want 1 (the reply still goes out)", b.sends)
	}
}

// TestLifecycleReactionChain: the full ported chain on one turn,
// 👀 receipt -> ⚡ working -> 👍 done, in that order.
func TestLifecycleReactionChain(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	runTurn(t, a, echoAdapter{}, textUpdate(1, 111, 6543, 1750000000, 123456789, "hi"))

	want := []string{channel.ReactionReceived, channel.ReactionWorking, channel.ReactionDone}
	got := b.waitEmojis(t, want)
	if !sameStrings(got, want) {
		t.Fatalf("reaction chain = %v, want %v", got, want)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c.ChatID != "111" || c.MessageID != 6543 {
			t.Errorf("reaction targeted %s/%d, want 111/6543", c.ChatID, c.MessageID)
		}
	}
}

// TestNeedsInputReaction: a needs_input event surfaces 🤔 mid-turn and the
// terminal glyph still lands afterwards.
func TestNeedsInputReaction(t *testing.T) {
	b := newBotStub(t)
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	runTurn(t, a, echoAdapter{needsInput: true}, textUpdate(1, 111, 6543, 1750000000, 123456789, "hi"))

	want := []string{
		channel.ReactionReceived, channel.ReactionWorking,
		channel.ReactionNeedsInput, channel.ReactionDone,
	}
	got := b.waitEmojis(t, want)
	if !sameStrings(got, want) {
		t.Fatalf("reaction chain = %v, want %v", got, want)
	}
}

// TestRejectedEmojiDoesNotBreakTurn: Telegram answers REACTION_INVALID for every
// call; the turn still completes and the reply still goes out.
func TestRejectedEmojiDoesNotBreakTurn(t *testing.T) {
	b := newBotStub(t)
	b.reject = true
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	runTurn(t, a, echoAdapter{}, textUpdate(1, 111, 6543, 1750000000, 123456789, "hi"))

	want := []string{channel.ReactionReceived, channel.ReactionWorking, channel.ReactionDone}
	got := b.waitEmojis(t, want)
	if !sameStrings(got, want) {
		t.Fatalf("reaction chain = %v, want %v (rejections must not stop the chain)", got, want)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sends != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (turn must complete despite rejected reactions)", b.sends)
	}
}

// TestAckRejectionIsReportedSynchronously covers setReaction's error path
// directly (the queue swallows it by design, but the call must classify it).
func TestAckRejectionIsReportedSynchronously(t *testing.T) {
	b := newBotStub(t)
	b.reject = true
	a := New("tok", []string{"111"})
	a.base = b.srv.URL

	err := a.setReaction("111", 6543, "✅") // check mark: NOT on Telegram's whitelist
	if err == nil {
		t.Fatal("expected setReaction to report the REACTION_INVALID rejection")
	}
	if !strings.Contains(err.Error(), "REACTION_INVALID") {
		t.Errorf("error = %v, want it to carry the API description", err)
	}
	// And the interface-level Ack still swallows it (never fails a turn).
	if err := a.Ack("111", "6543", "✅"); err != nil {
		t.Errorf("Ack returned %v, want nil (best-effort contract)", err)
	}
}

// runTurn drives one update through the adapter and session.RouteInbound,
// exactly as cmd/agentd/main.go wires it, and waits for the turn to finish.
func runTurn(t *testing.T, a *Adapter, ha harness.Adapter, u tgUpdate) {
	t.Helper()
	bus := eventbus.New()
	mgr := session.NewManager(ha, bus, nil)
	mgr.Policy = session.Policy{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a.handleUpdate(u)

	var in channel.InboundMsg
	select {
	case in = <-a.Inbound():
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound message emitted")
	}
	done := make(chan error, 1)
	go func() { done <- mgr.RouteInbound(ctx, a, in, map[string]string{}, "", "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RouteInbound: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RouteInbound did not complete")
	}
}
