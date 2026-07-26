package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/media"
)

// fakeWacli records every invocation and replays canned stdout. It is the
// exec-boundary equivalent of the httptest server the telegram tests use: the
// adapter never runs a real wacli, and every assertion is on the real argv the
// adapter would have executed.
type fakeWacli struct {
	mu    sync.Mutex
	calls [][]string
	// respond returns stdout for one invocation; nil falls back to `{}`.
	respond func(args []string) ([]byte, error)
}

func (f *fakeWacli) run(ctx context.Context, args []string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(args)
	}
	return []byte(`{}`), nil
}

func (f *fakeWacli) argv() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// find returns the first recorded invocation whose args contain all needles.
func (f *fakeWacli) find(needles ...string) []string {
	for _, c := range f.argv() {
		joined := strings.Join(c, " ")
		ok := true
		for _, n := range needles {
			if !strings.Contains(joined, n) {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return nil
}

// waitFor polls until cond is true or the deadline passes. Reactions and media
// run on their own goroutines, so they are observed, not awaited.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func newAdapter(allow []string) (*Adapter, *fakeWacli) {
	f := &fakeWacli{}
	a := New("wacli", allow)
	a.run = f.run
	a.cursor = time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	return a, f
}

// listJSON builds a real-shaped `wacli messages list --json` response.
func listJSON(msgs ...waMessage) []byte {
	var resp waListResp
	resp.Success = true
	resp.Data.Messages = msgs
	b, _ := json.Marshal(resp)
	return b
}

func textMsg(chat, sender, id, text string) waMessage {
	return waMessage{
		ChatJID:   chat,
		SenderJID: sender,
		MsgID:     id,
		Text:      text,
		Timestamp: "2026-07-26T10:09:08Z",
	}
}

// ---------------------------------------------------------------- allowlist

func TestAllowlistEnforcement(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})

	// Bare number, full phone JID and @lid JID for the same peer all match:
	// WhatsApp delivers replies on a @lid JID distinct from the phone JID, so
	// matching only one form silently loses messages.
	for _, id := range []string{"41791234567", "41791234567@s.whatsapp.net", "41791234567@lid", "41791234567:12@s.whatsapp.net"} {
		if !a.allowed(id) {
			t.Errorf("%s should be allowed", id)
		}
	}
	for _, id := range []string{"", "41799999999", "41799999999@s.whatsapp.net", "120363000000@g.us"} {
		if a.allowed(id) {
			t.Errorf("%s should be rejected", id)
		}
	}

	f.respond = func(args []string) ([]byte, error) {
		return listJSON(
			textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "M1", "hi"),
			textMsg("41799999999@s.whatsapp.net", "41799999999@s.whatsapp.net", "M2", "sneaky"),
		), nil
	}
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	select {
	case m := <-a.Inbound():
		if m.UserID != "41791234567@s.whatsapp.net" || m.Text != "hi" || m.MsgID != "M1" {
			t.Fatalf("got %+v", m)
		}
	default:
		t.Fatal("expected an inbound message from the allowlisted chat")
	}
	select {
	case m := <-a.Inbound():
		t.Fatalf("did not expect a second message, got %+v", m)
	default:
	}
}

func TestStartRefusesEmptyAllowlist(t *testing.T) {
	// WhatsApp is a wide-open inbound surface; an empty allowlist must be loud.
	a := New("wacli", nil)
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("expected Start to refuse an empty allowlist")
	}
}

// ------------------------------------------------------------ inbound parse

