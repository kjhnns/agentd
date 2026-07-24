package session

// Past-sessions browser: sessions that left the live list (idle GC reclaim or
// a daemon restart) stay discoverable from the durable run-log. PastSessions
// enumerates them for GET /sessions/past; their conversations are readable via
// the existing History replay; Continue starts a NEW live session seeded with
// a compact rendering of the past thread (continue-as-new-session; same-id
// resurrection is deliberately NOT attempted, attach-survives-restart stays a
// reserved future design).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/runlog"
)

// PastSession is one run-log-derived listing entry for a no-longer-live session.
type PastSession struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	Chat      string    `json:"chat,omitempty"` // channel chat label from outbound records
	FirstTS   time.Time `json:"first_ts"`
	LastTS    time.Time `json:"last_ts"`
	Turns     int       `json:"turns"`
	Preview   string    `json:"preview,omitempty"` // first user input, else last output snippet
}

// PastSessionsMaxList caps the listing (most recent first). The scan is a
// single pass over the append-only run-log; no index is built on purpose.
const PastSessionsMaxList = 100

// previewRunes bounds the preview snippet length.
const previewRunes = 140

// PastSessions enumerates sessions seen in the run-log that are NOT currently
// live (live ones are served by GET /sessions), most recent activity first,
// capped at limit (<=0 or too large means PastSessionsMaxList).
func (m *Manager) PastSessions(limit int) ([]PastSession, error) {
	if m.log == nil {
		return []PastSession{}, nil
	}
	recs, err := runlog.Replay(m.log.Path())
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > PastSessionsMaxList {
		limit = PastSessionsMaxList
	}

	type agg struct {
		PastSession
		lastOutput string
	}
	byID := map[string]*agg{}
	touch := func(id string, ts time.Time) *agg {
		if id == "" {
			return nil
		}
		a, ok := byID[id]
		if !ok {
			a = &agg{PastSession: PastSession{ID: id, FirstTS: ts}}
			byID[id] = a
		}
		if a.FirstTS.IsZero() || ts.Before(a.FirstTS) {
			a.FirstTS = ts
		}
		if ts.After(a.LastTS) {
			a.LastTS = ts
		}
		return a
	}

	for _, rec := range recs {
		switch rec.Type {
		case "session_create":
			var d struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Workspace string `json:"workspace"`
			}
			if unmarshalData(rec, &d) != nil {
				continue
			}
			if a := touch(d.ID, rec.TS); a != nil {
				a.Title = d.Title
				a.Workspace = d.Workspace
			}
		case "input":
			var d struct {
				Session string `json:"session"`
				Text    string `json:"text"`
			}
			if unmarshalData(rec, &d) != nil {
				continue
			}
			if a := touch(d.Session, rec.TS); a != nil {
				a.Turns++
				if a.Preview == "" {
					a.Preview = truncateRunes(strings.TrimSpace(d.Text), previewRunes)
				}
			}
		case "outbound":
			var d struct {
				Session string `json:"session"`
				Chat    string `json:"chat"`
			}
			if unmarshalData(rec, &d) != nil {
				continue
			}
			if a := touch(d.Session, rec.TS); a != nil && d.Chat != "" {
				a.Chat = d.Chat
			}
		case "event":
			var e eventbus.Event
			if unmarshalData(rec, &e) != nil {
				continue
			}
			if a := touch(e.SessionID, rec.TS); a != nil {
				if e.Kind == eventbus.KindOutput && strings.TrimSpace(e.Text) != "" {
					a.lastOutput = e.Text
				}
			}
		}
	}

	out := make([]PastSession, 0, len(byID))
	for id, a := range byID {
		if _, live := m.Get(id); live {
			continue // live sessions are in GET /sessions
		}
		if a.Preview == "" {
			a.Preview = truncateRunes(strings.TrimSpace(a.lastOutput), previewRunes)
		}
		out = append(out, a.PastSession)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS.After(out[j].LastTS) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func unmarshalData(rec runlog.Record, v any) error {
	return json.Unmarshal(rec.Data, v)
}

// truncateRunes caps s at n runes (rune-safe, marks elision).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " [...]"
}

// ---- continue-as-new-session ----

// ContinueSeedTurns caps how many past turns are rendered into the seed.
const ContinueSeedTurns = 20

// continueSeedMaxRunes bounds the whole transcript block in the seed turn
// (oldest lines are dropped first; the recent turns matter most).
const continueSeedMaxRunes = 12000

// Continue starts a NEW live session seeded with a compact rendering of the
// past session's conversation (from the run-log). The seed is delivered as the
// new session's first turn in the background (a real harness turn, so the
// model actually reads it); the returned session is immediately visible in the
// live list, titled "continued:<source id>" so the UI can link it back.
func (m *Manager) Continue(ctx context.Context, sourceID string) (*Session, error) {
	if _, live := m.Get(sourceID); live {
		return nil, fmt.Errorf("session %s is still live; continue is for past sessions", sourceID)
	}
	hist, err := m.History(sourceID, ContinueSeedTurns)
	if err != nil {
		return nil, err
	}
	if len(hist) == 0 {
		return nil, fmt.Errorf("no recorded conversation for session %s", sourceID)
	}
	seed := continueSeed(sourceID, hist)

	s, err := m.Create(ctx, "", m.DefaultCwd, m.DefaultModel, "continued:"+sourceID)
	if err != nil {
		return nil, err
	}
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		if _, err := m.Send(sctx, s.ID, seed); err != nil {
			log.Printf("session: continuation %s (from %s) seed turn failed: %v", s.ID, sourceID, err)
		}
	}()
	return s, nil
}

// continueSeed renders the past conversation into one seed turn: a preamble
// plus a "User:/Agent:" transcript of the replayed turns, budget-capped with
// oldest lines dropped first.
func continueSeed(sourceID string, hist []HistoryEntry) string {
	var lines []string
	for _, en := range hist {
		text := strings.TrimSpace(en.Text)
		if text == "" {
			continue
		}
		text = truncateRunes(text, 1000)
		switch {
		case en.Role == "user":
			lines = append(lines, "User: "+text)
		case en.Kind == "output":
			lines = append(lines, "Agent: "+text)
		}
	}
	// Keep the most recent lines within budget.
	total, start := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		total += len([]rune(lines[i])) + 1
		if total > continueSeedMaxRunes {
			break
		}
		start = i
	}
	lines = lines[start:]

	return "SYSTEM NOTE: This session continues a previous conversation (session " + sourceID +
		") whose live process ended. A compact transcript of its most recent turns follows so you have the context. " +
		"Read it, then reply with ONE short sentence acknowledging you are caught up.\n\n" +
		"--- previous conversation ---\n" +
		strings.Join(lines, "\n") +
		"\n--- end of previous conversation ---"
}
