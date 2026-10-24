package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/runlog"
)

// cancelAdapter is an echo adapter whose send of "cancel" returns a context
// error while its own context is still alive (a harness-internal deadline):
// the harness turn may still be running.
type cancelAdapter struct{ *echoAdapter }

func (c *cancelAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	if in.Text == "cancel" {
		c.mu.Lock()
		c.sends = append(c.sends, in.Text)
		c.mu.Unlock()
		return fmt.Errorf("inner: %w", context.DeadlineExceeded)
	}
	return c.echoAdapter.Send(ctx, h, in)
}

// TestSendReturningOnACancelStillRetiresTheTurn: a send that returned with a
// context error does not prove the harness stopped, so the turn is
// interrupted and the process replaced before the session is released.
func TestSendReturningOnACancelStillRetiresTheTurn(t *testing.T) {
	a := &cancelAdapter{newEchoAdapter()}
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{}
	s, _ := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	if _, err := mgr.SendAndCollect(context.Background(), s.ID, "cancel", TurnOptions{Timeout: time.Second}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	a.mu.Lock()
	interrupted, starts := a.interrupted, len(a.started)
	a.mu.Unlock()
	if interrupted != 1 || starts != 2 {
		t.Fatalf("interrupts=%d starts=%d, want 1 and 2", interrupted, starts)
	}
}

// lagAdapter is structured: its terminal event reaches the event channel
// only after Send returned (as if the pump lagged behind).
type lagAdapter struct {
	*echoAdapter
	lag time.Duration
}

func (l *lagAdapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{StructuredEvents: true}
}

func (l *lagAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	sh := h.(*scriptedHandle)
	l.mu.Lock()
	l.sends = append(l.sends, in.Text)
	out := sh.events
	l.mu.Unlock()
	lag := time.Duration(0)
	if in.Text == "A" {
		lag = l.lag
	}
	go func() {
		time.Sleep(lag)
		out <- eventbus.Event{SessionID: sh.id, Kind: eventbus.KindResult, Text: "re:" + in.Text}
	}()
	return nil
}

// TestLaggingTerminalEventIsNotStampedWithTheNextTurn (review round 1): the
// pump stamps the turn id when it reads an event. A turn used to release the
// session after a 2 s grace even when its terminal event was not read yet,
// so the next turn collected it. Now a structured harness's turn waits until
// the pump stamped its terminal event.
func TestLaggingTerminalEventIsNotStampedWithTheNextTurn(t *testing.T) {
	a := &lagAdapter{echoAdapter: newEchoAdapter(), lag: resultGrace + 500*time.Millisecond}
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{}
	s, _ := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	resA := make(chan string, 1)
	go func() {
		r, _ := mgr.SendAndCollect(context.Background(), s.ID, "A", TurnOptions{Timeout: 10 * time.Second})
		resA <- r
	}()
	waitFor(t, func() bool { return a.sendsContaining("A") == 1 })
	gotB, err := mgr.SendAndCollect(context.Background(), s.ID, "B", TurnOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if gotA := <-resA; gotA != "re:A" || gotB != "re:B" {
		t.Fatalf("A=%q B=%q, want re:A and re:B", gotA, gotB)
	}
}

// failingDurable is a channel whose durable "working" write fails.
type failingDurable struct{ recordingChannel }

func (f *failingDurable) AckDurable(chatID, msgID, reaction string) error {
	return errors.New("disk full")
}

// TestTurnDoesNotStartWithoutADurableWorkingState (review round 1): when the
// wrist cannot persist "working", the harness never sees the turn (a restart
// would otherwise run it a second time).
func TestTurnDoesNotStartWithoutADurableWorkingState(t *testing.T) {
	a := newEchoAdapter()
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{TurnTimeout: time.Second}
	in := channel.InboundMsg{Channel: "watch", UserID: "c", MsgID: "m1", Text: "do it"}
	if err := mgr.RouteInbound(context.Background(), &failingDurable{}, in, NewChatMap(LaneMain), t.TempDir(), ""); err == nil {
		t.Fatal("the turn ran although working was not persisted")
	}
	if n := a.sendsContaining("do it"); n != 0 {
		t.Fatalf("harness sends = %d, want 0", n)
	}
}

// TestRecapKeepsLegacyRecordsWrittenAfterARollback (review round 1): an
// older binary (after a rollback) writes unmarked inbound/outbound records
// after turn records exist; they stay in the recap.
func TestRecapKeepsLegacyRecordsWrittenAfterARollback(t *testing.T) {
	const chat = "42"
	path := writeThread(t, []func(*runlog.Log){
		func(l *runlog.Log) {
			_ = l.Append("inbound", map[string]any{"channel": "telegram", "user_id": chat, "msg_id": "1", "text": "new binary question", "rec": "turn"})
		},
		turnRec(TurnRecord{TurnID: "t1", Chat: chat, Conv: ConvMain, MessageID: "1", UserText: "new binary question", Result: "new binary answer"}),
		func(l *runlog.Log) {
			_ = l.Append("outbound", map[string]string{"chat": chat, "session": "s", "msg_id": "2", "text": "new binary answer", "rec": "turn"})
		},
		inbound(chat, "3", "old binary question"),
		outbound(chat, "s", "4", "old binary answer"),
	})
	got, _ := ThreadRecap(path, "telegram:"+chat, RecapPolicy{}, time.Now().UTC())
	for _, want := range []string{"new binary question", "new binary answer", "old binary question", "old binary answer"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("%q appears %d times:\n%s", want, strings.Count(got, want), got)
		}
	}
}

// silentStructured is structured but never reports a result.
type silentStructured struct{ *echoAdapter }

func (s *silentStructured) Capabilities() harness.Capabilities {
	return harness.Capabilities{StructuredEvents: true}
}
func (s *silentStructured) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	s.mu.Lock()
	s.sends = append(s.sends, in.Text)
	s.mu.Unlock()
	return nil
}