func TestInboundParsing(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) {
		m := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "3BC0D1F4B1C2", "ah - wir waren auch schon im Indigo")
		return listJSON(m), nil
	}
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	in := <-a.Inbound()
	if in.Channel != "whatsapp" {
		t.Errorf("channel = %q", in.Channel)
	}
	if in.MsgID != "3BC0D1F4B1C2" {
		t.Errorf("msg id = %q", in.MsgID)
	}
	// wacli 0.11.1 exposes no quoted/reply-to field, so ReplyTo stays empty on
	// this channel. Asserted so a future wacli that DOES expose it makes this
	// test fail loudly rather than the gap staying invisible.
	if in.ReplyTo != "" {
		t.Errorf("reply_to = %q, want empty (wacli exposes no quote field)", in.ReplyTo)
	}
	want := time.Date(2026, 7, 26, 10, 9, 8, 0, time.UTC).Unix()
	if in.TS != want {
		t.Errorf("ts = %d, want %d", in.TS, want)
	}
	// Adapters MUST NOT pre-bake marker text: Text is the raw message only.
	if in.Text != "ah - wir waren auch schon im Indigo" {
		t.Errorf("text = %q (adapter must not decorate it)", in.Text)
	}
	// The cursor must have advanced so the next poll does not re-read this.
	if !a.cursor.Equal(time.Date(2026, 7, 26, 10, 9, 8, 0, time.UTC)) {
		t.Errorf("cursor = %v, did not advance", a.cursor)
	}
	// The poll must ask wacli only for messages from others, oldest-first.
	c := f.find("messages", "list")
	if c == nil {
		t.Fatal("no messages list invocation recorded")
	}
	joined := strings.Join(c, " ")
	for _, want := range []string{"--json", "--from-them", "--asc", "--after", "--lock-wait"} {
		if !strings.Contains(joined, want) {
			t.Errorf("messages list missing %s: %v", want, c)
		}
	}
}

func TestGroupSenderDistinctFromChat(t *testing.T) {
	// Groups ARE supported: the chat is the session + allowlist key, the
	// participant is the identity. They must not collapse into one field.
	a, f := newAdapter([]string{"41789505264-1623055354@g.us"})
	f.respond = func(args []string) ([]byte, error) {
		return listJSON(textMsg(
			"41789505264-1623055354@g.us",
			"41789505264@s.whatsapp.net",
			"3AD3385E", "Our flight is cancelled")), nil
	}
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	in := <-a.Inbound()
	if in.UserID != "41789505264-1623055354@g.us" {
		t.Errorf("UserID = %q, want the group jid", in.UserID)
	}
	if in.Sender != "41789505264@s.whatsapp.net" {
		t.Errorf("Sender = %q, want the participant jid", in.Sender)
	}
	if in.UserID == in.Sender {
		t.Error("group chat identity and sender identity must differ")
	}
}

func TestOwnMessagesAndReactionsSkipped(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	mine := textMsg("41791234567@s.whatsapp.net", "me@s.whatsapp.net", "M1", "my own")
	mine.FromMe = true
	react := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "M2", "👍")
	react.ReactionToID = "M1"
	revoked := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "M3", "oops")
	revoked.Revoked = true
	empty := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "M4", "   ")

	f.respond = func(args []string) ([]byte, error) { return listJSON(mine, react, revoked, empty), nil }
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	select {
	case m := <-a.Inbound():
		t.Fatalf("expected no inbound, got %+v", m)
	default:
	}
}

func TestDuplicateMessageEmittedOnce(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) {
		return listJSON(textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "M1", "hi")), nil
	}
	for i := 0; i < 3; i++ {
		if err := a.pollOnce(context.Background()); err != nil {
			t.Fatalf("pollOnce %d: %v", i, err)
		}
	}
	if got := len(a.Inbound()); got != 1 {
		t.Fatalf("emitted %d messages, want exactly 1 (boundary dedup failed)", got)
	}
}

// ------------------------------------------------------------------- media

type stubTranscriber struct {
	out media.Transcript
	err error
}

func (s *stubTranscriber) Transcribe(ctx context.Context, path string) (media.Transcript, error) {
	return s.out, s.err
}

// TestVoiceNoteRoutedThroughIngest proves the voice path gets the SAME
// treatment as Telegram: download, hand to media.Ingest, transcript on the
// artifact, and no marker text baked by the adapter.
func TestVoiceNoteRoutedThroughIngest(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	a.WithMedia(&media.Service{
		Dir:         t.TempDir(),
		Transcriber: &stubTranscriber{out: media.Transcript{Text: "hallo joe", Language: "german", DurationS: 4}},
	})

	voice := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "V1", "")
	voice.MediaType = "audio"
	voice.MimeType = "audio/ogg"
	voice.Filename = "note.ogg"

	f.respond = func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "messages list"):
			return listJSON(voice), nil
		case strings.Contains(joined, "media download"):
			// wacli writes the file into --output; mimic that.
			dir := args[indexOf(args, "--output")+1]
			if err := os.WriteFile(filepath.Join(dir, "note.ogg"), []byte("OGGDATA"), 0o600); err != nil {
				return nil, err
			}
			return []byte(`{"success":true}`), nil
		}
		return []byte(`{}`), nil
	}

	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	var in channel.InboundMsg
	select {
	case in = <-a.Inbound():
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message emitted for the voice note")
	}
	if len(in.Media) != 1 {
		t.Fatalf("media artifacts = %d, want 1", len(in.Media))
	}
	art := in.Media[0]
	if art.Kind != media.KindAudio {
		t.Errorf("kind = %q, want audio", art.Kind)
	}
	if art.Transcript != "hallo joe" {
		t.Errorf("transcript = %q", art.Transcript)
	}
	// Text must stay EMPTY: the "[voice message, ...]" marker belongs to
	// session.RenderInbound, not to this adapter.
	if in.Text != "" {
		t.Errorf("text = %q, adapter must not pre-bake a voice marker", in.Text)
	}
	// The download must be addressed by chat + message id.
	c := f.find("media", "download")
	if c == nil {
		t.Fatal("no media download invocation recorded")
	}
	joined := strings.Join(c, " ")
	if !strings.Contains(joined, "--chat 41791234567@s.whatsapp.net") || !strings.Contains(joined, "--id V1") {
		t.Errorf("media download argv wrong: %v", c)
	}
}

