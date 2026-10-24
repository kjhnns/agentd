package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/runlog"
)

// TestRecapKeepsWristQuestionsInTheSharedSession is the D6 regression. With
// share_session the wrist drives the Telegram chat's session, but the recap
// matched inbound records by the "telegram" channel name, so every wrist
// question was missing from the transcript a fresh process got. The recap
// now reads the `turn` records RunTurn writes under ownership, for both
// channels.
func TestRecapKeepsWristQuestionsInTheSharedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runlog.jsonl")
	rl, err := runlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), rl)
	mgr.Policy = Policy{TurnTimeout: 5 * time.Second}
	chats := NewChatMap(LaneMain)
	ch := &recordingChannel{}
	ctx := context.Background()
	for _, in := range []channel.InboundMsg{
		{Channel: "telegram", UserID: "7597951120", MsgID: "100", Text: "telegram question"},
		{Channel: "watch", UserID: "7597951120", MsgID: "w1", Text: "wrist question"},
	} {
		if err := mgr.RouteInbound(ctx, ch, in, chats, t.TempDir(), ""); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := chats.Get("7597951120")
	if err := mgr.Reset(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	prompt := a.started[len(a.started)-1].SystemPrompt
	a.mu.Unlock()
	for _, want := range []string{"User: telegram question", "You: re:telegram question", "User: wrist question", "You: re:wrist question"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("recap after reset is missing %q:\n%s", want, prompt)
		}
	}
	for _, q := range []string{"User: wrist question", "User: telegram question"} {
		if n := strings.Count(prompt, q); n != 1 {
			t.Fatalf("%q appears %d times in the recap, want once:\n%s", q, n, prompt)
		}
	}
}

func turnRec(tr TurnRecord) func(*runlog.Log) {
	return func(l *runlog.Log) { _ = l.Append("turn", tr) }
}

// TestRecapReadsOnlyTheCurrentConversationEpoch: legacy inbound/outbound
// records count only before the chat's first turn record and only in epoch
// 0; after a conversation_start only that epoch's turns are replayed.
func TestRecapReadsOnlyTheCurrentConversationEpoch(t *testing.T) {
	const chat = "42"
	path := writeThread(t, []func(*runlog.Log){
		inbound(chat, "1", "legacy question"),
		outbound(chat, "s", "2", "legacy answer"),
		inbound(chat, "3", "epoch zero question"), // written before its turn record: not doubled
		turnRec(TurnRecord{TurnID: "t1", Chat: chat, Conv: ConvMain, MessageID: "3", UserText: "epoch zero question", Result: "epoch zero answer"}),
	})
	got, err := ThreadRecap(path, "telegram:"+chat, RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"legacy question", "legacy answer", "User: epoch zero question", "You: epoch zero answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("epoch 0 recap misses %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "epoch zero question") != 1 {
		t.Fatalf("question doubled:\n%s", got)
	}

	path = writeThread(t, []func(*runlog.Log){
		inbound(chat, "1", "legacy question"),
		turnRec(TurnRecord{TurnID: "t1", Chat: chat, Conv: ConvMain, UserText: "old conversation"}),
		func(l *runlog.Log) {
			_ = l.Append("conversation_start", map[string]any{"chat": chat, "conv": ConvMain, "epoch": 1})
		},
		turnRec(TurnRecord{TurnID: "t2", Chat: chat, Conv: ConvMain, ConversationEpoch: 1, UserText: "new conversation", Result: "fresh"}),
	})
	got, _ = ThreadRecap(path, "telegram:"+chat, RecapPolicy{}, time.Now().UTC())
	if strings.Contains(got, "legacy question") || strings.Contains(got, "old conversation") || !strings.Contains(got, "new conversation") {
		t.Fatalf("epoch 1 recap:\n%s", got)
	}
}

// TestTurnRecordIsWrittenUnderOwnership: RouteInbound's turn leaves one turn
// record with the message identity, the epoch and the process generation.
func TestTurnRecordIsWrittenUnderOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runlog.jsonl")
	rl, _ := runlog.Open(path)
	defer rl.Close()
	mgr := NewManager(newEchoAdapter(), eventbus.New(), rl)
	mgr.Policy = Policy{TurnTimeout: 5 * time.Second}
	in := channel.InboundMsg{Channel: "watch", UserID: "c1", MsgID: "m9", Text: "hello"}
	if err := mgr.RouteInbound(context.Background(), &recordingChannel{}, in, NewChatMap(LaneMain), t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	tr, _, ok := mgr.FindTurn("m9", "")
	if !ok {
		t.Fatal("no turn record for m9")
	}
	if tr.Chat != "c1" || tr.Conv != ConvMain || tr.UserText != "hello" || tr.Result != "re:hello" || tr.TurnID == "" || tr.Session == "" {
		t.Fatalf("turn record = %+v", tr)
	}
}
