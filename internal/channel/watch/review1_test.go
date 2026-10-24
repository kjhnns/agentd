package watch

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/kjhnns/agentd/internal/channel"
)

// TestFailedSyncIsRolledBack: a record whose write succeeded but whose
// fsync failed is cut back, so a retry with the same request_id does not
// leave two queued copies on disk (both would run after a restart).
func TestFailedSyncIsRolledBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := New("wt", st)
	st.failSync = func() error { return errors.New("fsync: I/O error") }
	if code, _, _ := postJSON(t, a, `{"text":"q","request_id":"r"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", code)
	}
	st.failSync = nil
	if code, _, _ := postJSON(t, a, `{"text":"q","request_id":"r"}`); code != http.StatusAccepted {
		t.Fatalf("retry code = %d", code)
	}
	st.Close()
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(st2.Unfinished()); n != 1 {
		t.Fatalf("queued on disk after reopen = %d, want 1", n)
	}
}

// TestCompactionKeepsTheSeqHighWater: when the snapshot with the highest
// seq is one compaction drops, the reopened store must not hand out lower
// seqs (a client keeps max(cursor) and would skip new messages).
func TestCompactionKeepsTheSeqHighWater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, _ := openStoreWith(path, 1000, 2)
	a := New("wt", st)
	postJSON(t, a, `{"text":"a","request_id":"a"}`)
	postJSON(t, a, `{"text":"b","request_id":"b"}`)
	n := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "evicted at once"})
	st.Close()
	st2, err := openStoreWith(path, 1, 2) // compacts on open
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
	st3, _ := openStoreWith(path, 1, 2)
	next := st3.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "after"})
	if next.Seq <= n.Seq {
		t.Fatalf("seq went back: %d after %d", next.Seq, n.Seq)
	}
}

// TestReconcileQueuedWithCommittedTurnIsNotRerun: when the "working" write
// was lost but the turn committed, the request is answered from the turn
// record, not run again.
func TestReconcileQueuedWithCommittedTurnIsNotRerun(t *testing.T) {
	a, _ := newAdapter(t)
	_, q, _ := postJSON(t, a, `{"text":"ran already","request_id":"r"}`)
	drainInbound(a)
	_, _, failed := a.Reconcile(context.Background(),
		func(msgID, reqID string) (CommittedTurn, bool) {
			return CommittedTurn{SessionID: "s", Result: "the answer"}, msgID == q.ID
		},
		func(ctx context.Context, in channel.InboundMsg, sid, result string, finalOnly bool) error {
			_, err := a.Send(ctx, channel.OutboundMsg{Text: result, InReplyTo: in.MsgID})
			return err
		})
	if failed != 0 || len(drainInbound(a)) != 0 {
		t.Fatal("a committed request was re-enqueued or failed")
	}
	if m, _ := a.store.Get(q.ID); m.Status != StatusDone {
		t.Fatalf("status = %s", m.Status)
	}
}

// TestReconcileDeliversOnlyTheMissingSummary: a restart between the long
// half and the summary delivers the summary only.
func TestReconcileDeliversOnlyTheMissingSummary(t *testing.T) {
	a, _ := newAdapter(t)
	_, w, _ := postJSON(t, a, `{"text":"long one","request_id":"r"}`)
	drainInbound(a)
	_ = a.Ack("", w.ID, channel.ReactionWorking)
	if _, err := a.Send(context.Background(), channel.OutboundMsg{Text: "long body", Silent: true, InReplyTo: w.ID}); err != nil {
		t.Fatal(err)
	}
	var gotFinalOnly []bool
	_, del, _ := a.Reconcile(context.Background(),
		func(msgID, reqID string) (CommittedTurn, bool) { return CommittedTurn{Result: "x"}, true },
		func(ctx context.Context, in channel.InboundMsg, sid, result string, finalOnly bool) error {
			gotFinalOnly = append(gotFinalOnly, finalOnly)
			return nil
		})
	if del != 1 || len(gotFinalOnly) != 1 || !gotFinalOnly[0] {
		t.Fatalf("delivered=%d finalOnly=%v, want the final part only", del, gotFinalOnly)
	}
}

// TestDurableAckReportsAFailedWrite: "working" that cannot be persisted is
// an error the session layer acts on (the turn does not start).
func TestDurableAckReportsAFailedWrite(t *testing.T) {
	a, st := newAdapter(t)
	_, m, _ := postJSON(t, a, `{"text":"x","request_id":"r"}`)
	st.failWrite = func() error { return errors.New("disk full") }
	if err := a.AckDurable("", m.ID, channel.ReactionWorking); err == nil {
		t.Fatal("a failed write was not reported")
	}
	if got, _ := st.Get(m.ID); got.Status != StatusQueued {
		t.Fatalf("status = %s, want queued (nothing changed)", got.Status)
	}
}
