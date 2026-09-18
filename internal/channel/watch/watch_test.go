package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/notify"
)

type stubTranscriber struct{ text string }

func (s stubTranscriber) Transcribe(ctx context.Context, path string) (media.Transcript, error) {
	return media.Transcript{Text: s.text, Language: "en", DurationS: 7}, nil
}

func newAdapter(t *testing.T) (*Adapter, *Store) {
	t.Helper()
	st, err := OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	a := New("wt", st).WithMedia(&media.Service{Dir: t.TempDir(), Transcriber: stubTranscriber{"book the train to bern"}})
	return a, st
}

func do(t *testing.T, h http.Handler, method, path, token string, body *bytes.Buffer, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type fetchResp struct {
	Messages  []Message `json:"messages"`
	LatestSeq int64     `json:"latest_seq"`
	Reset     bool      `json:"reset"`
	HasMore   bool      `json:"has_more"`
	Pending   int       `json:"pending"`
}

func fetch(t *testing.T, h http.Handler, q string) fetchResp {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/watch/messages"+q, "wt", nil, "")
	if rec.Code != 200 {
		t.Fatalf("fetch %s: %d %s", q, rec.Code, rec.Body.String())
	}
	var fr fetchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &fr); err != nil {
		t.Fatal(err)
	}
	return fr
}

func TestAuthIsOwnTokenAndClosedWhenEmpty(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	if rec := do(t, h, http.MethodGet, "/watch/ping", "", nil, ""); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/watch/ping", "wrong", nil, ""); rec.Code != 401 {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/watch/ping", "wt", nil, ""); rec.Code != 200 {
		t.Fatalf("right token: %d", rec.Code)
	}
	st, _ := OpenStore("")
	closed := New("", st).Handler()
	if rec := do(t, closed, http.MethodGet, "/watch/ping", "", nil, ""); rec.Code != 401 {
		t.Fatalf("empty token must be closed, got %d", rec.Code)
	}
}

