package session

// CONVERSATIONAL CONTINUITY across a cold start.
//
// A session's harness process dies far more often than the CONVERSATION does.
// From the user's side a Telegram thread is one unbroken chat; from agentd's
// side that same thread has, in practice, been carried by dozens of distinct
// harness processes (context-pressure resets, idle reclaims, daemon restarts).
// Every one of those transitions throws away the harness context window.
//
// reset.go's checkpoint-flush is the existing answer, and it is the right
// answer for DURABLE state: it makes the agent write open threads, decisions
// and blockers into context.md + memory/pages before teardown. But a flush is a
// SUMMARY, written in the agent's own words. Back-references do not survive a
// summary. "add a calendar hold entry re 2" needs the literal numbered list the
// agent emitted two minutes earlier; "like i asked you" needs the user's literal
// ask. Those are exactly the tokens a summary drops.
//
// So this file adds the other half, and only the other half:
//
//	context.md  -> summarised state, older material, agent-authored  (existing)
//	recap       -> the verbatim tail of the actual channel thread     (here)
//
// The recap is reconstructed from the run-log, which already records every
// inbound and outbound message verbatim with timestamps and message ids, is
// append-only and fsynced per record, and (crucially) is keyed by CHANNEL CHAT
// ID, not by session id. That makes it the one artifact that spans every
// cold-start path. Nothing new has to be persisted; the log was already there
// and simply had no reader on the write-back side.
//
// It is injected into the harness SYSTEM PROMPT at process start (create,
// context reset, dead-process recovery) and nowhere else. That is the cheap
// shape: no extra turn, no per-turn cost, and it rides the prompt cache. It is
// deliberately bounded so it cannot make context pressure worse than the
// problem it solves: naively replaying a whole thread into a 200k window would
// leave no room to work and would trip the 0.75 reset threshold SOONER, which
// is the opposite of continuity.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/runlog"
)

// RecapPolicy bounds the injected recap. Every bound exists because the failure
// mode of an unbounded recap is worse than no recap at all.
type RecapPolicy struct {
	// MaxMessages caps how many recent messages are replayed verbatim.
	MaxMessages int
	// MaxAge drops anything older than this. This is the STALENESS guard: a
	// recap must not resurrect a thread that was resolved days ago as though it
	// were still open. context.md is what carries genuinely long-lived state.
	MaxAge time.Duration
	// MaxBytes is the hard budget for the rendered recap. Messages are selected
	// newest-first, so when the budget binds it is the OLDEST that fall away.
	MaxBytes int
	// MaxUserChars / MaxAgentChars truncate one over-long message. The user's
	// own words get the bigger allowance: they are short and they are the thing
	// a back-reference most often points at. An agent reply is truncated from
	// the END, keeping its head, because that is where an enumerated list is.
	MaxUserChars  int
	MaxAgentChars int
}

// DefaultRecapPolicy is tuned against the live run-log: ~20 messages of a real
// Telegram thread render to roughly 3-5 KB, which is ~1% of a 200k window. That
// is affordable on every turn via the prompt cache and cannot meaningfully move
// the pressure threshold.
func DefaultRecapPolicy() RecapPolicy {
	return RecapPolicy{
		MaxMessages:   20,
		MaxAge:        24 * time.Hour,
		MaxBytes:      6000,
		MaxUserChars:  2000,
		MaxAgentChars: 1200,
	}
}

func (p RecapPolicy) withDefaults() RecapPolicy {
	d := DefaultRecapPolicy()
	if p.MaxMessages <= 0 {
		p.MaxMessages = d.MaxMessages
	}
	if p.MaxAge <= 0 {
		p.MaxAge = d.MaxAge
	}
	if p.MaxBytes <= 0 {
		p.MaxBytes = d.MaxBytes
	}
	if p.MaxUserChars <= 0 {
		p.MaxUserChars = d.MaxUserChars
	}
	if p.MaxAgentChars <= 0 {
		p.MaxAgentChars = d.MaxAgentChars
	}
	return p
}

