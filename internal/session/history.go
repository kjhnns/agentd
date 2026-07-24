package session

// History replay: the web UI (and any other client) can rebuild a session's
// prior conversation after a page reload from the durable run-log, instead of
// only seeing events that arrive live over the WS after load. Served as
// GET /sessions/:id/history by the api package.

import (
	"encoding/json"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/runlog"
)

// HistoryEntry is one replayed conversation item, assembled from the run-log.
type HistoryEntry struct {
	TS   time.Time `json:"ts"`
	Role string    `json:"role"` // "user" | "agent"
	Kind string    `json:"kind"` // input | output | tool_call | result | error
	Text string    `json:"text,omitempty"`
	Tool string    `json:"tool,omitempty"`
}

// HistoryMaxTurns caps replay at the last N turns (a turn starts at a user
// input record). Older records are dropped from the RESPONSE only; the run-log
// itself keeps full fidelity. No pagination yet: the cap is the whole contract.
const HistoryMaxTurns = 50

// History replays the session's conversation from the run-log: user input
// records plus the harness events of each turn, in log order, capped to the
// last maxTurns turns (<=0 means HistoryMaxTurns).
//
// The single-visible-reply rule applies to replay exactly as it does to the
// live stream (see claudecode's resultDeduper): a success result whose text
// merely duplicates the turn's final output is blanked, so records written
// BEFORE the dedupe fix landed do not double-render either. Result events are
// kept as text-less turn markers, matching what the live path now emits.
//
// An id with no recorded turns yields an empty history, not an error: history
// may be requested for a session that was just created (nothing logged yet).
func (m *Manager) History(id string, maxTurns int) ([]HistoryEntry, error) {
	if m.log == nil || id == "" {
		return []HistoryEntry{}, nil
	}
	recs, err := runlog.Replay(m.log.Path())
	if err != nil {
		return nil, err
	}
	if maxTurns <= 0 {
		maxTurns = HistoryMaxTurns
	}

	out := []HistoryEntry{}
	lastOutput := ""
	for _, rec := range recs {
		switch rec.Type {
		case "input":
			var in struct {
				Session string `json:"session"`
				Text    string `json:"text"`
			}
			if json.Unmarshal(rec.Data, &in) != nil || in.Session != id {
				continue
			}
			out = append(out, HistoryEntry{TS: rec.TS, Role: "user", Kind: "input", Text: in.Text})
			lastOutput = ""
		case "event":
			var e eventbus.Event
			if json.Unmarshal(rec.Data, &e) != nil || e.SessionID != id {
				continue
			}
			switch e.Kind {
			case eventbus.KindOutput:
				lastOutput = e.Text
			case eventbus.KindToolCall:
				// kept as-is
			case eventbus.KindResult:
				if e.Text != "" && e.Text == lastOutput {
					e.Text = "" // pre-fix record: drop the duplicated reply text
				}
				lastOutput = ""
			case eventbus.KindError:
				lastOutput = ""
			default:
				continue // status/needs_input are live-only signals
			}
			ts := e.TS
			if ts.IsZero() {
				ts = rec.TS
			}
			out = append(out, HistoryEntry{TS: ts, Role: "agent", Kind: string(e.Kind), Text: e.Text, Tool: e.Tool})
		}
	}

	// Cap to the last maxTurns turns (turn boundary = a user input entry).
	var starts []int
	for i, en := range out {
		if en.Role == "user" {
			starts = append(starts, i)
		}
	}
	if len(starts) > maxTurns {
		out = out[starts[len(starts)-maxTurns]:]
	}
	return out, nil
}
