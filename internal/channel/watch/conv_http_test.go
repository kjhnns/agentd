package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
)

func rawFetch(t *testing.T, a *Adapter, q string) map[string]json.RawMessage {
	t.Helper()
	rec := do(t, a.Handler(), http.MethodGet, "/watch/messages"+q, "wt", nil, "")
	if rec.Code != 200 {
		t.Fatalf("fetch %s: %d %s", q, rec.Code, rec.Body.String())
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var k []string
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

// TestMainIsUnchangedForAClientWithoutConvOrScope: the wrist, the
// complications, /watch/ui and old app builds read exactly today's shape:
// the main thread only, rows without conv, no conversation records, in the
// newest page, the incremental long-poll and a history page alike.
func TestMainIsUnchangedForAClientWithoutConvOrScope(t *testing.T) {
	a, _ := newAdapter(t)
	_, mainMsg, _ := postJSON(t, a, `{"text":"main question","request_id":"m1"}`)
	if _, err := a.Send(context.Background(), channel.OutboundMsg{Text: "main answer", InReplyTo: mainMsg.ID}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := postJSON(t, a, `{"text":"side question","conv":"b","request_id":"s1"}`); code != http.StatusAccepted {
		t.Fatalf("side post: %d", code)
	}
	rr := do(t, a.Handler(), http.MethodPut, "/watch/conversations/c", "wt", bytes.NewBufferString(`{"title":"Other"}`), "application/json")
	if rr.Code != 200 {
		t.Fatalf("put: %d", rr.Code)
	}

	for _, q := range []string{"", "?after=1", "?before=" + "nope"} {
		out := rawFetch(t, a, q)
		var rows []json.RawMessage
		_ = json.Unmarshal(out["messages"], &rows)
		want := 2
		if q == "?after=1" {
			want = 1 // the answer; the side question and the record changed later too
		}
		if len(rows) != want {
			t.Fatalf("%q: %d rows, want %d main ones", q, len(rows), want)
		}
		for _, r := range rows {
			s := string(r)
			if strings.Contains(s, "side question") || strings.Contains(s, `"conv"`) || strings.Contains(s, `"title"`) || strings.Contains(s, `"archived"`) {
				t.Fatalf("%q: a main reader saw %s", q, s)
			}
		}
		if got := keysOf(t, rows[0]); q != "?after=1" && got != "id,kind,request_id,role,seq,status,text,ts,updated_at" {
			t.Fatalf("%q: user row keys = %s", q, got)
		}
		top := []string{}
		for k := range out {
			top = append(top, k)
		}
		sort.Strings(top)
		if strings.Join(top, ",") != "has_more,has_more_changes,latest_seq,messages,pending,reset,server_time" {
			t.Fatalf("%q: top-level keys = %v", q, top)
		}
		if string(out["pending"]) != "1" {
			t.Fatalf("%q: pending = %s, want main only", q, out["pending"])
		}
	}
}

// TestScopeAllAndConvThread: scope=all carries every conversation with conv
// on every row plus the conversation records; conv=<id> is one thread.
func TestScopeAllAndConvThread(t *testing.T) {
	a, _ := newAdapter(t)
	postJSON(t, a, `{"text":"main question"}`)
	postJSON(t, a, `{"text":"side question","conv":"b"}`)
	fr := fetch(t, a.Handler(), "?scope=all")
	convs := map[string]int{}
	records := 0
	for _, m := range fr.Messages {
		if m.Conv == "" {
			t.Fatalf("scope=all row without conv: %+v", m)
		}
		convs[m.Conv]++
		if m.Kind == KindConv {
			records++
			if m.Role != RoleSystem || m.Archived == nil || m.Title != "side question" {
				t.Fatalf("conv record = %+v", m)
			}
		}
	}
	if convs["main"] != 1 || convs["b"] != 2 || records != 1 {
		t.Fatalf("scope=all = %v, records %d", convs, records)
	}
	fr = fetch(t, a.Handler(), "?conv=b")
	if len(fr.Messages) != 2 || fr.Messages[1].Text != "side question" || fr.Messages[1].Conv != "b" {
		t.Fatalf("conv=b = %+v", fr.Messages)
	}
	for _, q := range []string{"?conv=B!", "?scope=mine", "?scope=all&conv=b"} {
		if rec := do(t, a.Handler(), http.MethodGet, "/watch/messages"+q, "wt", nil, ""); rec.Code != 400 {
			t.Fatalf("%s: %d", q, rec.Code)
		}
	}
}

// TestConversationRoutesValidate: PUT/PATCH refuse main (400) and a bad id
// (400); PATCH of an unknown id is 404; PUT creates; PATCH archives.
func TestConversationRoutesValidate(t *testing.T) {
	a, _ := newAdapter(t)
	h := a.Handler()
	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPut, "/watch/conversations/main", `{"title":"x"}`, 400},
		{http.MethodPatch, "/watch/conversations/main", `{"archived":true}`, 400},
		{http.MethodPut, "/watch/conversations/Bad_Id", `{}`, 400},
		{http.MethodPut, "/watch/conversations/" + strings.Repeat("a", 41), `{}`, 400},
		{http.MethodPatch, "/watch/conversations/unknown-1", `{"title":"x"}`, 404},
		{http.MethodPut, "/watch/conversations/new-1", `{"title":"  Train   to Basel "}`, 200},
		{http.MethodPatch, "/watch/conversations/new-1", `{"archived":true}`, 200},
		{http.MethodPut, "/watch/conversations/new-1", `{bad`, 400},
	}
	for _, c := range cases {
		rec := do(t, h, c.method, c.path, "wt", bytes.NewBufferString(c.body), "application/json")
		if rec.Code != c.want {
			t.Fatalf("%s %s: %d %s, want %d", c.method, c.path, rec.Code, rec.Body.String(), c.want)
		}
	}
	rec := do(t, h, http.MethodGet, "/watch/conversations?archived=1", "wt", nil, "")
	var out struct {
		Conversations []ConvInfo `json:"conversations"`
		Next          *string    `json:"next"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Conversations) != 1 || out.Conversations[0].Title != "Train to Basel" || !out.Conversations[0].Archived || out.Next != nil {
		t.Fatalf("archived list = %s", rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/watch/conversations", "wt", nil, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Conversations) != 1 || out.Conversations[0].ID != "main" || out.Conversations[0].Title != "Main" {
		t.Fatalf("list = %s", rec.Body.String())
	}
}

// TestDefaultTitleAndExplicitTitle: a POST into an unknown conversation
// creates it with the first 60 characters of the first message; a title set
// by PUT is never replaced.
func TestDefaultTitleAndExplicitTitle(t *testing.T) {
	a, st := newAdapter(t)
	long := "Book   the train\nto Basel on Friday morning, second class, window seat please, thanks"
	body, _ := json.Marshal(map[string]string{"text": long, "conv": "t1"})
	code, rcpt, _ := postJSON(t, a, string(body))
	if code != 202 || rcpt.Conv != "t1" {
		t.Fatalf("receipt: %d %+v", code, rcpt)
	}
	info, _ := st.ConvInfo("t1")
	want := cleanTitle(long, 60)
	if info.Title != want || len([]rune(info.Title)) != 60 {
		t.Fatalf("title = %q", info.Title)
	}
	postJSON(t, a, `{"text":"second message","conv":"t1"}`)
	if info, _ := st.ConvInfo("t1"); info.Title != want || info.Count != 2 {
		t.Fatalf("after a second message: %+v", info)
	}
	do(t, a.Handler(), http.MethodPut, "/watch/conversations/t2", "wt", bytes.NewBufferString(`{"title":"Mine"}`), "application/json")
	postJSON(t, a, `{"text":"first message","conv":"t2"}`)
	if info, _ := st.ConvInfo("t2"); info.Title != "Mine" {
		t.Fatalf("explicit title replaced: %q", info.Title)
	}
}

// TestSideConversationsNeverMirror: neither the question nor the answer of a
// side conversation reaches the Telegram mirror; main still does.
func TestSideConversationsNeverMirror(t *testing.T) {
	a, _ := newAdapter(t)
	var mu sync.Mutex
	var got []string
	a.WithMirror(func(text string, silent bool) {
		mu.Lock()
		got = append(got, text)
		mu.Unlock()
	})
	_, side, _ := postJSON(t, a, `{"text":"side question","conv":"b"}`)
	_, _ = a.Send(context.Background(), channel.OutboundMsg{Text: "side answer", InReplyTo: side.ID})
	_, main, _ := postJSON(t, a, `{"text":"main question"}`)
	_, _ = a.Send(context.Background(), channel.OutboundMsg{Text: "main answer", InReplyTo: main.ID})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "|") != "⌚ You: main question|⌚ main answer" {
		t.Fatalf("mirror = %q", got)
	}
	if m, _ := a.store.Get(side.ID); m.Conv != "b" {
		t.Fatal("side question lost its conv")
	}
}

// TestPostCarriesConvAndContextConv: JSON and multipart both take conv and
// context_conv; the receipt shows the attachment without a server path; an
// unknown or self context is refused.
func TestPostCarriesConvAndContextConv(t *testing.T) {
	a, _ := newAdapter(t)
	postJSON(t, a, `{"text":"old thread","conv":"old"}`)
	drainInbound(a)
	if code, _, _ := postJSON(t, a, `{"text":"x","conv":"b","context_conv":"nope"}`); code != 400 {
		t.Fatalf("unknown context: %d", code)
	}
	if code, _, _ := postJSON(t, a, `{"text":"x","conv":"b","context_conv":"b"}`); code != 400 {
		t.Fatalf("self context: %d", code)
	}
	if code, _, _ := postJSON(t, a, `{"text":"x","conv":"Nope!"}`); code != 400 {
		t.Fatalf("bad conv: %d", code)
	}
	code, rcpt, _ := postJSON(t, a, `{"text":"use it","conv":"b","context_conv":"old"}`)
	if code != 202 || len(rcpt.Attachments) != 1 || rcpt.Attachments[0].ID != "old" || rcpt.Attachments[0].Title != "old thread" || rcpt.Attachments[0].Path != "" {
		t.Fatalf("receipt = %d %+v", code, rcpt)
	}
	in := drainInbound(a)
	if len(in) != 1 || in[0].Conv != "b" || in[0].ContextConv != "old" {
		t.Fatalf("inbound = %+v", in)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", "multipart side")
	_ = mw.WriteField("conv", "c")
	_ = mw.WriteField("context_conv", "main")
	mw.Close()
	rec := do(t, a.Handler(), http.MethodPost, "/watch/messages", "wt", &buf, mw.FormDataContentType())
	if rec.Code != 202 {
		t.Fatalf("multipart: %d %s", rec.Code, rec.Body.String())
	}
	in = drainInbound(a)
	if len(in) != 1 || in[0].Conv != "c" || in[0].ContextConv != "main" {
		t.Fatalf("multipart inbound = %+v", in)
	}
}

// TestPingAnnouncesConversations: a new app reads the feature flag.
func TestPingAnnouncesConversations(t *testing.T) {
	a, _ := newAdapter(t)
	rec := do(t, a.Handler(), http.MethodGet, "/watch/ping", "wt", nil, "")
	if !strings.Contains(rec.Body.String(), `"features":["conversations"]`) {
		t.Fatalf("ping = %s", rec.Body.String())
	}
}

// TestRestartReconciliationKeepsTheConversation: a queued side request is
// re-enqueued with its conversation, a working one with a committed turn is
// delivered into it, also for a row stored without its payload.
func TestRestartReconciliationKeepsTheConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.jsonl")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := New("wt", st)
	_, q, _ := postJSON(t, a, `{"text":"queued side","conv":"b","request_id":"q"}`)
	_, w, _ := postJSON(t, a, `{"text":"working side","conv":"b","request_id":"w"}`)
	_ = a.Ack("", w.ID, channel.ReactionWorking)
	// A row without its stored payload (the fallback path).
	_, _, _ = st.UpdateDurable(w.ID, func(m *Message) bool { m.Inbound = nil; return true })
	st.Close()

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b := New("wt", st2)
	var deliveredConv string
	req, del, _ := b.Reconcile(context.Background(),
		func(msgID, reqID string) (CommittedTurn, bool) {
			if msgID == w.ID {
				return CommittedTurn{SessionID: "side", Result: "side answer"}, true
			}
			return CommittedTurn{}, false
		},
		func(ctx context.Context, in channel.InboundMsg, sid, result string, finalOnly bool) error {
			deliveredConv = in.Conv
			_, err := b.Send(ctx, channel.OutboundMsg{Text: result, InReplyTo: in.MsgID})
			return err
		})
	if req != 1 || del != 1 {
		t.Fatalf("reconcile = %d requeued, %d delivered", req, del)
	}
	in := drainInbound(b)
	if len(in) != 1 || in[0].MsgID != q.ID || in[0].Conv != "b" {
		t.Fatalf("re-enqueued = %+v", in)
	}
	if deliveredConv != "b" {
		t.Fatalf("delivered into %q", deliveredConv)
	}
	fr := fetch(t, b.Handler(), "?conv=b")
	if last := fr.Messages[len(fr.Messages)-1]; last.Text != "side answer" || last.Conv != "b" {
		t.Fatalf("the stored answer is not in the side thread: %+v", last)
	}
}

// TestReviewRound1Fixes covers the Codex round-1 findings of R4.
func TestReviewRound1Fixes(t *testing.T) {
	t.Run("restart notice stays in its conversation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "watch.jsonl")
		st, _ := OpenStore(path)
		a := New("wt", st)
		_, w, _ := postJSON(t, a, `{"text":"lost side","conv":"b","request_id":"w"}`)
		_ = a.Ack("", w.ID, channel.ReactionWorking)
		st.Close()
		st2, _ := OpenStore(path)
		defer st2.Close()
		b := New("wt", st2)
		b.Reconcile(context.Background(),
			func(string, string) (CommittedTurn, bool) { return CommittedTurn{}, false },
			func(context.Context, channel.InboundMsg, string, string, bool) error { return nil })
		if main, _, _ := st2.TailF(MainOnly, 50); len(main) != 0 {
			t.Fatalf("the notice went to main: %+v", main)
		}
		fr := fetch(t, b.Handler(), "?conv=b")
		if last := fr.Messages[len(fr.Messages)-1]; last.Kind != KindFailure || last.Text != InterruptedNotice {
			t.Fatalf("side thread ends with %+v", last)
		}
	})
	t.Run("receipt pending is main only for an old client", func(t *testing.T) {
		a, _ := newAdapter(t)
		postJSON(t, a, `{"text":"side","conv":"b"}`)
		rec := do(t, a.Handler(), http.MethodPost, "/watch/messages", "wt", bytes.NewBufferString(`{"text":"main"}`), "application/json")
		if !strings.Contains(rec.Body.String(), `"pending":1}`) {
			t.Fatalf("receipt = %s", rec.Body.String())
		}
	})
	t.Run("a conv record always carries its title", func(t *testing.T) {
		a, _ := newAdapter(t)
		do(t, a.Handler(), http.MethodPut, "/watch/conversations/e1", "wt", bytes.NewBufferString(`{}`), "application/json")
		fr := fetch(t, a.Handler(), "?scope=all")
		if len(fr.Messages) != 1 || fr.Messages[0].Title != DefaultConvTitle {
			t.Fatalf("record = %+v", fr.Messages)
		}
		postJSON(t, a, `{"text":"the real first message","conv":"e1"}`)
		if info, _ := a.store.ConvInfo("e1"); info.Title != "the real first message" {
			t.Fatalf("title = %q", info.Title)
		}
	})
	t.Run("a failed admission does not pin the default title", func(t *testing.T) {
		a, st := newAdapter(t)
		writes := 0
		st.failWrite = func() error {
			writes++
			if writes == 2 { // the conversation record is written, the message is not
				return errors.New("disk full")
			}
			return nil
		}
		if code, _, _ := postJSON(t, a, `{"text":"lost first","conv":"f1"}`); code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d", code)
		}
		st.failWrite = nil
		postJSON(t, a, `{"text":"kept first","conv":"f1"}`)
		if info, _ := st.ConvInfo("f1"); info.Title != "kept first" {
			t.Fatalf("title = %q", info.Title)
		}
	})
}

// TestRejectedFirstMessageDoesNotPinTheTitle: a request refused for a full
// queue was never admitted; the next admitted one names the conversation.
func TestRejectedFirstMessageDoesNotPinTheTitle(t *testing.T) {
	a, st := newAdapter(t)
	for len(a.inbound) < cap(a.inbound) {
		a.inbound <- channel.InboundMsg{}
	}
	if code, _, _ := postJSON(t, a, `{"text":"Discard this","conv":"r1","request_id":"rr"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", code)
	}
	drainInbound(a)
	postJSON(t, a, `{"text":"Actual question","conv":"r1"}`)
	if info, _ := st.ConvInfo("r1"); info.Title != "Actual question" {
		t.Fatalf("title = %q", info.Title)
	}
}

// TestAnnouncingPutDoesNotSetTheTitle: the app announces a conversation
// with PUT {} (or an empty title) before its first message; only a
// non-empty title counts as set.
func TestAnnouncingPutDoesNotSetTheTitle(t *testing.T) {
	a, st := newAdapter(t)
	for id, body := range map[string]string{"p1": `{}`, "p2": `{"title":""}`, "p3": `{"title":"   "}`, "p4": `{"title":"Named"}`} {
		if rec := do(t, a.Handler(), http.MethodPut, "/watch/conversations/"+id, "wt", bytes.NewBufferString(body), "application/json"); rec.Code != 200 {
			t.Fatalf("put %s: %d", id, rec.Code)
		}
		postJSON(t, a, `{"text":"first   words\nhere","conv":"`+id+`"}`)
	}
	for id, want := range map[string]string{"p1": "first words here", "p2": "first words here", "p3": "first words here", "p4": "Named"} {
		if info, _ := st.ConvInfo(id); info.Title != want {
			t.Fatalf("%s title = %q, want %q", id, info.Title, want)
		}
	}
}