func TestImageRoutedThroughIngestWithCaption(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	a.WithMedia(&media.Service{Dir: t.TempDir()})

	img := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "I1", "")
	img.MediaType = "image"
	img.MimeType = "image/jpeg"
	img.Filename = "photo.jpg"
	img.MediaCaption = "look at this"

	f.respond = func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "messages list"):
			return listJSON(img), nil
		case strings.Contains(joined, "media download"):
			dir := args[indexOf(args, "--output")+1]
			// 1x1 JPEG-ish header is enough: media.KindFor keys off name + mime.
			if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("\xff\xd8\xff\xe0JFIF"), 0o600); err != nil {
				return nil, err
			}
			return []byte(`{"success":true}`), nil
		}
		return []byte(`{}`), nil
	}

	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	var in channel.InboundMsg
	select {
	case in = <-a.Inbound():
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound message emitted for the image")
	}
	if len(in.Media) != 1 || in.Media[0].Kind != media.KindImage {
		t.Fatalf("expected one image artifact, got %+v", in.Media)
	}
	if in.Media[0].Path == "" {
		t.Error("image artifact should keep a path for the agent to Read")
	}
	// The caption is the text, verbatim, with no "[The user sent an image...]"
	// marker: that is session.RenderInbound's job.
	if in.Text != "look at this" {
		t.Errorf("text = %q, want the bare caption", in.Text)
	}
}

func TestMediaWithoutServiceDeclinesPolitely(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	// no WithMedia
	img := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "I1", "")
	img.MediaType = "image"
	f.respond = func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "messages list") {
			return listJSON(img), nil
		}
		return []byte(`{"success":true,"data":{"MsgID":"OUT1"}}`), nil
	}
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	// Never silence: a decline must be sent.
	if !waitFor(t, func() bool { return f.find("send", "text") != nil }) {
		t.Fatal("expected a polite decline send, got none")
	}
}

// TestUnsupportedMediaKindsDeclined pins the real MediaType vocabulary wacli
// 0.11.1 emits: image, audio, document are handled; sticker, gif and video get
// a one-line decline rather than silence.
func TestUnsupportedMediaKindsDeclined(t *testing.T) {
	for _, kind := range []string{"sticker", "gif", "video"} {
		t.Run(kind, func(t *testing.T) {
			a, f := newAdapter([]string{"41791234567"})
			a.WithMedia(&media.Service{Dir: t.TempDir()})
			m := textMsg("41791234567@s.whatsapp.net", "41791234567@s.whatsapp.net", "X1", "")
			m.MediaType = kind
			f.respond = func(args []string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "messages list") {
					return listJSON(m), nil
				}
				return []byte(`{"success":true,"data":{"MsgID":"OUT1"}}`), nil
			}
			if err := a.pollOnce(context.Background()); err != nil {
				t.Fatalf("pollOnce: %v", err)
			}
			if !waitFor(t, func() bool { return f.find("send", "text") != nil }) {
				t.Fatalf("%s should get a polite decline, got %v", kind, f.argv())
			}
			if f.find("media", "download") != nil {
				t.Errorf("%s must not be downloaded", kind)
			}
			select {
			case in := <-a.Inbound():
				t.Fatalf("%s must not produce a turn, got %+v", kind, in)
			default:
			}
		})
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------- outbound

func TestSendMessage(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) {
		return []byte(`{"success":true,"data":{"MsgID":"3EB0ABCDEF"}}`), nil
	}
	rcpt, err := a.Send(context.Background(), channel.OutboundMsg{
		ChatID: "41791234567@s.whatsapp.net", Text: "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rcpt.ID != "3EB0ABCDEF" {
		t.Errorf("receipt id = %q, want 3EB0ABCDEF", rcpt.ID)
	}
	c := f.find("send", "text")
	if c == nil {
		t.Fatal("no send text invocation recorded")
	}
	joined := strings.Join(c, " ")
	if !strings.Contains(joined, "--to 41791234567@s.whatsapp.net") || !strings.Contains(joined, "--message hello") {
		t.Errorf("send argv wrong: %v", c)
	}
	if !strings.Contains(joined, "--json") {
		t.Errorf("send must ask for --json to get a real message id: %v", c)
	}
}

func TestSendRefusesNonAllowlistedChat(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	_, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: "41799999999@s.whatsapp.net", Text: "hi"})
	if err == nil {
		t.Fatal("expected Send to refuse a non-allowlisted chat")
	}
	if len(f.argv()) != 0 {
		t.Errorf("nothing should have been executed, got %v", f.argv())
	}
}