// TestMissingResultIsAFailure (review round 2): a structured harness that
// finished a turn without a readable result is a failed turn, never an
// empty success delivered as "(no result)".
func TestMissingResultIsAFailure(t *testing.T) {
	old := drainWait
	drainWait = 200 * time.Millisecond
	defer func() { drainWait = old }()
	a := &silentStructured{newEchoAdapter()}
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{}
	s, _ := mgr.Create(context.Background(), "", t.TempDir(), "", "t")
	_, err := mgr.SendAndCollect(context.Background(), s.ID, "q", TurnOptions{Timeout: 5 * time.Second})
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
}

// seqChannel records sends and acks in one ordered list.
type seqChannel struct {
	recordingChannel
	mu3 sync.Mutex
	log []string
}

func (c *seqChannel) Ack(chatID, msgID, reaction string) error {
	c.mu3.Lock()
	c.log = append(c.log, "ack:"+reaction)
	c.mu3.Unlock()
	return nil
}
func (c *seqChannel) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	c.mu3.Lock()
	c.log = append(c.log, fmt.Sprintf("send:failure=%v", m.Failure))
	c.mu3.Unlock()
	return channel.SendReceipt{ID: "x"}, nil
}

// TestFailureNoticeGoesOutBeforeTheErrorGlyph (review round 2): the wrist
// persists the status, so the notice is stored before "failed".
func TestFailureNoticeGoesOutBeforeTheErrorGlyph(t *testing.T) {
	a := &cancelAdapter{newEchoAdapter()}
	mgr := NewManager(a, eventbus.New(), nil)
	mgr.Policy = Policy{TurnTimeout: time.Second}
	ch := &seqChannel{}
	in := channel.InboundMsg{Channel: "watch", UserID: "c", MsgID: "m", Text: "cancel"}
	if err := mgr.RouteInbound(context.Background(), ch, in, NewChatMap(LaneMain), t.TempDir(), ""); err == nil {
		t.Fatal("want a failed turn")
	}
	want := []string{"ack:" + channel.ReactionWorking, "send:failure=true", "ack:" + channel.ReactionError}
	if fmt.Sprint(ch.log) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", ch.log, want)
	}
}

// TestMarkedRecordsWithoutTheirTurnRecordStayInTheRecap (review round 2):
// when the turn record could not be written, the inbound/outbound records of
// that message are read instead.
func TestMarkedRecordsWithoutTheirTurnRecordStayInTheRecap(t *testing.T) {
	const chat = "42"
	path := writeThread(t, []func(*runlog.Log){
		func(l *runlog.Log) {
			_ = l.Append("inbound", map[string]any{"channel": "telegram", "user_id": chat, "msg_id": "1", "text": "lost turn question", "rec": "turn", "ref": "r1"})
		},
		func(l *runlog.Log) {
			_ = l.Append("outbound", map[string]string{"chat": chat, "session": "s", "msg_id": "2", "text": "lost turn answer", "rec": "turn", "ref": "r1"})
		},
		func(l *runlog.Log) {
			_ = l.Append("inbound", map[string]any{"channel": "telegram", "user_id": chat, "msg_id": "3", "text": "kept question", "rec": "turn", "ref": "r2"})
		},
		turnRec(TurnRecord{TurnID: "t2", Chat: chat, Conv: ConvMain, MessageID: "3", Ref: "r2", UserText: "kept question", Result: "kept answer"}),
	})
	got, _ := ThreadRecap(path, "telegram:"+chat, RecapPolicy{}, time.Now().UTC())
	for _, want := range []string{"lost turn question", "lost turn answer", "kept question", "kept answer"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("%q appears %d times:\n%s", want, strings.Count(got, want), got)
		}
	}
}

// flakyDurable is a durable channel whose Send fails a set number of times.
type flakyDurable struct {
	seqChannel
	fail int
}

