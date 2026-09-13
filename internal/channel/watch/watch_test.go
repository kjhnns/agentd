package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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
