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
// produced.
func (s *Store) LookupRequest(requestID string) (Message, bool) {
	if requestID == "" {
		return Message{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupLocked(requestID)
}

func (s *Store) lookupLocked(requestID string) (Message, bool) {
	for _, id := range s.order {
		if m := s.latest[id]; m.RequestID == requestID && m.Role == RoleUser {
			return m, true
		}
	}
	return Message{}, false
}

// Admit stores a new user message (or re-admits a rejected request) and
// offers its inbound payload to the queue, all under the store lock. msg must
// carry Role, Kind, Text and its RequestID ("" = no idempotency); in is the
// inbound payload (its MsgID is set here). offer must not block.
func (s *Store) Admit(msg Message, in channel.InboundMsg, offer func(channel.InboundMsg) bool) (Message, AdmitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	msg.Status = StatusQueued
	msg.Inbound = &in
	stored, err := s.appendDurable(msg, true)
	if err != nil {
		return Message{}, Rejected, errors.Join(ErrPersist, err)
	}
	if offer(in) {
		return stored, Admitted, nil
	}
	// The queue is full. A request with an id can be retried and is marked
	// rejected; one without (an old client) fails, as before.
	stored.Status = StatusFailed
	if msg.RequestID != "" {
		stored.Status = StatusRejected
	}
	stored.Inbound = nil
	return s.appendLocked(stored), Rejected, nil
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

// HasReply reports whether an agent message answering id is stored.
func (s *Store) HasReply(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, oid := range s.order {
		m := s.latest[oid]
		if m.Role == RoleAgent && m.ReplyTo == id && m.Kind != KindFailure {
			return true
		}
	}
	return false
}
