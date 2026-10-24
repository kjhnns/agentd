package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kjhnns/agentd/internal/channel"
)

func postJSON(t *testing.T, a *Adapter, body string) (int, Message, bool) {
	t.Helper()
	rr := do(t, a.Handler(), http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(body), "application/json")
	var out struct {
		Message   Message `json:"message"`
		Duplicate bool    `json:"duplicate"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out.Message, out.Duplicate
}

func drainInbound(a *Adapter) []channel.InboundMsg {
	var out []channel.InboundMsg
	for {
		select {
		case in := <-a.inbound:
			out = append(out, in)
		default:
			return out
		}
	}
}

// TestRepeatedRequestIDAnswersTheOriginalReceipt is the D5 regression: the
// outbox retries a POST whose receipt was lost, and the server stored and
// queued it again (a second turn for one question). Now a repeat of the
// same request_id answers the original receipt and queues nothing.
func TestRepeatedRequestIDAnswersTheOriginalReceipt(t *testing.T) {
	a, st := newAdapter(t)
	code, first, _ := postJSON(t, a, `{"text":"book the train","request_id":"outbox-1"}`)
	if code != http.StatusAccepted {
		t.Fatalf("first post: %d", code)
	}
	code, again, dup := postJSON(t, a, `{"text":"book the train","request_id":"outbox-1"}`)
	if code != http.StatusAccepted || again.ID != first.ID || !dup {
		t.Fatalf("repeat: %d %+v dup=%v, want the original receipt %s", code, again, dup, first.ID)
	}
	if n := len(drainInbound(a)); n != 1 {
		t.Fatalf("queued turns = %d, want 1", n)
	}
	users := 0
	msgs, _, _ := st.Tail(50)
	for _, m := range msgs {
		if m.Role == RoleUser {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("stored user messages = %d, want 1", users)
	}
	if again.Inbound != nil {
		t.Fatal("the receipt leaked the stored inbound payload")
	}
}

// TestPersistFailureStoresNothing: a write or fsync error answers 503 and
// leaves neither a message nor a queued turn behind.
func TestPersistFailureStoresNothing(t *testing.T) {
	a, st := newAdapter(t)
	st.failWrite = func() error { return errors.New("disk full") }
	code, _, _ := postJSON(t, a, `{"text":"hello","request_id":"r1"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", code)
	}
	if msgs, _, _ := st.Tail(50); len(msgs) != 0 {
		t.Fatalf("stored %d messages after a failed write", len(msgs))
	}
	if len(drainInbound(a)) != 0 {
		t.Fatal("a turn was queued for a message that was not stored")
	}
	st.failWrite = nil
	if code, _, _ := postJSON(t, a, `{"text":"hello","request_id":"r1"}`); code != http.StatusAccepted {
		t.Fatalf("retry after the disk recovered: %d", code)
	}
}

// TestFullQueueRejectsAndRetryReadmits: a full queue marks a request
// rejected (retryable); the retry with the same id re-admits the same
// message. Without a request_id the old behaviour stays: failed.
func TestFullQueueRejectsAndRetryReadmits(t *testing.T) {
	a, st := newAdapter(t)
	for i := 0; i < cap(a.inbound); i++ {
		postJSON(t, a, `{"text":"fill `+strconv.Itoa(i)+`"}`)
	}
	code, _, _ := postJSON(t, a, `{"text":"late","request_id":"r-late"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("full queue: %d", code)
	}
	rej, ok := st.LookupRequest("r-late")
	if !ok || rej.Status != StatusRejected {
		t.Fatalf("rejected request = %+v %v", rej, ok)
	}
	code, _, _ = postJSON(t, a, `{"text":"old client"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("old client on a full queue: %d", code)
	}
	drainInbound(a)
	code, re, dup := postJSON(t, a, `{"text":"late","request_id":"r-late"}`)
	if code != http.StatusAccepted || dup || re.ID != rej.ID || re.Status != StatusQueued {
		t.Fatalf("retry: %d %+v dup=%v", code, re, dup)
	}
	q := drainInbound(a)
	if len(q) != 1 || q[0].MsgID != rej.ID || q[0].RequestID != "r-late" {
		t.Fatalf("queued after retry = %+v", q)
	}
}

// TestRestartReconciliation: queued requests are re-enqueued with their
// stored payload; working ones get their committed result delivered or fail
// with the interrupted notice.
func TestRestartReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := New("wt", st)
	// Nobody consumes: q stays queued.
	_, q, _ := postJSON(t, a, `{"text":"queued one","request_id":"rq"}`)
	_, w1, _ := postJSON(t, a, `{"text":"working, committed","request_id":"rw1"}`)
	_, w2, _ := postJSON(t, a, `{"text":"working, lost","request_id":"rw2"}`)
	_, n1, _ := postJSON(t, a, `{"text":"needs input, lost"}`)
	_ = a.Ack("", w1.ID, channel.ReactionWorking)
	_ = a.Ack("", w2.ID, channel.ReactionWorking)
	_ = a.Ack("", n1.ID, channel.ReactionNeedsInput)
	st.Close()

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b := New("wt", st2)
	var delivered []string
	req, del, failed := b.Reconcile(context.Background(),
		func(msgID, reqID string) (CommittedTurn, bool) {
			if msgID == w1.ID {
				return CommittedTurn{SessionID: "s1", Result: "the committed answer"}, true
			}
			return CommittedTurn{}, false
		},
		func(ctx context.Context, in channel.InboundMsg, sid, result string) error {
			delivered = append(delivered, in.MsgID+"="+result)
			_, err := b.Send(ctx, channel.OutboundMsg{Text: result, InReplyTo: in.MsgID})
			return err
		})
	if req != 1 || del != 1 || failed != 2 {
		t.Fatalf("reconcile = %d requeued, %d delivered, %d failed", req, del, failed)
	}
	in := drainInbound(b)
	if len(in) != 1 || in[0].MsgID != q.ID || in[0].Text != "queued one" || in[0].RequestID != "rq" {
		t.Fatalf("re-enqueued = %+v", in)
	}
	if len(delivered) != 1 || delivered[0] != w1.ID+"=the committed answer" {
		t.Fatalf("delivered = %v", delivered)
	}
	for id, want := range map[string]string{w1.ID: StatusDone, w2.ID: StatusFailed, n1.ID: StatusFailed} {
		if m, _ := st2.Get(id); m.Status != want {
			t.Fatalf("%s status = %s, want %s", m.Text, m.Status, want)
		}
	}
	if !b.hasFailure(w2.ID) {
		t.Fatal("the lost turn has no failure notice")
	}
}

// TestRequestIndexOutlivesEviction is the D5b regression: the store dropped
// an evicted message from its lookup map, so a retry of an old request
// (receipt lost long ago) ran the turn again. The request index keeps it
// across eviction, compaction and a restart.
func TestRequestIndexOutlivesEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, err := openStoreWith(path, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	a := New("wt", st)
	_, orig, _ := postJSON(t, a, `{"text":"old question","request_id":"keep-me"}`)
	_ = a.Ack("", orig.ID, channel.ReactionDone)
	for i := 0; i < 5; i++ {
		st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "n" + strconv.Itoa(i)})
	}
	if _, live := st.Get(orig.ID); live {
		t.Fatal("setup: the message was not evicted")
	}
	drainInbound(a)
	if code, m, dup := postJSON(t, a, `{"text":"old question","request_id":"keep-me"}`); code != http.StatusAccepted || !dup || m.ID != orig.ID {
		t.Fatalf("retry after eviction: %d %+v dup=%v", code, m, dup)
	}
	st.Close()

	// Compaction (more lines than compactAt) and a restart.
	st2, err := openStoreWith(path, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := st2.replayCount(); n > 3+1 {
		t.Fatalf("compaction kept %d lines", n)
	}
	b := New("wt", st2)
	if code, m, dup := postJSON(t, b, `{"text":"old question","request_id":"keep-me"}`); code != http.StatusAccepted || !dup || m.ID != orig.ID {
		t.Fatalf("retry after compaction and restart: %d %+v dup=%v", code, m, dup)
	}
	if len(drainInbound(b)) != 0 {
		t.Fatal("a retry of an evicted request queued a second turn")
	}
	if msgs, _, _ := st2.Tail(50); len(msgs) != 3 {
		t.Fatalf("conversation after reopen has %d messages, want 3 (the evicted request stays out)", len(msgs))
	}
}

// TestTrimNeverEvictsAnUnfinishedRequest: a queued or working message stays
// in the store however many messages follow it.
func TestTrimNeverEvictsAnUnfinishedRequest(t *testing.T) {
	st, _ := openStoreWith("", 1000, 3)
	a := New("wt", st)
	_, q, _ := postJSON(t, a, `{"text":"still queued","request_id":"q"}`)
	for i := 0; i < 10; i++ {
		st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "n"})
	}
	if m, ok := st.Get(q.ID); !ok || m.Status != StatusQueued {
		t.Fatal("trim evicted a queued request")
	}
	if len(st.Unfinished()) != 1 {
		t.Fatal("reconciliation would not find the queued request")
	}
}