func (f *flakyDurable) AckDurable(chatID, msgID, reaction string) error {
	return f.Ack(chatID, msgID, reaction)
}
func (f *flakyDurable) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	f.mu3.Lock()
	defer f.mu3.Unlock()
	if f.fail > 0 {
		f.fail--
		f.log = append(f.log, "send:err")
		return channel.SendReceipt{}, errors.New("disk full")
	}
	f.log = append(f.log, fmt.Sprintf("send:failure=%v", m.Failure))
	return channel.SendReceipt{ID: "x"}, nil
}

// TestDeliveryFailureOfACommittedAnswer (review round 3): when only the
// delivery of a successful turn fails, the wrist request is retried and,
// if it still fails, left unfinished (no failure notice, no "failed"), so
// the restart reconciliation delivers the stored answer.
func TestDeliveryFailureOfACommittedAnswer(t *testing.T) {
	old := deliveryRetries
	deliveryRetries = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { deliveryRetries = old }()
	mgr := NewManager(newEchoAdapter(), eventbus.New(), nil)
	mgr.Policy = Policy{TurnTimeout: time.Second}
	in := channel.InboundMsg{Channel: "watch", UserID: "c", MsgID: "m", Text: "q"}

	ch := &flakyDurable{fail: 1}
	if err := mgr.RouteInbound(context.Background(), ch, in, NewChatMap(LaneMain), t.TempDir(), ""); err != nil {
		t.Fatalf("one failed delivery is retried: %v", err)
	}
	if fmt.Sprint(ch.log) != fmt.Sprint([]string{"ack:" + channel.ReactionWorking, "send:err", "send:failure=false", "ack:" + channel.ReactionDone}) {
		t.Fatalf("log = %v", ch.log)
	}

	ch = &flakyDurable{fail: 10}
	_ = mgr.RouteInbound(context.Background(), ch, in, NewChatMap(LaneMain), t.TempDir(), "")
	for _, e := range ch.log {
		if e == "send:failure=true" || e == "ack:"+channel.ReactionError {
			t.Fatalf("a committed answer was turned into a failure: %v", ch.log)
		}
	}
}

// TestRecapShowsAFailedTurnOnce (review round 3): the failure notice of a
// turn is skipped (its ref is covered), so the turn record renders it.
func TestRecapShowsAFailedTurnOnce(t *testing.T) {
	const chat = "42"
	path := writeThread(t, []func(*runlog.Log){
		turnRec(TurnRecord{TurnID: "t1", Chat: chat, Conv: ConvMain, Ref: "r1", UserText: "book it", Result: "found two trains", Error: "turn timed out"}),
		func(l *runlog.Log) {
			_ = l.Append("outbound", map[string]string{"chat": chat, "session": "s", "text": "I stopped working on that message", "kind": "failure-notice", "rec": "turn", "ref": "r1"})
		},
	})
	got, _ := ThreadRecap(path, "telegram:"+chat, RecapPolicy{}, time.Now().UTC())
	if strings.Count(got, "this request failed: turn timed out") != 1 || !strings.Contains(got, "found two trains") || strings.Contains(got, "I stopped working") {
		t.Fatalf("recap:\n%s", got)
	}
}

// noticeFailsDurable is a durable channel whose every Send fails.
type noticeFailsDurable struct{ seqChannel }

func (f *noticeFailsDurable) AckDurable(chatID, msgID, reaction string) error {
	return f.Ack(chatID, msgID, reaction)
}
func (f *noticeFailsDurable) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	return channel.SendReceipt{}, errors.New("disk full")
}

// TestNoFailedWithoutItsNotice (review round 4): on a durable channel a
// request is not marked failed when its notice could not be stored.
func TestNoFailedWithoutItsNotice(t *testing.T) {
	mgr := NewManager(&cancelAdapter{newEchoAdapter()}, eventbus.New(), nil)
	mgr.Policy = Policy{TurnTimeout: time.Second}
	ch := &noticeFailsDurable{}
	in := channel.InboundMsg{Channel: "watch", UserID: "c", MsgID: "m", Text: "cancel"}
	_ = mgr.RouteInbound(context.Background(), ch, in, NewChatMap(LaneMain), t.TempDir(), "")
	for _, e := range ch.log {
		if e == "ack:"+channel.ReactionError {
			t.Fatalf("failed without a stored notice: %v", ch.log)
		}
	}
}

// TestRecapKeepsTheSummaryOfALongAnswer (review round 6): the summary of a
// split answer is its own recap entry, so a long body cannot cut it.
func TestRecapKeepsTheSummaryOfALongAnswer(t *testing.T) {
	const chat = "42"
	long := strings.Repeat("detail ", 400)
	path := writeThread(t, []func(*runlog.Log){
		turnRec(TurnRecord{TurnID: "t1", Chat: chat, Conv: ConvMain, UserText: "plan?", Result: long + "\n" + SummaryMarker + "\nPick 1 or 2?"}),
	})
	got, _ := ThreadRecap(path, "telegram:"+chat, RecapPolicy{}, time.Now().UTC())
	if !strings.Contains(got, "Pick 1 or 2?") {
		t.Fatalf("summary lost:\n%s", got)
	}
}
