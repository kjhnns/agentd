package session

// TURN RECORDS (D6).
//
// RunTurn writes ONE `turn` record per turn into the run-log while it still
// owns the session: who asked (request id, message id, chat, conversation),
// what was asked, what came back, on which process generation and in which
// conversation epoch. Inbound and outbound records are written outside the
// turn lock, so they cannot tell two conversations of one chat apart; turn
// records can. The recap reads them (both channels of a shared session),
// and the wrist's restart recovery (D5) finds a request's committed result by
// its message id.
//
// The CONVERSATION EPOCH of a chat is persisted in the run-log as
// `conversation_start` records {chat, conv, epoch}. It is a property of the
// CHAT (the session key), not of one conversation id: it advances only when
// the side session starts a fresh conversation (RunTurn, Side), and it
// survives resets, recovery and reclaim. The newest record of a chat also
// names its ACTIVE conversation. A main chat never gets one and stays in
// epoch 0. The recap reads only the current epoch's turn records; the legacy
// inbound/outbound matching applies only to epoch 0, and only to records
// written before the chat's first turn record.

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/runlog"
)

// ConvMain is the primary conversation of a chat (an absent conv).
const ConvMain = "main"

// TurnRecord is the payload of a `turn` run-log record.
type TurnRecord struct {
	TurnID            string           `json:"turn_id"`
	RequestID         string           `json:"request_id,omitempty"`
	MessageID         string           `json:"message_id,omitempty"`
	Ref               string           `json:"ref,omitempty"`
	Chat              string           `json:"chat,omitempty"`
	Conv              string           `json:"conv,omitempty"`
	ConversationEpoch uint64           `json:"conversation_epoch"`
	ProcessGeneration uint64           `json:"process_generation"`
	Session           string           `json:"session"`
	UserText          string           `json:"user_text"`
	Result            string           `json:"result"`
	Error             string           `json:"error,omitempty"`
	Attachments       []TurnAttachment `json:"attachments,omitempty"`
}

// conversationStart is the payload of a `conversation_start` record.
type conversationStart struct {
	Chat  string `json:"chat"`
	Conv  string `json:"conv"`
	Epoch uint64 `json:"epoch"`
}

// epochs is the persisted conversation epoch and active conversation per
// chat, loaded once from the run-log and updated by startConversation.
type epochs struct {
	once sync.Once
	mu   sync.Mutex
	m    map[string]conversationStart
}

// loadEpochs reads the conversation_start records once.
func (m *Manager) loadEpochs() {
	m.epochs.once.Do(func() {
		m.epochs.m = map[string]conversationStart{}
		if m.log == nil {
			return
		}
		recs, err := runlog.Replay(m.log.Path())
		if err != nil {
			log.Printf("session: conversation epochs: %v", err)
		}
		for _, r := range recs {
			if r.Type != "conversation_start" {
				continue
			}
			var cs conversationStart
			if json.Unmarshal(r.Data, &cs) == nil && cs.Chat != "" && cs.Epoch >= m.epochs.m[cs.Chat].Epoch {
				m.epochs.m[cs.Chat] = cs
			}
		}
	})
}

// conversationEpoch returns the current conversation epoch of a chat (0
// until a conversation_start advances it).
func (m *Manager) conversationEpoch(chat string) uint64 {
	if chat == "" {
		return 0
	}
	m.loadEpochs()
	m.epochs.mu.Lock()
	defer m.epochs.mu.Unlock()
	return m.epochs.m[chat].Epoch
}

// activeConversation returns the conversation the chat's session holds now
// ("" before the first conversation_start).
func (m *Manager) activeConversation(chat string) string {
	if chat == "" {
		return ""
	}
	m.loadEpochs()
	m.epochs.mu.Lock()
	defer m.epochs.mu.Unlock()
	return m.epochs.m[chat].Conv
}

// startConversation persists a conversation_start {chat, conv, epoch+1} and
// makes conv the chat's active conversation. Nothing changes when the record
// cannot be written. Caller owns the chat's session (RunTurn).
func (m *Manager) startConversation(chat, conv string) (uint64, error) {
	m.loadEpochs()
	m.epochs.mu.Lock()
	defer m.epochs.mu.Unlock()
	cs := conversationStart{Chat: chat, Conv: conv, Epoch: m.epochs.m[chat].Epoch + 1}
	if m.log != nil {
		if err := m.log.Append("conversation_start", cs); err != nil {
			return 0, err
		}
	}
	m.epochs.m[chat] = cs
	return cs.Epoch, nil
}

// writeTurnRecord appends the turn record and reports whether it is durable
// (one retry). Caller owns the session (RunTurn).
func (m *Manager) writeTurnRecord(s *Session, req TurnRequest, res TurnResult, err error) bool {
	if m.log == nil {
		return false
	}
	conv := req.Conv
	if conv == "" && req.Chat != "" {
		conv = ConvMain
	}
	user := req.UserText
	if user == "" {
		user = req.Text
	}
	rec := TurnRecord{
		TurnID:            res.TurnID,
		RequestID:         req.RequestID,
		MessageID:         req.MessageID,
		Ref:               req.Ref,
		Chat:              req.Chat,
		Conv:              conv,
		ConversationEpoch: m.conversationEpoch(req.Chat),
		ProcessGeneration: res.Generation,
		Session:           s.ID,
		UserText:          user,
		Result:            res.Text,
		Attachments:       req.Attachments,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	aerr := m.log.Append("turn", rec)
	if aerr != nil {
		aerr = m.log.Append("turn", rec)
	}
	if aerr != nil {
		log.Printf("session %s: turn record not written: %v", s.ID, aerr)
		return false
	}
	return true
}

// FindTurn returns the newest committed turn record of an inbound message
// (by message id, else by request id), for the restart recovery of the
// wrist (D5).
func (m *Manager) FindTurn(messageID, requestID string) (TurnRecord, time.Time, bool) {
	if m.log == nil || (messageID == "" && requestID == "") {
		return TurnRecord{}, time.Time{}, false
	}
	recs, err := runlog.Replay(m.log.Path())
	if err != nil {
		return TurnRecord{}, time.Time{}, false
	}
	return findTurn(recs, messageID, requestID)
}

func findTurn(recs []runlog.Record, messageID, requestID string) (TurnRecord, time.Time, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Type != "turn" {
			continue
		}
		var tr TurnRecord
		if json.Unmarshal(recs[i].Data, &tr) != nil {
			continue
		}
		if (messageID != "" && tr.MessageID == messageID) || (requestID != "" && tr.RequestID == requestID) {
			return tr, recs[i].TS, true
		}
	}
	return TurnRecord{}, time.Time{}, false
}
