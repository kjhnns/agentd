package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kjhnns/agentd/internal/channel"
)

// TestReplyToAnUnfinishedRequestIsNotEvicted (review round 2): with the
// store full of unfinished requests, a reply used to be evicted the moment
// it was appended.
func TestReplyToAnUnfinishedRequestIsNotEvicted(t *testing.T) {
	st, _ := openStoreWith("", 1000, 2)
	a := New("wt", st)
	_, q1, _ := postJSON(t, a, `{"text":"one","request_id":"1"}`)
	postJSON(t, a, `{"text":"two","request_id":"2"}`)
	rc, err := a.Send(context.Background(), channel.OutboundMsg{Text: "answer one", InReplyTo: q1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get(rc.ID); !ok {
		t.Fatal("the reply was evicted while its request was unfinished")
	}
}

// TestRejectionIsWhatTheDiskSays (review round 2): a request refused for a
// full queue is stored rejected, never queued, so a restart does not run it.
func TestRejectionIsWhatTheDiskSays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, _ := OpenStore(path)
	a := New("wt", st)
	for i := 0; i < cap(a.inbound); i++ {
		postJSON(t, a, `{"text":"fill `+strconv.Itoa(i)+`"}`)
	}
	if code, _, _ := postJSON(t, a, `{"text":"late","request_id":"late"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", code)
	}
	st.Close()
	st2, _ := OpenStore(path)
	m, ok := st2.LookupRequest("late")
	if !ok || m.Status != StatusRejected {
		t.Fatalf("on disk: %+v %v", m, ok)
	}
	for _, u := range st2.Unfinished() {
		if u.RequestID == "late" {
			t.Fatal("a rejected request is queued on disk")
		}
	}
}

// TestFailedDeliveryKeepsTheCommittedAnswer (review round 2): when the
// stored answer cannot be delivered at start, the request stays unfinished
// for the next start instead of being failed with a notice.
func TestFailedDeliveryKeepsTheCommittedAnswer(t *testing.T) {
	a, st := newAdapter(t)
	_, w, _ := postJSON(t, a, `{"text":"q","request_id":"r"}`)
	drainInbound(a)
	_ = a.Ack("", w.ID, channel.ReactionWorking)
	_, del, failed := a.Reconcile(context.Background(),
		func(string, string) (CommittedTurn, bool) { return CommittedTurn{Result: "answer"}, true },
		func(context.Context, channel.InboundMsg, string, string, bool) error { return errors.New("disk full") })
	if del != 0 || failed != 0 || a.hasFailure(w.ID) {
		t.Fatalf("delivered=%d failed=%d notice=%v", del, failed, a.hasFailure(w.ID))
	}
	if m, _ := st.Get(w.ID); m.Status != StatusWorking {
		t.Fatalf("status = %s, want working (left for the next start)", m.Status)
	}
}

// TestCompactedFileKeepsTheHighWaterForAnOlderReader (review round 2): the
// high-water seq survives compaction in an ordinary record, which a reader
// that ignores the new fields (the previous binary) also counts.
func TestCompactedFileKeepsTheHighWaterForAnOlderReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, _ := openStoreWith(path, 1000, 2)
	a := New("wt", st)
	postJSON(t, a, `{"text":"a","request_id":"a"}`)
	postJSON(t, a, `{"text":"b","request_id":"b"}`)
	n := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "evicted at once"})
	st.Close()
	st2, err := openStoreWith(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
	f, _ := os.Open(path)
	defer f.Close()
	var max int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m struct {
			Seq int64  `json:"seq"`
			ID  string `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.ID != "" && m.Seq > max {
			max = m.Seq
		}
	}
	if max < n.Seq {
		t.Fatalf("an older reader sees high-water %d, want >= %d", max, n.Seq)
	}
}

// TestReconcileReportsARecordedFailure (review round 5): a committed failed
// turn whose notice was lost gets its recorded cause, not "interrupted".
func TestReconcileReportsARecordedFailure(t *testing.T) {
	a, st := newAdapter(t)
	_, w, _ := postJSON(t, a, `{"text":"q","request_id":"r"}`)
	drainInbound(a)
	_ = a.Ack("", w.ID, channel.ReactionWorking)
	a.Reconcile(context.Background(),
		func(string, string) (CommittedTurn, bool) {
			return CommittedTurn{Error: "turn timed out", Result: "two trains found"}, true
		},
		func(context.Context, channel.InboundMsg, string, string, bool) error { return nil })
	msgs, _, _ := st.Tail(10)
	found := false
	for _, m := range msgs {
		if m.Kind == KindFailure && m.ReplyTo == w.ID {
			found = true
			if m.Text == InterruptedNotice || !strings.Contains(m.Text, "turn timed out") || !strings.Contains(m.Text, "two trains found") {
				t.Fatalf("notice = %q", m.Text)
			}
		}
	}
	if m, _ := st.Get(w.ID); !found || m.Status != StatusFailed {
		t.Fatalf("found=%v status=%s", found, m.Status)
	}
}

// TestUnfinishedIsInAdmissionOrder (review round 6): a re-admitted request
// is re-enqueued after the requests admitted before its retry.
func TestUnfinishedIsInAdmissionOrder(t *testing.T) {
	a, st := newAdapter(t)
	for i := 0; i < cap(a.inbound); i++ {
		postJSON(t, a, `{"text":"fill"}`)
	}
	postJSON(t, a, `{"text":"A","request_id":"A"}`) // rejected
	drainInbound(a)
	for _, m := range st.Unfinished() {
		_ = a.Ack("", m.ID, channel.ReactionDone)
	}
	postJSON(t, a, `{"text":"B","request_id":"B"}`)
	postJSON(t, a, `{"text":"A","request_id":"A"}`) // re-admitted after B
	var order []string
	for _, m := range st.Unfinished() {
		order = append(order, m.Text)
	}
	if strings.Join(order, ",") != "B,A" {
		t.Fatalf("restart order = %v, want B,A", order)
	}
}