// recapMsg is one reconstructed thread entry.
type recapMsg struct {
	TS    time.Time
	Role  string // "user" | "agent"
	Text  string
	MsgID string
}

// ThreadRecap renders the bounded verbatim recap for one channel thread, given
// a session TITLE ("telegram:7597951120", "cli:johannes", ...). The title is the
// thread anchor precisely because it is stable across every session id the
// thread has ever had. An empty string means "nothing worth injecting" and
// callers should inject nothing rather than an empty heading.
func ThreadRecap(logPath, title string, p RecapPolicy, now time.Time) (string, error) {
	msgs, err := threadMessages(logPath, title)
	if err != nil {
		return "", err
	}
	return renderRecap(msgs, p, now), nil
}

// threadMessages replays the run-log and reconstructs one thread's messages in
// chronological order.
//
// Two matching strategies, because two kinds of thread exist:
//
//   - A CHANNEL thread ("telegram:<chat>"): matched on the chat id carried by
//     the inbound/outbound records themselves. This deliberately ignores session
//     ids, which is the whole point: the thread outlives them.
//   - Anything else (CLI, web, ad-hoc titles): matched via the set of session
//     ids that were created under that title, using the input/output records.
func threadMessages(logPath, title string) ([]recapMsg, error) {
	if logPath == "" || title == "" {
		return nil, nil
	}
	recs, err := runlog.Replay(logPath)
	if err != nil {
		return nil, err
	}

	channelName, chatID, isChannel := strings.Cut(title, ":")
	if !isChannel || chatID == "" {
		channelName, chatID = "", ""
	}

	// Session ids ever created under this title. Used as the fallback anchor,
	// and as a secondary match for outbound records (which carry a session id
	// but no channel name).
	ids := map[string]bool{}
	for _, rec := range recs {
		if rec.Type != "session_create" {
			continue
		}
		var c struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if json.Unmarshal(rec.Data, &c) == nil && c.Title == title && c.ID != "" {
			ids[c.ID] = true
		}
	}

	var out []recapMsg
	for _, rec := range recs {
		switch rec.Type {
		case "inbound":
			if chatID == "" {
				continue
			}
			var in struct {
				Channel string `json:"channel"`
				UserID  string `json:"user_id"`
				Text    string `json:"text"`
				MsgID   string `json:"msg_id"`
			}
			if json.Unmarshal(rec.Data, &in) != nil {
				continue
			}
			if in.UserID != chatID || (channelName != "" && in.Channel != "" && in.Channel != channelName) {
				continue
			}
			out = append(out, recapMsg{TS: rec.TS, Role: "user", Text: in.Text, MsgID: in.MsgID})

		case "outbound":
			var ob struct {
				Chat    string `json:"chat"`
				Session string `json:"session"`
				Text    string `json:"text"`
				MsgID   string `json:"msg_id"`
			}
			if json.Unmarshal(rec.Data, &ob) != nil {
				continue
			}
			// Either anchor is sufficient: the chat id (survives session churn)
			// or membership in this title's session set (covers a record whose
			// chat id is absent).
			if !(chatID != "" && ob.Chat == chatID) && !ids[ob.Session] {
				continue
			}
			out = append(out, recapMsg{TS: rec.TS, Role: "agent", Text: ob.Text, MsgID: ob.MsgID})

		case "input":
			// CLI / non-channel threads only. For a channel thread the same
			// text is already present as an `inbound` record and replaying it
			// here would duplicate every user message.
			if chatID != "" {
				continue
			}
			var iv struct {
				Session string `json:"session"`
				Text    string `json:"text"`
			}
			if json.Unmarshal(rec.Data, &iv) != nil || !ids[iv.Session] {
				continue
			}
			out = append(out, recapMsg{TS: rec.TS, Role: "user", Text: iv.Text})
		}
	}
	return out, nil
}

