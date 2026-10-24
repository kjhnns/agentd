package watch

// ADMISSION (D5).
//
// A delegation is admitted in ONE step under the store lock: look up its
// request_id; if it is new, persist the user message in state queued with
// the full inbound payload (fsync; a write error stores nothing and the
// client gets 503), then offer it to the inbound queue. A full queue marks
// it rejected (retryable) and answers 503. A repeat of a queued, working,
// needs_input or finished request answers the original receipt; a repeat of
// a rejected one re-admits it. A message without a request_id (an old
// client) is admitted exactly as before: persisted, offered, and marked
// failed when the queue is full.

import (
	"errors"
	"log"

	"github.com/kjhnns/agentd/internal/channel"
)

// AdmitOutcome says what Admit did.
type AdmitOutcome int

const (
	// Admitted: a new message was stored and queued (or a rejected request
	// was re-admitted).
	Admitted AdmitOutcome = iota
	// Duplicate: the request_id is known and not rejected; the returned
	// message is the original receipt. Nothing was stored or queued.
	Duplicate
	// Rejected: stored, but the inbound queue was full.
	Rejected
)

// ErrPersist wraps a write or fsync error at admission: nothing was stored.
var ErrPersist = errors.New("watch store: could not persist the message")

// LookupRequest returns the newest snapshot of the message a request_id
// produced, also when the message itself was evicted.
func (s *Store) LookupRequest(requestID string) (Message, bool) {
	if requestID == "" {
		return Message{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(requestID)
}

func (s *Store) lookupLocked(requestID string) (Message, bool) {
	m, ok := s.reqs[requestID]
	if !ok {
		return Message{}, false
	}
	if cur, live := s.latest[m.ID]; live && cur.Seq >= m.Seq {
		m = cur
	}
	m.Inbound = nil
	return m, true
}

// Admit stores a new user message (or re-admits a rejected request) and
// offers its inbound payload to the queue, all under the store lock. msg must
// carry Role, Kind, Text and its RequestID ("" = no idempotency); in is the
// inbound payload (its MsgID is set here). room reports whether the queue
// has space; offer must not block. Admit is the only sender to the queue
// while the HTTP surface serves, so room() under the store lock guarantees
// the offer: a full queue is decided BEFORE anything is written, and a
// request is stored either queued (and offered) or rejected, never queued on
// disk but rejected in the answer.
func (s *Store) Admit(msg Message, in channel.InboundMsg, room func() bool, offer func(channel.InboundMsg) bool) (Message, AdmitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return Message{}, Rejected, errors.Join(ErrPersist, s.broken)
	}
	if msg.RequestID != "" {
		if prev, ok := s.lookupLocked(msg.RequestID); ok {
			if prev.Status != StatusRejected {
				return prev, Duplicate, nil
			}
			// A retry of a rejected request: re-admit it with the new payload.
			msg.ID = prev.ID
			msg.TS = prev.TS
		}
	}
	if msg.ID == "" {
		msg.ID = newID()
	}
	in.MsgID = msg.ID
	msg.Role = RoleUser
	if !room() {
		// The queue is full. A request with an id can be retried and is
		// stored rejected; one without (an old client) is stored failed, as
		// before. Nothing is queued, on disk or in memory.
		msg.Status = StatusFailed
		if msg.RequestID != "" {
			msg.Status = StatusRejected
		}
		msg.Inbound = nil
		stored, err := s.appendDurable(msg, true)
		if err != nil {
			return Message{}, Rejected, errors.Join(ErrPersist, err)
		}
		return stored, Rejected, nil
	}
	msg.Status = StatusQueued
	msg.Inbound = &in
	stored, err := s.appendDurable(msg, true)
	if err != nil {
		return Message{}, Rejected, errors.Join(ErrPersist, err)
	}
	if !offer(in) {
		// Cannot happen while Admit is the only sender (room was checked
		// under the lock). The request stays queued on disk and in memory, so
		// the restart reconciliation runs it; the client sees it accepted.
		log.Printf("watch: queue full after the room check; %s stays queued until the next start", stored.ID)
	}
	return stored, Admitted, nil
}

// Unfinished returns the user messages whose turn is not finished (queued,
// working, needs_input), in conversation order. Used by the restart
// reconciliation.
func (s *Store) Unfinished() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, id := range s.order {
		if m := s.latest[id]; inFlight(m) {
			out = append(out, m)
		}
	}
	return out
}

// ReplyState reports what of an answer to id is stored: any part, and the
// final part (an unsplit reply or the summary; not the long half alone).
func (s *Store) ReplyState(id string) (anyPart, final bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, oid := range s.order {
		m := s.latest[oid]
		if m.Role != RoleAgent || m.ReplyTo != id || m.Kind == KindFailure {
			continue
		}
		anyPart = true
		if m.Part != PartBody {
			final = true
		}
	}
	return anyPart, final
}