func TestTextDelegationLifecycle(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()

	body := bytes.NewBufferString(`{"text":"  what is on my calendar  "}`)
	rec := do(t, h, http.MethodPost, "/watch/messages", "wt", body, "application/json")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	var posted struct {
		Message Message `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &posted)
	if posted.Message.Role != RoleUser || posted.Message.Kind != KindText || posted.Message.Status != StatusQueued || posted.Message.Text != "what is on my calendar" {
		t.Fatalf("receipt = %+v", posted.Message)
	}

	// The inbound message is emitted with the receipt id as MsgID (the Ack target).
	var in channel.InboundMsg
	select {
	case in = <-a.Inbound():
	case <-time.After(time.Second):
		t.Fatal("no inbound emitted")
	}
	if in.Channel != "watch" || in.MsgID != posted.Message.ID || in.UserID != DefaultUserID || in.Text != "what is on my calendar" {
		t.Fatalf("inbound = %+v", in)
	}

	// The reaction chain drives the status the wrist sees.
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionReceived) // ignored
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionWorking)
	fr := fetch(t, h, "?after="+itoa(posted.Message.Seq))
	if len(fr.Messages) != 1 || fr.Messages[0].Status != StatusWorking || fr.Pending != 1 {
		t.Fatalf("after working: %+v pending=%d", fr.Messages, fr.Pending)
	}

	// Reply lands attributed to the in-flight message; then done.
	if _, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: in.UserID, Text: "Two meetings."}); err != nil {
		t.Fatal(err)
	}
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionDone)
	fr = fetch(t, h, "?after="+itoa(fr.LatestSeq))
	if len(fr.Messages) != 2 {
		t.Fatalf("want reply + status update, got %+v", fr.Messages)
	}
	var reply, user *Message
	for i := range fr.Messages {
		switch fr.Messages[i].Role {
		case RoleAgent:
			reply = &fr.Messages[i]
		case RoleUser:
			user = &fr.Messages[i]
		}
	}
	if reply == nil || reply.Kind != KindReply || reply.ReplyTo != posted.Message.ID || reply.Text != "Two meetings." {
		t.Fatalf("reply = %+v", reply)
	}
	if user == nil || user.Status != StatusDone || fr.Pending != 0 {
		t.Fatalf("user = %+v pending=%d", user, fr.Pending)
	}

	// A done turn never regresses.
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionWorking)
	if m, _ := a.Store().Get(in.MsgID); m.Status != StatusDone {
		t.Fatalf("regressed to %s", m.Status)
	}

	// Tail fetch (no after) returns distinct messages, newest snapshot each.
	tail := fetch(t, h, "")
	if len(tail.Messages) != 2 || tail.Messages[0].Status != StatusDone || tail.Messages[1].Role != RoleAgent {
		t.Fatalf("tail = %+v", tail.Messages)
	}
}

func TestFailureNoticeIsAttributedAsFailure(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	rec := do(t, h, http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(`{"text":"do it"}`), "application/json")
	if rec.Code != 202 {
		t.Fatal(rec.Code)
	}
	in := <-a.Inbound()
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionWorking)
	// RouteInbound's defers: the final 😱 reaction runs BEFORE the notice Send.
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionError)
	_, _ = a.Send(context.Background(), channel.OutboundMsg{ChatID: in.UserID, Text: "Sorry, the turn timed out after 15m."})
	tail := fetch(t, h, "")
	if len(tail.Messages) != 2 || tail.Messages[1].Kind != KindFailure || tail.Messages[1].ReplyTo != in.MsgID || tail.Messages[0].Status != StatusFailed {
		t.Fatalf("tail = %+v", tail.Messages)
	}
}

func TestVoiceUploadCarriesTranscriptAndMedia(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="memo.m4a"`},
		"Content-Type":        {"audio/mp4"},
	}
	part, _ := mw.CreatePart(hdr)
	_, _ = part.Write([]byte("AACDATA"))
	_ = mw.WriteField("duration_s", "6")
	_ = mw.Close()
	rec := do(t, h, http.MethodPost, "/watch/messages", "wt", &buf, mw.FormDataContentType())
	if rec.Code != 202 {
		t.Fatalf("voice post: %d %s", rec.Code, rec.Body.String())
	}
	var posted struct {
		Message Message `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &posted)
	if posted.Message.Kind != KindVoice || posted.Message.Text != "book the train to bern" || posted.Message.DurationS != 6 {
		t.Fatalf("receipt = %+v", posted.Message)
	}
	in := <-a.Inbound()
	if len(in.Media) != 1 || in.Media[0].Kind != media.KindAudio || in.Media[0].Transcript != "book the train to bern" || in.Text != "" {
		t.Fatalf("inbound = %+v", in)
	}
	// No media service: a file upload is a clear 503, not a silent text turn.
	st, _ := OpenStore("")
	noMedia := New("wt", st).Handler()
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	p2, _ := mw2.CreatePart(hdr)
	_, _ = p2.Write([]byte("x"))
	_ = mw2.Close()
	if rec := do(t, noMedia, http.MethodPost, "/watch/messages", "wt", &buf2, mw2.FormDataContentType()); rec.Code != 503 {
		t.Fatalf("no media: %d", rec.Code)
	}
}

func TestEmptyAndBadPosts(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	if rec := do(t, h, http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(`{"text":"   "}`), "application/json"); rec.Code != 400 {
		t.Fatalf("blank: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(`nope`), "application/json"); rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, "/watch/messages", "wt", nil, ""); rec.Code != 405 {
		t.Fatalf("delete: %d", rec.Code)
	}
}

func TestLongPollWakesOnAppend(t *testing.T) {
	a, st := newAdapter(t)
	a.maxWait = 5 * time.Second
	h := a.Handler()
	first := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "hello"})
	done := make(chan fetchResp, 1)
	start := time.Now()
	go func() { done <- fetch(t, h, "?after="+itoa(first.Seq)+"&wait=5") }()
	time.Sleep(150 * time.Millisecond)
	_ = a.Notify(notify.Notification{Source: "job:x", Level: notify.LevelIssue, Text: "run failed"})
	select {
	case fr := <-done:
		if time.Since(start) > 3*time.Second {
			t.Fatalf("long-poll did not wake early: %v", time.Since(start))
		}
		if len(fr.Messages) != 1 || fr.Messages[0].Kind != KindNotice || !strings.Contains(fr.Messages[0].Text, "job:x: run failed") {
			t.Fatalf("woke with %+v", fr.Messages)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("long-poll never returned")
	}
	// Nothing new: returns empty after the wait, capped.
	a.maxWait = 200 * time.Millisecond
	start = time.Now()
	fr := fetch(t, h, "?after=999999&wait=30")
	if len(fr.Messages) != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("idle poll: %+v in %v", fr.Messages, time.Since(start))
	}
}

func TestStorePersistsReplaysAndCompacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.jsonl")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "a", Status: StatusQueued})
	st.Update(u.ID, func(m *Message) bool { m.Status = StatusDone; return true })
	st.Append(Message{Role: RoleAgent, Kind: KindReply, Text: "b", ReplyTo: u.ID})
	_ = st.Close()

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs, latest, reset := st2.Since(0, 50)
	if len(msgs) != 2 || msgs[0].Status != StatusDone || msgs[1].Text != "b" || latest != 3 || reset {
		t.Fatalf("replayed = %+v latest=%d reset=%v", msgs, latest, reset)
	}
	// Seq continues after the replayed max.
	n := st2.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "c"})
	if n.Seq != 4 {
		t.Fatalf("seq after replay = %d", n.Seq)
	}
	_ = st2.Close()

	// Compaction: many status flips collapse to one line per id on reopen.
	st3, _ := OpenStore(path)
	for i := 0; i < 20; i++ {
		st3.Update(u.ID, func(m *Message) bool { m.Text = "a" + itoa(int64(i)); return true })
	}
	_ = st3.Close()
	st4, err := openStore(path, 10) // 24 lines on disk > 10: compacts on open
	if err != nil {
		t.Fatal(err)
	}
	if lines, _ := st4.replayCount(); lines != 3 {
		t.Fatalf("not compacted: %d lines", lines)
	}
	if m, _ := st4.Get(u.ID); m.Text != "a19" {
		t.Fatalf("latest snapshot lost: %+v", m)
	}
}

func TestSinceResetWhenLogTrimmed(t *testing.T) {
	st, _ := OpenStore("")
	st.maxLog = 20
	var first Message
	for i := 0; i < 60; i++ {
		m := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: itoa(int64(i))})
		if i == 0 {
			first = m
		}
	}
	msgs, _, reset := st.Since(first.Seq, 50)
	if !reset || len(msgs) == 0 {
		t.Fatalf("expected reset tail, got reset=%v n=%d", reset, len(msgs))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ---- paging (lazy history loading) ----

func seedConversation(t *testing.T, st *Store, pairs int) []Message {
	t.Helper()
	var all []Message
	for i := 0; i < pairs; i++ {
		u := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "q" + strconv.Itoa(i), Status: StatusDone})
		a := st.Append(Message{Role: RoleAgent, Kind: KindReply, Text: "a" + strconv.Itoa(i), ReplyTo: u.ID})
		all = append(all, u, a)
	}
	return all
}

func TestTailAndPageWalkBackwards(t *testing.T) {
	st, _ := OpenStore("")
	all := seedConversation(t, st, 30) // 60 messages

	tail, more, latest := st.Tail(10)
	if len(tail) != 10 || !more || latest != 60 {
		t.Fatalf("tail: n=%d more=%v latest=%d", len(tail), more, latest)
	}
	if tail[9].ID != all[59].ID || tail[0].ID != all[50].ID {
		t.Fatalf("tail window wrong: %s..%s", tail[0].Text, tail[9].Text)
	}

	// Walk back to the very beginning; every message appears exactly once.
	seen := map[string]bool{}
	for _, m := range tail {
		seen[m.ID] = true
	}
	anchor := tail[0].ID
	for i := 0; i < 20 && more; i++ {
		var page []Message
		page, more, _ = st.Page(anchor, 10)
		if len(page) == 0 {
			t.Fatal("empty page while has_more was true")
		}
		for _, m := range page {
			if seen[m.ID] {
				t.Fatalf("message %s served twice", m.ID)
			}
			seen[m.ID] = true
		}
		if page[len(page)-1].Seq >= st.mustGet(t, anchor).Seq {
			t.Fatalf("page is not strictly older than the anchor")
		}
		anchor = page[0].ID
	}
	if more {
		t.Fatal("never reached the start of the conversation")
	}
	if len(seen) != 60 {
		t.Fatalf("walked %d of 60 messages", len(seen))
	}
}

func TestPageWithUnknownAnchorFallsBackToTail(t *testing.T) {
	st, _ := OpenStore("")
	seedConversation(t, st, 5)
	page, more, _ := st.Page("gone", 4)
	if len(page) != 4 || !more {
		t.Fatalf("unknown anchor: n=%d more=%v", len(page), more)
	}
	if page[3].Text != "a4" {
		t.Fatalf("expected the tail, got %q", page[3].Text)
	}
	// The oldest message has nothing before it.
	first, _, _ := st.Tail(10)
	empty, more, _ := st.Page(first[0].ID, 10)
	if len(empty) != 0 || more {
		t.Fatalf("before the first message: n=%d more=%v", len(empty), more)
	}
}

func (s *Store) mustGet(t *testing.T, id string) Message {
	t.Helper()
	m, ok := s.Get(id)
	if !ok {
		t.Fatalf("no message %s", id)
	}
	return m
}

func TestFetchEndpointPagesAndReportsHasMore(t *testing.T) {
	a, st := newAdapter(t)
	h := a.Handler()
	seedConversation(t, st, 20) // 40 messages

	first := fetch(t, h, "?limit=10")
	if len(first.Messages) != 10 || !first.HasMore {
		t.Fatalf("first page: n=%d has_more=%v", len(first.Messages), first.HasMore)
	}
	if first.Messages[9].Text != "a19" {
		t.Fatalf("first page must be the NEWEST 10, got %q last", first.Messages[9].Text)
	}

	older := fetch(t, h, "?limit=10&before="+first.Messages[0].ID)
	if len(older.Messages) != 10 || !older.HasMore {
		t.Fatalf("older page: n=%d has_more=%v", len(older.Messages), older.HasMore)
	}
	if older.Messages[9].Seq >= first.Messages[0].Seq {
		t.Fatal("older page overlaps the first page")
	}

	// A before= request must NOT long-poll even when wait is set.
	start := time.Now()
	_ = fetch(t, h, "?limit=5&wait=25&before="+older.Messages[0].ID)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("history page long-polled (%v)", time.Since(start))
	}

	// Incremental fetches still work and carry the new message.
	last := fetch(t, h, "?limit=10")
	m := st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "later"})
	inc := fetch(t, h, "?after="+itoa(last.LatestSeq))
	if len(inc.Messages) != 1 || inc.Messages[0].ID != m.ID {
		t.Fatalf("incremental: %+v", inc.Messages)
	}
}

// ---- browser auth for the desktop UI ----

func postForm(t *testing.T, h http.Handler, path, origin, form string, extra func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if extra != nil {
		extra(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUIRouteSignInFlow(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()

	// No cookie: the page is served with 401 so the browser can show its form.
	rec := do(t, h, http.MethodGet, "/watch/ui", "", nil, "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatalf("bare /watch/ui: %d", rec.Code)
	}
	for _, hdr := range []string{"Cache-Control", "Referrer-Policy"} {
		if rec.Header().Get(hdr) == "" {
			t.Errorf("sign-in page lacks %s", hdr)
		}
	}

	// A token in the URL is NOT a sign-in any more: no cookie, still the form.
	rec = do(t, h, http.MethodGet, "/watch/ui?token=wt", "", nil, "")
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("GET ?token= must not sign in: %d cookies=%d", rec.Code, len(rec.Result().Cookies()))
	}

	// A wrong token in the form never sets a cookie.
	rec = postForm(t, h, "/watch/ui", "http://example.com", "token=nope", nil)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: %d cookies=%d", rec.Code, len(rec.Result().Cookies()))
	}

	// The right token in a same-origin POST sets a hardened cookie and 303s to
	// the clean URL, with the no-store headers ON the redirect.
	rec = postForm(t, h, "/watch/ui", "http://example.com", "token=wt", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/watch/ui" {
		t.Fatalf("good token: %d -> %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "same-origin" {
		t.Errorf("redirect lacks no-store/same-origin referrer policy: %v", rec.Header())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != tokenCookie || c.Value != "wt" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/watch/" {
		t.Fatalf("cookie not hardened: %+v", c)
	}
	if c.Secure {
		t.Fatal("plain HTTP must not set a Secure cookie")
	}

	// Behind a TLS-terminating proxy the cookie IS Secure.
	rec = postForm(t, h, "/watch/ui", "https://example.com", "token=wt", func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if rec.Code != http.StatusSeeOther || !rec.Result().Cookies()[0].Secure {
		t.Fatalf("proxied HTTPS must set a Secure cookie: %d", rec.Code)
	}

	// A foreign page must not be able to sign this browser into ITS token, and
	// neither may an opaque `Origin: null` (what a no-referrer page sends).
	for _, origin := range []string{"https://evil.example", "null"} {
		rec = postForm(t, h, "/watch/ui", origin, "token=wt", nil)
		if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("origin %q sign-in: %d cookies=%d", origin, rec.Code, len(rec.Result().Cookies()))
		}
	}
	// Fetch Metadata alone proves same-origin, even without a usable Origin.
	rec = postForm(t, h, "/watch/ui", "null", "token=wt", func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("Sec-Fetch-Site same-origin sign-in = %d", rec.Code)
	}
}

func withCookie(t *testing.T, h http.Handler, method, path, origin string, body *bytes.Buffer, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: "wt"})
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The cookie authenticates reads freely; WRITES need a matching Origin,
// because SameSite=Strict still admits a sibling host of the same domain.
func TestCookieWritesRequireSameOrigin(t *testing.T) {
	a, st := newAdapter(t)
	h := a.Handler()
	st.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "hi"})

	if rr := withCookie(t, h, http.MethodGet, "/watch/messages", "", nil, ""); rr.Code != 200 {
		t.Fatalf("cookie GET without origin = %d, want 200", rr.Code)
	}
	body := `{"text":"hi"}`
	// Same origin (the page's own fetch): accepted.
	if rr := withCookie(t, h, http.MethodPost, "/watch/messages", "http://example.com", bytes.NewBufferString(body), "application/json"); rr.Code != 202 {
		t.Fatalf("same-origin cookie POST = %d %s", rr.Code, rr.Body.String())
	}
	<-a.Inbound()
	// A sibling host of the same site: same-site for the cookie, refused here.
	if rr := withCookie(t, h, http.MethodPost, "/watch/messages", "https://site.example.com", bytes.NewBufferString(body), "application/json"); rr.Code != http.StatusForbidden {
		t.Fatalf("sibling-origin cookie POST = %d, want 403", rr.Code)
	}
	// Fetch Metadata from a sibling site is refused too.
	{
		req := httptest.NewRequest(http.MethodPost, "/watch/messages", bytes.NewBufferString(body))
		req.AddCookie(&http.Cookie{Name: tokenCookie, Value: "wt"})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "null")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("same-site (not same-origin) fetch metadata = %d, want 403", rr.Code)
		}
	}
	// No Origin at all (a non-browser client that somehow has the cookie): refused.
	if rr := withCookie(t, h, http.MethodPost, "/watch/messages", "", bytes.NewBufferString(body), "application/json"); rr.Code != http.StatusForbidden {
		t.Fatalf("origin-less cookie POST = %d, want 403", rr.Code)
	}
	// The multipart form shape a hostile page would use is refused the same way.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", "forged")
	_ = mw.Close()
	if rr := withCookie(t, h, http.MethodPost, "/watch/messages", "https://site.example.com", &buf, mw.FormDataContentType()); rr.Code != http.StatusForbidden {
		t.Fatalf("sibling-origin multipart = %d, want 403", rr.Code)
	}
	// The bearer header is unaffected by all of this.
	if rec := do(t, h, http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(body), "application/json"); rec.Code != 202 {
		t.Fatalf("bearer POST = %d", rec.Code)
	}
	<-a.Inbound()
	// And the JSON API never accepts a token in the URL.
	if rec := do(t, h, http.MethodGet, "/watch/messages?token=wt", "", nil, ""); rec.Code != 401 {
		t.Fatalf("query token on the API = %d, want 401", rec.Code)
	}
}

func TestAuthBrakeEngagesAfterRepeatedFailuresOnly(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	for i := 0; i < authFailLimit; i++ {
		if rec := do(t, h, http.MethodGet, "/watch/ping", "wrong", nil, ""); rec.Code != 401 {
			t.Fatalf("failure %d = %d", i, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodGet, "/watch/ping", "wrong", nil, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures = %d, want 429", authFailLimit, rec.Code)
	}
	// A valid token is never slowed by the brake.
	if rec := do(t, h, http.MethodGet, "/watch/ping", "wt", nil, ""); rec.Code != 200 {
		t.Fatalf("valid token while braked = %d", rec.Code)
	}
	// The window turns and guessing is answered with 401 again.
	a.now = func() time.Time { return time.Now().UTC().Add(2 * authFailWindow) }
	if rec := do(t, h, http.MethodGet, "/watch/ping", "wrong", nil, ""); rec.Code != 401 {
		t.Fatalf("after the window = %d, want 401", rec.Code)
	}
}

func TestPingIsReadOnly(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := do(t, h, m, "/watch/ping", "wt", nil, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /watch/ping = %d, want 405", m, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodGet, "/watch/ping", "wt", nil, ""); rec.Code != 200 {
		t.Errorf("GET /watch/ping = %d", rec.Code)
	}
}

// A huge text field must not become a store record that the next start
// cannot read. Both request shapes are capped, and the store itself would
// truncate anything that slipped through.
func TestOversizedTextIsRefusedOnBothShapes(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	big := strings.Repeat("a", maxInboundText+10)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", big)
	_ = mw.Close()
	if rec := do(t, h, http.MethodPost, "/watch/messages", "wt", &buf, mw.FormDataContentType()); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart oversize = %d", rec.Code)
	}
	j, _ := json.Marshal(map[string]string{"text": big})
	if rec := do(t, h, http.MethodPost, "/watch/messages", "wt", bytes.NewBuffer(j), "application/json"); rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Fatalf("json oversize = %d", rec.Code)
	}
	if a.store.Pending() != 0 {
		t.Fatal("an oversized message must not be stored")
	}
}

func TestStoreSurvivesAnOversizedAndATornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.jsonl")
	st, _ := OpenStore(path)
	good := st.Append(Message{Role: RoleUser, Kind: KindText, Text: "keep me", Status: StatusDone})
	_ = st.Close()

	// A record beyond the replay line limit, then a torn tail with no newline.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	huge, _ := json.Marshal(Message{ID: "huge", Role: RoleAgent, Kind: KindReply, Text: strings.Repeat("x", maxLineBytes+1)})
	_, _ = f.Write(append(huge, '\n'))
	_, _ = f.Write([]byte(`{"seq":99,"id":"torn","role":"agent","kind":"reply","text":"cut off`))
	_ = f.Close()

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("a bad line must not prevent the store from opening: %v", err)
	}
	if _, ok := st2.Get(good.ID); !ok {
		t.Fatal("the good record was lost")
	}
	if _, ok := st2.Get("huge"); ok {
		t.Fatal("the oversized record should have been skipped")
	}
	// The next append lands on its own line, not glued to the torn fragment.
	n := st2.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: "after"})
	_ = st2.Close()
	st3, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st3.Get(n.ID); !ok {
		t.Fatal("the record appended after a torn tail was not readable on the next open")
	}
	if _, ok := st3.Get("torn"); ok {
		t.Fatal("the torn fragment must not resurrect as a record")
	}
}

