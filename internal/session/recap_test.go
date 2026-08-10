package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/runlog"
)

// writeThread builds a run-log fixture shaped exactly like the live one.
func writeThread(t *testing.T, records []func(*runlog.Log)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runlog.jsonl")
	l, err := runlog.Open(path)
	if err != nil {
		t.Fatalf("open runlog: %v", err)
	}
	for _, w := range records {
		w(l)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close runlog: %v", err)
	}
	return path
}

func create(id, title string) func(*runlog.Log) {
	return func(l *runlog.Log) {
		_ = l.Append("session_create", map[string]string{
			"id": id, "harness": "claude-code", "title": title, "workspace": "main"})
	}
}

func inbound(chat, msgID, text string) func(*runlog.Log) {
	return func(l *runlog.Log) {
		_ = l.Append("inbound", map[string]any{
			"channel": "telegram", "user_id": chat, "sender": chat,
			"msg_id": msgID, "reply_to": "", "media": nil, "text": text})
	}
}

func outbound(chat, sess, msgID, text string) func(*runlog.Log) {
	return func(l *runlog.Log) {
		_ = l.Append("outbound", map[string]string{
			"chat": chat, "session": sess, "msg_id": msgID, "text": text})
	}
}

// TestReTwoResolvesAcrossAContextReset is the regression test for the concrete
// failure of 2026-08-09 21:00Z. The sequence is the real one, compressed:
//
//	20:58:44Z  agentd emits a NUMBERED list  (session eb52002b, msg 100)
//	20:58:55Z  context reset fires at pressure 1.00 >= 0.75 -> that list is gone
//	20:59:27Z  Joe writes "add a calendar hold entry re 2 ..."
//
// The fresh process must be able to say what "2" is. Before this change its
// only inheritance was context.md, an agent-authored summary that does not
// contain the list, and it answered with "I could not find the exact wording of
// your earlier ask". After it, item 2's literal text is in the injection.
func TestReTwoResolvesAcrossAContextReset(t *testing.T) {
	const chat = "7597951120"
	const title = "telegram:" + chat
	numbered := "Quick recap of what I did:\n" +
		"1. Deleted the all-day \"Werlte: visit parents\" block Sep 18-20.\n" +
		"2. Painter appointment Fri 21.08, 07:00-07:15 arrival, Klosbachstrasse.\n" +
		"3. Nothing sent to Cami yet."

	path := writeThread(t, []func(*runlog.Log){
		create("eb52002b9654110d", title),
		inbound(chat, "98", "sounds good, please add a calendar block"),
		outbound(chat, "eb52002b9654110d", "99", "Done. Added an all-day block."),
		inbound(chat, "100", "no the calendar block was meant for the email regarding the paint job"),
		outbound(chat, "eb52002b9654110d", "101", numbered),
		// ---- context reset here: a NEW session id, empty context window ----
		create("bc6848d6bd331aaf", title),
	})

	got, err := ThreadRecap(path, title, RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if got == "" {
		t.Fatal("recap is empty: the fresh session would cold-start")
	}
	// The load-bearing assertion: item 2 survives VERBATIM.
	if !strings.Contains(got, "2. Painter appointment Fri 21.08, 07:00-07:15 arrival") {
		t.Fatalf("item 2 did not survive the reset; recap was:\n%s", got)
	}
	// And Joe's own words, which "like i asked you" points at.
	if !strings.Contains(got, "the calendar block was meant for the email regarding the paint job") {
		t.Fatalf("the user's literal ask did not survive; recap was:\n%s", got)
	}
	if !strings.Contains(got, "User:") || !strings.Contains(got, "You:") {
		t.Fatalf("recap does not distinguish speakers:\n%s", got)
	}
}

// TestRecapSpansEverySessionIdOfTheThread: the thread anchor is the CHAT, not
// the session id. This is what makes one recap cover a pressure reset, an idle
// reclaim and a daemon restart with no special case for any of them.
func TestRecapSpansEverySessionIdOfTheThread(t *testing.T) {
	const chat = "42"
	const title = "telegram:" + chat
	path := writeThread(t, []func(*runlog.Log){
		create("aaa", title),
		inbound(chat, "1", "first ask"),
		outbound(chat, "aaa", "2", "first answer"),
		create("bbb", title), // idle reclaim -> new id
		inbound(chat, "3", "second ask"),
		outbound(chat, "bbb", "4", "second answer"),
		create("ccc", title), // daemon restart -> new id again
	})
	got, err := ThreadRecap(path, title, RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	for _, want := range []string{"first ask", "first answer", "second ask", "second answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("recap dropped %q across session ids:\n%s", want, got)
		}
	}
	// Chronological order, not log-grouped-by-session order.
	if strings.Index(got, "first ask") > strings.Index(got, "second ask") {
		t.Fatalf("recap is out of order:\n%s", got)
	}
}

// TestRecapIgnoresOtherThreads: chat A must never see chat B.
func TestRecapIgnoresOtherThreads(t *testing.T) {
	path := writeThread(t, []func(*runlog.Log){
		create("aaa", "telegram:111"),
		create("bbb", "telegram:222"),
		inbound("111", "1", "alice secret"),
		outbound("111", "aaa", "2", "alice answer"),
		inbound("222", "3", "bob secret"),
		outbound("222", "bbb", "4", "bob answer"),
	})
	got, err := ThreadRecap(path, "telegram:111", RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if !strings.Contains(got, "alice secret") {
		t.Fatalf("own thread missing:\n%s", got)
	}
	if strings.Contains(got, "bob secret") || strings.Contains(got, "bob answer") {
		t.Fatalf("cross-thread leak:\n%s", got)
	}
}

// TestRecapIsBounded: the whole objection to "just ingest the log" is that it
// makes pressure WORSE. The recap must stay inside its byte budget and message
// cap and must keep the NEWEST messages when the budget binds.
func TestRecapIsBounded(t *testing.T) {
	const chat = "9"
	const title = "telegram:" + chat
	var recs []func(*runlog.Log)
	recs = append(recs, create("s1", title))
	for i := 0; i < 200; i++ {
		recs = append(recs, inbound(chat, fmt.Sprint(i), fmt.Sprintf("message number %d %s", i, strings.Repeat("x", 300))))
	}
	recs = append(recs, inbound(chat, "last", "THE NEWEST THING"))
	path := writeThread(t, recs)

	p := RecapPolicy{MaxMessages: 20, MaxBytes: 6000, MaxAge: time.Hour}
	got, err := ThreadRecap(path, title, p, time.Now().UTC())
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if len(got) > p.MaxBytes+1500 { // +header
		t.Fatalf("recap of %d bytes blew the %d budget", len(got), p.MaxBytes)
	}
	if !strings.Contains(got, "THE NEWEST THING") {
		t.Fatalf("budget dropped the NEWEST message, which is the one that matters:\n%s", got[:400])
	}
	if strings.Contains(got, "message number 0 ") {
		t.Fatalf("budget kept the oldest message; selection is backwards")
	}
	if !strings.Contains(got, "omitted") {
		t.Fatalf("recap must say material was omitted so the agent knows to read context.md")
	}
}

// TestRecapDropsStaleThreads: a recap must not resurrect a resolved thread.
func TestRecapDropsStaleThreads(t *testing.T) {
	const chat = "7"
	const title = "telegram:" + chat
	path := writeThread(t, []func(*runlog.Log){
		create("s1", title),
		inbound(chat, "1", "book the flight"),
	})
	// Look at it a week later.
	got, err := ThreadRecap(path, title, RecapPolicy{MaxAge: 24 * time.Hour}, time.Now().UTC().Add(7*24*time.Hour))
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if got != "" {
		t.Fatalf("week-old thread should not be injected at all, got:\n%s", got)
	}
}

// TestRecapIsAsOf: `now` bounds BOTH ends. Reconstructing the thread as of an
// instant must not include what was said after it, which is what makes an
// as-of replay of the real run-log a truthful reproduction rather than a
// coincidence.
func TestRecapIsAsOf(t *testing.T) {
	const chat = "5"
	const title = "telegram:" + chat
	path := writeThread(t, []func(*runlog.Log){
		create("s1", title),
		inbound(chat, "1", "said before the cut"),
	})
	then := time.Now().UTC()
	l, err := runlog.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	inbound(chat, "2", "said after the cut")(l)
	l.Close()

	got, err := ThreadRecap(path, title, RecapPolicy{}, then)
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if !strings.Contains(got, "said before the cut") {
		t.Fatalf("as-of recap lost the past:\n%s", got)
	}
	if strings.Contains(got, "said after the cut") {
		t.Fatalf("as-of recap leaked the future:\n%s", got)
	}
}

// TestRecapWarnsOnAGap: inside the age window but hours old, the agent is told
// so it does not treat a dormant thread as live and act on it unprompted.
func TestRecapWarnsOnAGap(t *testing.T) {
	const chat = "8"
	const title = "telegram:" + chat
	path := writeThread(t, []func(*runlog.Log){
		create("s1", title),
		inbound(chat, "1", "draft that email"),
	})
	got, err := ThreadRecap(path, title, RecapPolicy{}, time.Now().UTC().Add(5*time.Hour))
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if !strings.Contains(got, "5h old") {
		t.Fatalf("expected a staleness note, got:\n%s", got)
	}
	if !strings.Contains(got, "Do not re-open it") {
		t.Fatalf("staleness note must tell the agent not to act:\n%s", got)
	}
}

// TestRecapForNonChannelTitles: a CLI thread logs `input`, not `inbound`, and
// has no chat id, so it is anchored by the title's session-id set instead.
func TestRecapForNonChannelTitles(t *testing.T) {
	path := writeThread(t, []func(*runlog.Log){
		create("c1", "cli-johannes"),
		func(l *runlog.Log) { _ = l.Append("input", map[string]string{"session": "c1", "text": "cli question"}) },
		func(l *runlog.Log) {
			_ = l.Append("outbound", map[string]string{"session": "c1", "chat": "", "text": "cli answer"})
		},
		create("x1", "cli-someone-else"),
		func(l *runlog.Log) { _ = l.Append("input", map[string]string{"session": "x1", "text": "other question"}) },
	})
	got, err := ThreadRecap(path, "cli-johannes", RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if !strings.Contains(got, "cli question") || !strings.Contains(got, "cli answer") {
		t.Fatalf("cli thread not reconstructed:\n%s", got)
	}
	if strings.Contains(got, "other question") {
		t.Fatalf("cross-thread leak on the id-anchored path:\n%s", got)
	}
}

// TestRecapNeverDuplicatesChannelInput: a channel message is logged twice, once
// as `inbound` and once as `input`. Only one may reach the recap.
func TestRecapNeverDuplicatesChannelInput(t *testing.T) {
	const chat = "55"
	const title = "telegram:" + chat
	path := writeThread(t, []func(*runlog.Log){
		create("s1", title),
		inbound(chat, "1", "unique phrase here"),
		func(l *runlog.Log) {
			_ = l.Append("input", map[string]string{"session": "s1", "text": "unique phrase here"})
		},
	})
	got, err := ThreadRecap(path, title, RecapPolicy{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ThreadRecap: %v", err)
	}
	if n := strings.Count(got, "unique phrase here"); n != 1 {
		t.Fatalf("message appears %d times, want 1:\n%s", n, got)
	}
}

// TestRecapDegradesToNothing: a missing or unusable log must never break
// session start. Continuity is a bonus; losing it is not a fatal error.
func TestRecapDegradesToNothing(t *testing.T) {
	for _, tc := range []struct{ name, path, title string }{
		{"missing file", filepath.Join(t.TempDir(), "nope.jsonl"), "telegram:1"},
		{"empty path", "", "telegram:1"},
		{"empty title", filepath.Join(t.TempDir(), "nope.jsonl"), ""},
	} {
		got, err := ThreadRecap(tc.path, tc.title, RecapPolicy{}, time.Now().UTC())
		if err != nil || got != "" {
			t.Fatalf("%s: got (%q, %v), want empty and no error", tc.name, got, err)
		}
	}
}

// TestComposeInjectionOrderAndOptOut pins the contract the harness sees: the
// recap is appended AFTER the workspace injection (so it reads as a correction
// to context.md), and RecapDisabled restores the old behaviour exactly.
func TestComposeInjectionOrderAndOptOut(t *testing.T) {
	const chat = "77"
	const title = "telegram:" + chat
	path := writeThread(t, []func(*runlog.Log){
		create("s1", title),
		inbound(chat, "1", "the literal ask"),
	})
	l, err := runlog.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l.Close()

	m := &Manager{log: l}
	base := "## Current handoff (context.md)\n\nsummarised state"
	got := m.composeInjection(base, title)
	if !strings.HasPrefix(got, base) {
		t.Fatalf("workspace injection must come first:\n%s", got)
	}
	if !strings.Contains(got, "the literal ask") {
		t.Fatalf("recap not appended:\n%s", got)
	}
	if strings.Index(got, "context.md") > strings.Index(got, "the literal ask") {
		t.Fatal("recap must come after the handoff, not before it")
	}

	m.RecapDisabled = true
	if off := m.composeInjection(base, title); off != base {
		t.Fatalf("RecapDisabled did not restore the old injection: %q", off)
	}

	// No log configured at all: unchanged base, no panic.
	if got := (&Manager{}).composeInjection(base, title); got != base {
		t.Fatalf("nil log must be a no-op, got %q", got)
	}
}