// renderRecap turns reconstructed messages into the injected block, applying
// every bound in RecapPolicy. Selection runs newest-first so the budget always
// keeps the most recent exchange, which is what a back-reference points at.
func renderRecap(msgs []recapMsg, p RecapPolicy, now time.Time) string {
	p = p.withDefaults()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-p.MaxAge)

	// Age filter first, so "dropped as stale" and "dropped for budget" stay
	// distinguishable in the rendered note.
	fresh := make([]recapMsg, 0, len(msgs))
	stale := 0
	for _, m := range msgs {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		// now is an AS-OF instant, not just "the current time": a message after
		// it is not part of the conversation being reconstructed. In production
		// now is the real clock so this never fires, but it makes an as-of
		// replay of the log honest (and a clock skew harmless).
		if m.TS.After(now) {
			continue
		}
		if m.TS.Before(cutoff) {
			stale++
			continue
		}
		fresh = append(fresh, m)
	}
	if len(fresh) == 0 {
		return ""
	}

	// Walk backwards accumulating until a bound binds.
	lines := make([]string, 0, p.MaxMessages)
	used := 0
	kept := 0
	for i := len(fresh) - 1; i >= 0 && kept < p.MaxMessages; i-- {
		line := renderLine(fresh[i], p)
		if used+len(line) > p.MaxBytes && kept > 0 {
			break
		}
		lines = append(lines, line)
		used += len(line)
		kept++
	}
	// Reverse into chronological order.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}

	omitted := len(fresh) - kept + stale
	newest := fresh[len(fresh)-1].TS

	var b strings.Builder
	b.WriteString("## Recent conversation on this thread (verbatim)\n\n")
	b.WriteString("Your process was just started or reset, so your context window does NOT contain " +
		"what was already said on this thread. The exchange below is the literal transcript, " +
		"replayed from the durable run-log. Treat it as ground truth for back-references " +
		"(\"re 2\", \"like I asked\", \"the one you mentioned\", \"that\"): resolve them against " +
		"these exact words rather than reconstructing what you think you must have said. " +
		"The handoff in context.md summarises OLDER and longer-lived state; where the two " +
		"disagree about what was literally said, this transcript wins.\n\n")
	if omitted > 0 {
		b.WriteString(fmt.Sprintf("(%d earlier message(s) omitted: see context.md for the summarised state.)\n\n", omitted))
	}
	for _, l := range lines {
		b.WriteString(l)
	}
	if gap := now.Sub(newest); gap > time.Hour {
		b.WriteString(fmt.Sprintf("\nNote: the last message above is %s old, so this thread may already be "+
			"resolved. Do not re-open it or re-send anything on its own; wait for the user's current message.\n",
			roundGap(gap)))
	}
	return b.String()
}

func renderLine(m recapMsg, p RecapPolicy) string {
	who := "User"
	limit := p.MaxUserChars
	if m.Role == "agent" {
		who = "You"
		limit = p.MaxAgentChars
	}
	text := strings.TrimSpace(m.Text)
	if len(text) > limit {
		text = text[:limit] + " [...truncated]"
	}
	// Indent continuation lines so a multi-line reply cannot be mistaken for a
	// new speaker turn.
	text = strings.ReplaceAll(text, "\n", "\n  ")
	id := ""
	if m.MsgID != "" {
		id = " #" + m.MsgID
	}
	return fmt.Sprintf("[%s%s] %s: %s\n", m.TS.UTC().Format("2006-01-02 15:04Z"), id, who, text)
}

func roundGap(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// composeInjection builds the full system-prompt injection for a session:
// the workspace composition (instructions + memory index + context.md handoff)
// followed by the verbatim thread recap.
//
// Order matters. The recap goes LAST so it is the most recent thing in the
// injection, and because it must be read as a correction to the handoff above
// it, not the other way round.
//
// A recap failure is never fatal: continuity is an improvement over the old
// behaviour, so a missing or unreadable run-log degrades to exactly the old
// behaviour rather than refusing to start a session.
func (m *Manager) composeInjection(base, title string) string {
	if m.log == nil || title == "" || m.RecapDisabled {
		return base
	}
	recap, err := ThreadRecap(m.log.Path(), title, m.Recap, time.Now().UTC())
	if err != nil || recap == "" {
		return base
	}
	if base == "" {
		return recap
	}
	return base + "\n\n" + recap
}