func TestAppendTruncatesAbsurdText(t *testing.T) {
	st, _ := OpenStore("")
	m := st.Append(Message{Role: RoleAgent, Kind: KindReply, Text: strings.Repeat("y", maxTextBytes*2)})
	if len(m.Text) > maxTextBytes || !strings.HasSuffix(m.Text, "[truncated by agentd]") {
		t.Fatalf("len=%d suffix=%q", len(m.Text), m.Text[len(m.Text)-30:])
	}
}

func TestSummaryHalfIsStoredAsItsOwnKind(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	rec := do(t, h, http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(`{"text":"q"}`), "application/json")
	if rec.Code != 202 {
		t.Fatal(rec.Code)
	}
	in := <-a.Inbound()
	_ = a.Ack(in.UserID, in.MsgID, channel.ReactionWorking)
	_, _ = a.Send(context.Background(), channel.OutboundMsg{ChatID: in.UserID, Text: "long", Silent: true})
	_, _ = a.Send(context.Background(), channel.OutboundMsg{ChatID: in.UserID, Text: "short", Summary: true})
	tail := fetch(t, h, "")
	kinds := []string{}
	for _, m := range tail.Messages {
		if m.Role == RoleAgent {
			kinds = append(kinds, m.Kind)
		}
	}
	if strings.Join(kinds, ",") != KindReply+","+KindSummary {
		t.Fatalf("agent kinds = %v", kinds)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A split reply's long half must reach the mirror WITHOUT a notification, and
// its summary with one: otherwise the Telegram push is the truncated half.
func TestMirrorCarriesSilenceOfTheLongHalf(t *testing.T) {
	a, _ := newAdapter(t)
	type echoed struct {
		text   string
		silent bool
	}
	var mu sync.Mutex
	var got []echoed
	a.WithMirror(func(text string, silent bool) {
		mu.Lock()
		got = append(got, echoed{text, silent})
		mu.Unlock()
	})

	_, _ = a.Send(context.Background(), channel.OutboundMsg{ChatID: "c", Text: "the long answer", Silent: true})
	_, _ = a.Send(context.Background(), channel.OutboundMsg{ChatID: "c", Text: "the summary", Silent: false})

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("mirror saw %d messages, want 2: %+v", len(got), got)
	}
	if !got[0].silent || got[0].text != "⌚ the long answer" {
		t.Errorf("long half must be mirrored silently, got %+v", got[0])
	}
	if got[1].silent || got[1].text != "⌚ the summary" {
		t.Errorf("summary must be mirrored with a notification, got %+v", got[1])
	}
}