func TestSendMediaStubbed(t *testing.T) {
	a, _ := newAdapter([]string{"41791234567"})
	_, err := a.Send(context.Background(), channel.OutboundMsg{
		ChatID: "41791234567@s.whatsapp.net", Media: []string{"x.jpg"},
	})
	if err == nil {
		t.Fatal("expected media send to be a stub error")
	}
}

func TestSendMissingIDIsAnError(t *testing.T) {
	// No id means no acceptance artifact, which must not read as success.
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) { return []byte(`{"success":true,"data":{}}`), nil }
	if _, err := a.Send(context.Background(), channel.OutboundMsg{
		ChatID: "41791234567@s.whatsapp.net", Text: "hi",
	}); err == nil {
		t.Fatal("expected an error when wacli returns no message id")
	}
}

// --------------------------------------------------------------- reactions

func TestAckReactionChain(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	chat := "41791234567@s.whatsapp.net"
	for _, e := range []string{channel.ReactionReceived, channel.ReactionWorking, channel.ReactionDone} {
		if err := a.Ack(chat, "M1", e); err != nil {
			t.Fatalf("Ack(%s): %v", e, err)
		}
	}
	if !waitFor(t, func() bool { return len(f.argv()) >= 3 }) {
		t.Fatalf("expected 3 reaction calls, got %v", f.argv())
	}
	var got []string
	for _, c := range f.argv() {
		i := indexOf(c, "--reaction")
		if i < 0 {
			t.Fatalf("invocation is not a reaction: %v", c)
		}
		got = append(got, c[i+1])
	}
	want := []string{channel.ReactionReceived, channel.ReactionWorking, channel.ReactionDone}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reaction %d = %q, want %q (chain must land in lifecycle order)", i, got[i], want[i])
		}
	}
}

func TestAckNoopWhenReactionsOff(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	a.WithReactions(false)
	if err := a.Ack("41791234567@s.whatsapp.net", "M1", channel.ReactionDone); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(f.argv()) != 0 {
		t.Errorf("reactions=false must execute nothing, got %v", f.argv())
	}
}

func TestAckIgnoresEmptyIDs(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	_ = a.Ack("", "M1", channel.ReactionDone)
	_ = a.Ack("41791234567@s.whatsapp.net", "", channel.ReactionDone)
	_ = a.Ack("41791234567@s.whatsapp.net", "M1", "")
	time.Sleep(50 * time.Millisecond)
	if len(f.argv()) != 0 {
		t.Errorf("expected no invocations, got %v", f.argv())
	}
}

func TestGroupReactionCarriesSender(t *testing.T) {
	// wacli needs --sender to address a reaction inside a group.
	a, f := newAdapter([]string{"120363000000@g.us"})
	a.enqueueReaction("120363000000@g.us", "M1", "41791234567@s.whatsapp.net", channel.ReactionReceived)
	if !waitFor(t, func() bool { return f.find("send", "react") != nil }) {
		t.Fatal("no reaction invocation recorded")
	}
	c := f.find("send", "react")
	if !strings.Contains(strings.Join(c, " "), "--sender 41791234567@s.whatsapp.net") {
		t.Errorf("group reaction must pass --sender: %v", c)
	}
}

// ------------------------------------------------------------------- misc

