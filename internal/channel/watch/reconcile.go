package watch

import (
	"context"
	"log"

	"github.com/kjhnns/agentd/internal/channel"
)

// InterruptedNotice is the failure text of a request whose turn was cut by a
// restart without a committed result. The app offers Retry, which sends it
// again as a NEW request.
const InterruptedNotice = "Interrupted by a restart; it may have partly run."

// CommittedTurn is what the run-log holds about a request's turn.
type CommittedTurn struct {
	SessionID string
	Result    string
	Error     string
}

// Reconcile settles every unfinished request after a daemon start (D5). It
// runs before the HTTP surface serves and after the inbound consumer runs:
//
//   - queued with its stored payload: re-enqueued, in conversation order;
//   - working or needs_input with a committed turn: its stored result is
//     delivered (unless a reply is already stored), or a failed turn is
//     marked failed;
//   - working or needs_input without one, or queued without a payload (a
//     row from before the payload was stored): failed with InterruptedNotice.
//
// lookup finds a committed turn by message id or request id; deliver sends
// a stored result through the session's reply path.
func (a *Adapter) Reconcile(ctx context.Context,
	lookup func(messageID, requestID string) (CommittedTurn, bool),
	deliver func(ctx context.Context, in channel.InboundMsg, sessionID, result string) error,
) (requeued, delivered, failed int) {
	for _, m := range a.store.Unfinished() {
		in := channel.InboundMsg{Channel: "watch", UserID: a.userID, MsgID: m.ID, Sender: "watch", Text: m.Text, RequestID: m.RequestID}
		if m.Inbound != nil {
			in = *m.Inbound
			in.MsgID = m.ID
			in.UserID = a.userID
		}
		if m.Status == StatusQueued && m.Inbound != nil {
			select {
			case a.inbound <- in:
				requeued++
				continue
			case <-ctx.Done():
				return
			}
		}
		if m.Status != StatusQueued {
			if t, ok := lookup(m.ID, m.RequestID); ok {
				if t.Error == "" {
					if a.store.HasReply(m.ID) {
						a.setStatus(m.ID, StatusDone)
						delivered++
						continue
					}
					if err := deliver(ctx, in, t.SessionID, t.Result); err == nil {
						a.setStatus(m.ID, StatusDone)
						delivered++
						continue
					} else {
						log.Printf("watch: reconcile %s: delivering the stored result failed: %v", m.ID, err)
					}
				}
			}
		}
		a.setStatus(m.ID, StatusFailed)
		if !a.hasFailure(m.ID) {
			a.store.Append(Message{Role: RoleAgent, Kind: KindFailure, Text: InterruptedNotice, ReplyTo: m.ID})
		}
		failed++
	}
	if requeued+delivered+failed > 0 {
		log.Printf("watch: restart reconciliation: %d re-enqueued, %d delivered, %d failed", requeued, delivered, failed)
	}
	return
}

func (a *Adapter) setStatus(id, status string) {
	a.store.Update(id, func(m *Message) bool {
		if m.Status == status {
			return false
		}
		m.Status = status
		if Terminal(status) {
			m.Inbound = nil
		}
		return true
	})
}

func (a *Adapter) hasFailure(id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, oid := range a.store.order {
		if m := a.store.latest[oid]; m.Role == RoleAgent && m.ReplyTo == id && m.Kind == KindFailure {
			return true
		}
	}
	return false
}