func TestWacliInvocationsAreSerialized(t *testing.T) {
	// wacli holds an EXCLUSIVE store lock, so overlapping invocations would
	// fail each other. Every call must go through the adapter's mutex.
	a, f := newAdapter([]string{"41791234567"})
	var live, maxLive int
	var mu sync.Mutex
	f.respond = func(args []string) ([]byte, error) {
		mu.Lock()
		live++
		if live > maxLive {
			maxLive = live
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		live--
		mu.Unlock()
		return []byte(`{"success":true,"data":{"MsgID":"X"}}`), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.Send(context.Background(), channel.OutboundMsg{
				ChatID: "41791234567@s.whatsapp.net", Text: "x",
			})
		}()
	}
	wg.Wait()
	if maxLive != 1 {
		t.Errorf("max concurrent wacli invocations = %d, want 1", maxLive)
	}
}

// TestLockWaitRespectsCallerDeadline guards the interaction with
// session.RouteInbound, which sends its failure notice on a detached 20s
// context: a longer --lock-wait would have wacli killed mid-wait and the user
// would get silence instead of the failure line.
func TestLockWaitRespectsCallerDeadline(t *testing.T) {
	if got := lockWaitFor(context.Background()); got != defaultLockWait {
		t.Errorf("no deadline: lock-wait = %v, want %v", got, defaultLockWait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := lockWaitFor(ctx)
	if got >= 20*time.Second {
		t.Errorf("lock-wait %v must stay inside the caller's 20s budget", got)
	}
	if got < 10*time.Second {
		t.Errorf("lock-wait %v is needlessly short for a 20s budget", got)
	}
	// An already-expired budget must not produce a negative or zero duration.
	dead, cancel2 := context.WithTimeout(context.Background(), -time.Second)
	defer cancel2()
	if got := lockWaitFor(dead); got <= 0 {
		t.Errorf("expired budget: lock-wait = %v, must stay positive", got)
	}
	// A very long budget is still capped.
	long, cancel3 := context.WithTimeout(context.Background(), time.Hour)
	defer cancel3()
	if got := lockWaitFor(long); got != defaultLockWait {
		t.Errorf("long budget: lock-wait = %v, want the %v cap", got, defaultLockWait)
	}
}

func TestSendForwardsBoundedLockWait(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) { return []byte(`{"success":true,"data":{"MsgID":"X"}}`), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Send(ctx, channel.OutboundMsg{ChatID: "41791234567@s.whatsapp.net", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	c := f.argv()[0]
	i := indexOf(c, "--lock-wait")
	if i < 0 {
		t.Fatalf("no --lock-wait in %v", c)
	}
	d, err := time.ParseDuration(c[i+1])
	if err != nil {
		t.Fatalf("unparsable lock-wait %q: %v", c[i+1], err)
	}
	if d >= 20*time.Second {
		t.Errorf("lock-wait %v exceeds the caller's budget", d)
	}
}

func TestStoreFlagForwarded(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	a.WithStore("/tmp/altstore")
	f.respond = func(args []string) ([]byte, error) { return []byte(`{"success":true,"data":{"MsgID":"X"}}`), nil }
	if _, err := a.Send(context.Background(), channel.OutboundMsg{
		ChatID: "41791234567@s.whatsapp.net", Text: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(strings.Join(f.argv()[0], " "), "--store /tmp/altstore") {
		t.Errorf("--store not forwarded: %v", f.argv()[0])
	}
}

func TestOwnSyncRunsRefresh(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	a.WithOwnSync(true)
	f.respond = func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "messages list") {
			return listJSON(), nil
		}
		return []byte(`{}`), nil
	}
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if f.find("sync", "--once") == nil {
		t.Errorf("ownSync must refresh the local DB first, got %v", f.argv())
	}
}

func TestOwnSyncOffDoesNotSync(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) { return listJSON(), nil }
	if err := a.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if f.find("sync", "--once") != nil {
		t.Errorf("sync must stay off by default so it does not fight an external syncer: %v", f.argv())
	}
}

func TestPollSurfacesWacliFailure(t *testing.T) {
	a, f := newAdapter([]string{"41791234567"})
	f.respond = func(args []string) ([]byte, error) {
		return nil, fmt.Errorf("store is locked (another wacli is running?)")
	}
	if err := a.pollOnce(context.Background()); err == nil {
		t.Fatal("expected the store-lock failure to surface, not be swallowed")
	}
}

var _ channel.Adapter = (*Adapter)(nil)
