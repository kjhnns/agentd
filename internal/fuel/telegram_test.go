package fuel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// T25 (spec section 24): the Fuel Telegram bot against a fake Bot API and the
// fake agent of the chat broker.

const (
	tgTok  = "123456:fake-bot-token"
	tgJoe  = int64(7597951120)
	tgElse = int64(42)
)

type fakeTG struct {
	mu      sync.Mutex
	srv     *httptest.Server
	updates []map[string]any
	sent    []map[string]any
	actions int
	files   map[string][]byte
	polls   int
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/bot"+tgTok+"/") {
			f.mu.Lock()
			b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/file/bot"+tgTok+"/")]
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/bot"+tgTok+"/") {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		ok := func(v any) {
			b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
			_, _ = w.Write(b)
		}
		switch strings.TrimPrefix(r.URL.Path, "/bot"+tgTok+"/") {
		case "getUpdates":
			off := int64(p["offset"].(float64))
			f.mu.Lock()
			f.polls++
			var out []map[string]any
			for _, u := range f.updates {
				if int64(u["update_id"].(int)) >= off {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if len(out) == 0 {
				time.Sleep(30 * time.Millisecond)
				out = []map[string]any{}
			}
			ok(out)
		case "sendMessage":
			f.mu.Lock()
			f.sent = append(f.sent, p)
			f.mu.Unlock()
			ok(map[string]any{"message_id": 1})
		case "sendChatAction":
			f.mu.Lock()
			f.actions++
			f.mu.Unlock()
			ok(true)
		case "getFile":
			ok(map[string]any{"file_id": p["file_id"], "file_path": p["file_id"]})
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) push(id int, msg map[string]any) {
	f.mu.Lock()
	f.updates = append(f.updates, map[string]any{"update_id": id, "message": msg})
	f.mu.Unlock()
}

func (f *fakeTG) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		out = append(out, s["text"].(string))
	}
	return out
}

func (f *fakeTG) waitSent(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := f.sentTexts(); len(s) >= n {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d sent messages, got %q", n, f.sentTexts())
	return nil
}

func tgMsg(h *harness, from int64, id int, extra map[string]any) map[string]any {
	m := map[string]any{"message_id": id, "date": h.clk.Now().Unix(),
		"from": map[string]any{"id": from}, "chat": map[string]any{"id": from, "type": "private"}}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func startBot(t *testing.T, h *harness, f *fakeTG, mut ...func(*TelegramOptions)) (stop func()) {
	t.Helper()
	o := TelegramOptions{Token: tgTok, AllowUser: tgJoe, API: f.srv.URL, AppToken: testToken, Handler: h.h,
		StateDir: h.opts.StateDir, Client: loopbackClient(), PollSecs: 1, EntryWait: 8 * time.Second, Now: h.clk.Now,
		Convert: func(context.Context, []byte) ([]byte, error) { return testWAV(1), nil }}
	for _, m := range mut {
		m(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); NewTelegramBot(o).Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the bot did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// testWAV is a silent 16 kHz mono 16-bit WAV of secs seconds.
func testWAV(secs int) []byte {
	n := 16000 * 2 * secs
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n))
	b.Write(make([]byte, n))
	return b.Bytes()
}

func entryByClient(h *harness, clientID string) (Entry, bool) {
	for _, e := range h.svc.journal.Entries() {
		if e.ClientID == clientID {
			return e, true
		}
	}
	return Entry{}, false
}

// A text from Joe is one agent chat turn marked via telegram; the reply is
// the agent's text plus the totals line; the feed's user line carries via.
func TestTelegramTextLog(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged 200 g skyr.", food("skyr", 200, 130, 22, 0.2))
	f := newFakeTG(t)
	f.push(10, tgMsg(h, tgJoe, 501, map[string]any{"text": "200 g skyr"}))
	startBot(t, h, f)
	sent := f.waitSent(t, 1)
	if !strings.HasPrefix(sent[0], "Logged 200 g skyr.") || !strings.Contains(sent[0], "Today: energy 130 of") || !strings.Contains(sent[0], "protein 22 of") {
		t.Fatalf("reply: %q", sent[0])
	}
	e, ok := entryByClient(h, fmt.Sprintf("tg-%d-501", tgJoe))
	if !ok || e.Via != ViaTelegram || len(e.ItemIDs) != 1 || e.Chat != chatAgent {
		t.Fatalf("entry: %+v (found %v)", e, ok)
	}
	page, _ := h.svc.feed.Page(0, 50)
	found := false
	for _, it := range page {
		if it.Role == "user" && it.EntryID != nil && *it.EntryID == e.ID {
			found = it.Via == ViaTelegram
		}
	}
	if !found {
		t.Fatal("the feed user line is not marked via telegram")
	}
	// The wire: GET /fuel/feed shows "via".
	rec := h.do("GET", "/fuel/feed", nil, "")
	if !strings.Contains(rec.Body.String(), `"via":"telegram"`) {
		t.Fatalf("feed has no via: %s", rec.Body)
	}
	f.mu.Lock()
	reply := f.sent[0]
	f.mu.Unlock()
	if rp, _ := reply["reply_parameters"].(map[string]any); rp == nil || rp["message_id"].(float64) != 501 {
		t.Fatalf("not a reply to the message: %v", reply)
	}
	if _, err := os.Stat(filepath.Join(h.opts.StateDir, "telegram-offset")); err != nil {
		t.Fatalf("offset not saved: %v", err)
	}
}

// Another user, a group chat: ignored, no turn, no reply.
func TestTelegramOnlyJoe(t *testing.T) {
	h, fa := newChat(t)
	f := newFakeTG(t)
	f.push(1, tgMsg(h, tgElse, 1, map[string]any{"text": "200 g skyr"}))
	group := tgMsg(h, tgJoe, 2, map[string]any{"text": "200 g skyr"})
	group["chat"] = map[string]any{"id": -100, "type": "group"}
	f.push(2, group)
	f.push(3, tgMsg(h, tgJoe, 3, map[string]any{"text": "/start"}))
	startBot(t, h, f)
	sent := f.waitSent(t, 1)
	time.Sleep(300 * time.Millisecond)
	if len(f.sentTexts()) != 1 || !strings.Contains(sent[0], "Fuel on Telegram") {
		t.Fatalf("sent: %q", f.sentTexts())
	}
	if n := len(fa.turns); n != 0 || len(chatEntries(h)) != 0 {
		t.Fatalf("turns %d, entries %d", n, len(chatEntries(h)))
	}
}

// A re-delivered message (a crash before the offset was saved) replays: one
// entry, one write.
func TestTelegramRedeliveryReplays(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged an apple.", food("apple", 150, 80, 0.4, 0))
	f := newFakeTG(t)
	f.push(5, tgMsg(h, tgJoe, 77, map[string]any{"text": "an apple"}))
	stop := startBot(t, h, f)
	f.waitSent(t, 1)
	stop()
	_ = os.Remove(filepath.Join(h.opts.StateDir, "telegram-offset"))
	startBot(t, h, f)
	sent := f.waitSent(t, 2)
	if sent[1] != sent[0] {
		t.Fatalf("the replay differs: %q vs %q", sent[0], sent[1])
	}
	if n := len(chatEntries(h)); n != 1 {
		t.Fatalf("entries: %d", n)
	}
	if n := len(fa.turns); n != 1 {
		t.Fatalf("turns: %d", n)
	}
}

// A voice note goes through Convert and the transcription; an album of two
// photos is ONE turn with two photos.
func TestTelegramVoiceAndAlbum(t *testing.T) {
	h, fa := newChat(t)
	n := 0
	fa.script = func(msg string) (int, string) {
		n++
		return logsItems(h, "Logged it.", []map[string]any{food("eggs", 100, 140, 12, 3), food("rice", 300, 390, 8, 0.2)}[(n-1)%2])(msg)
	}
	f := newFakeTG(t)
	f.files["v1"] = []byte("OggS fake")
	f.files["p1"] = testJPEG(64, 48)
	f.files["p2"] = testJPEG(80, 60)
	f.push(1, tgMsg(h, tgJoe, 11, map[string]any{"voice": map[string]any{"file_id": "v1", "duration": 3}}))
	f.push(2, tgMsg(h, tgJoe, 12, map[string]any{"media_group_id": "g1", "caption": "lunch", "photo": []any{map[string]any{"file_id": "p1"}}}))
	f.push(3, tgMsg(h, tgJoe, 13, map[string]any{"media_group_id": "g1", "photo": []any{map[string]any{"file_id": "p2"}}}))
	startBot(t, h, f)
	sent := f.waitSent(t, 2)
	v, ok := entryByClient(h, fmt.Sprintf("tg-%d-11", tgJoe))
	if !ok || v.Transcript == nil || *v.Transcript != "two eggs" {
		t.Fatalf("voice entry: %+v (sent %q)", v, sent)
	}
	a, ok := entryByClient(h, fmt.Sprintf("tg-%d-gg1", tgJoe))
	if !ok || len(a.PhotoIDs) != 2 || a.UserText != "lunch" {
		t.Fatalf("album entry: %+v", a)
	}
	if _, ok := entryByClient(h, fmt.Sprintf("tg-%d-13", tgJoe)); ok {
		t.Fatal("the second album photo became its own entry")
	}
	if fa.files != 2 {
		t.Fatalf("photos uploaded to the agent: %d", fa.files)
	}
}

// A voice note over 2 minutes and a sticker: a reply, no turn.
func TestTelegramRefusals(t *testing.T) {
	h, fa := newChat(t)
	f := newFakeTG(t)
	f.push(1, tgMsg(h, tgJoe, 1, map[string]any{"voice": map[string]any{"file_id": "v", "duration": 300}}))
	f.push(2, tgMsg(h, tgJoe, 2, map[string]any{"sticker": map[string]any{"file_id": "s"}}))
	old := tgMsg(h, tgJoe, 3, map[string]any{"text": "a pear"})
	old["date"] = h.clk.Now().Add(-50 * time.Hour).Unix()
	f.push(3, old)
	startBot(t, h, f)
	sent := f.waitSent(t, 3)
	if !strings.Contains(sent[0], "2 minutes") || !strings.Contains(sent[1], "text, voice notes and photos") || !strings.Contains(sent[2], "older than 2 days") {
		t.Fatalf("sent: %q", sent)
	}
	if len(fa.turns) != 0 {
		t.Fatalf("turns: %d", len(fa.turns))
	}
}

// The duplicate guard (24.4): an app message whose local_time is near a
// Telegram message carries the Telegram lines and the rule; one far away
// does not; a Telegram message never carries it.
func TestTelegramDuplicateGuard(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged.", food("pasta", 250, 400, 14, 2))
	f := newFakeTG(t)
	f.push(1, tgMsg(h, tgJoe, 1, map[string]any{"text": "pasta 250 g"}))
	startBot(t, h, f)
	f.waitSent(t, 1)
	if strings.Contains(fa.lastTurn(), "TELEGRAM FALLBACK") {
		t.Fatal("a Telegram turn carries the guard")
	}
	fa.script = func(msg string) (int, string) { return 200, end(msg, "Is that a second serving?") }
	logAt := func(text string, at time.Time) {
		t.Helper()
		var b bytes.Buffer
		_ = json.NewEncoder(&b).Encode(map[string]any{"client_id": newUUIDish(), "text": text, "local_time": at.UTC().Format(time.RFC3339)})
		rec := h.do("POST", "/fuel/log", &b, "application/json")
		if rec.Code != 200 && rec.Code != 202 {
			t.Fatalf("log: %d %s", rec.Code, rec.Body)
		}
		waitChatIdle(t, h)
	}
	logAt("pasta 250 g", h.clk.Now().Add(-time.Hour))
	msg := fa.lastTurn()
	if !strings.Contains(msg, "TELEGRAM FALLBACK") || !strings.Contains(msg, "pasta 250 g") {
		t.Fatalf("no guard in the app turn:\n%s", msg)
	}
	if !strings.Contains(msg, telegramMark+" pasta 250 g") {
		t.Fatalf("the chat history does not mark the Telegram line:\n%s", msg)
	}
	logAt("a banana", h.clk.Now().Add(-4*time.Hour))
	if strings.Contains(fa.lastTurn(), "TELEGRAM FALLBACK") {
		t.Fatal("the guard shows outside the 3 h window")
	}
}

var uuidSeq int

func newUUIDish() string { uuidSeq++; return fmt.Sprintf("0c000000-%04d", uuidSeq) }

func waitChatIdle(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, e := range chatEntries(h) {
			if e.Agent == agentPending {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a chat turn did not finish")
}

func TestTelegramRender(t *testing.T) {
	target := 2600.0
	blocks := []Block{textBlock("Logged coffee."), {Type: "widget", Widget: "macros_today", Data: map[string]any{"macros": []map[string]any{
		{"key": "protein_g", "label": "Protein", "unit": "g", "consumed": 41.34, "target": 160},
		{"key": "kcal", "label": "Energy", "unit": "kcal", "consumed": 478.7, "target": target},
		{"key": "net_carbs_g", "label": "Net carbs", "unit": "g", "consumed": 9.2, "target": nil},
	}}}}
	got := renderTelegram(blocks)
	want := "Logged coffee.\n\nToday: energy 479 of 2600 kcal, protein 41.3 of 160 g"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if renderTelegram(nil) == "" {
		t.Fatal("empty render")
	}
}

func TestTelegramConfig(t *testing.T) {
	base := "token = \"env:T\"\n"
	if _, err := ParseDaemonConfig([]byte(base + "telegram_enabled = true\ntelegram_allow_user = \"7597951120\"\n")); err == nil {
		t.Fatal("no token accepted")
	}
	if _, err := ParseDaemonConfig([]byte(base + "telegram_enabled = true\ntelegram_token = \"env:B\"\n")); err == nil {
		t.Fatal("no user accepted")
	}
	if _, err := ParseDaemonConfig([]byte(base + "telegram_enabled = true\ntelegram_token = \"env:T\"\ntelegram_allow_user = \"1\"\n")); err == nil {
		t.Fatal("the app token as the bot token accepted")
	}
	if _, err := ParseDaemonConfig([]byte(base + "telegram_enabled = true\ntelegram_token = \"env:B\"\ntelegram_allow_user = \"1\"\ntelegram_api = \"http://127.0.0.1:9\"\n")); err == nil {
		t.Fatal("telegram_api accepted without test_mode")
	}
	if _, err := ParseDaemonConfig([]byte(base + "test_mode = true\ntelegram_enabled = true\ntelegram_token = \"env:B\"\ntelegram_allow_user = \"1\"\ntelegram_api = \"http://127.0.0.1:9\"\n")); err != nil {
		t.Fatalf("telegram_api refused with test_mode: %v", err)
	}
	c, err := ParseDaemonConfig([]byte(base + "telegram_enabled = true\ntelegram_token = \"env:B\"\ntelegram_allow_user = \"7597951120\"\n"))
	if err != nil || !c.TelegramEnabled || c.TelegramAllowUser != "7597951120" {
		t.Fatalf("%+v %v", c, err)
	}
}

// The real ffmpeg conversion gives a WAV that fueld's audio check accepts.
func TestTelegramOggToWAV(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	ogg := filepath.Join(dir, "a.ogg")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=d=2", "-c:a", "libopus", ogg).CombinedOutput(); err != nil {
		t.Skipf("no opus encoder: %s", out)
	}
	raw, _ := os.ReadFile(ogg)
	wav, err := oggToWAV(context.Background(), filepath.Join(dir, "tmp"), raw)
	if err != nil {
		t.Fatal(err)
	}
	ext, secs, err := sniffAudio(wav)
	if err != nil || ext != ".wav" || secs < 1.9 || secs > 2.1 {
		t.Fatalf("sniff: %s %v %v", ext, secs, err)
	}
	if _, err := oggToWAV(context.Background(), filepath.Join(dir, "tmp"), []byte("not audio")); err == nil {
		t.Fatal("garbage converted")
	}
}

func (f *fakeTG) pollCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.polls }

// An album whose second photo arrives after the first poll, with a message of
// another user in between, is still ONE turn with two photos; the offset
// moves past everything only after it.
func TestTelegramAlbumAcrossPolls(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged lunch.", food("lunch", 400, 600, 30, 5))
	f := newFakeTG(t)
	f.files["p1"] = testJPEG(64, 48)
	f.files["p2"] = testJPEG(80, 60)
	f.push(1, tgMsg(h, tgJoe, 21, map[string]any{"media_group_id": "77", "caption": "lunch", "photo": []any{map[string]any{"file_id": "p1"}}}))
	startBot(t, h, f)
	for deadline := time.Now().Add(5 * time.Second); f.pollCount() < 1 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	f.push(2, tgMsg(h, tgElse, 5, map[string]any{"text": "spam"}))
	f.push(3, tgMsg(h, tgJoe, 22, map[string]any{"media_group_id": "77", "photo": []any{map[string]any{"file_id": "p2"}}}))
	f.waitSent(t, 1)
	e, ok := entryByClient(h, fmt.Sprintf("tg-%d-g77", tgJoe))
	if !ok || len(e.PhotoIDs) != 2 {
		t.Fatalf("album entry: %+v (found %v), entries %d", e, ok, len(chatEntries(h)))
	}
	if n := len(chatEntries(h)); n != 1 {
		t.Fatalf("entries: %d", n)
	}
	time.Sleep(200 * time.Millisecond)
	raw, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "telegram-offset"))
	if strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("offset %q", raw)
	}
}

// A retryable error of the API (a transcription failure, 502) is tried again,
// not acknowledged.
func TestTelegramRetryableError(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged a plum.", food("plum", 60, 30, 0.4, 0))
	f := newFakeTG(t)
	f.push(1, tgMsg(h, tgJoe, 41, map[string]any{"text": "a plum"}))
	var mu sync.Mutex
	fail := 1
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := fail > 0 && r.URL.Path == "/fuel/log"
		if b {
			fail--
		}
		mu.Unlock()
		if b {
			writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "transcription failed"))
			return
		}
		h.h.ServeHTTP(w, r)
	})
	startBot(t, h, f, func(o *TelegramOptions) { o.Handler = wrapped; o.RetryWait = 100 * time.Millisecond })
	sent := f.waitSent(t, 1)
	if !strings.HasPrefix(sent[0], "Logged a plum.") {
		t.Fatalf("sent %q", sent)
	}
}

// fueld busy (503) beyond the request's own retries: the message is NOT
// acknowledged and is tried again; it is logged once.
func TestTelegramBusyRetries(t *testing.T) {
	h, fa := newChat(t)
	fa.script = logsItems(h, "Logged a pear.", food("pear", 170, 100, 0.6, 0))
	f := newFakeTG(t)
	f.push(1, tgMsg(h, tgJoe, 31, map[string]any{"text": "a pear"}))
	var mu sync.Mutex
	busy := 4
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b := busy > 0 && r.URL.Path == "/fuel/log"
		if b {
			busy--
		}
		mu.Unlock()
		if b {
			w.Header().Set("Retry-After", "1")
			writeErr(w, errf(http.StatusServiceUnavailable, "busy", true, "busy"))
			return
		}
		h.h.ServeHTTP(w, r)
	})
	startBot(t, h, f, func(o *TelegramOptions) { o.Handler = wrapped; o.RetryWait = 100 * time.Millisecond })
	sent := f.waitSent(t, 1)
	if !strings.HasPrefix(sent[0], "Logged a pear.") {
		t.Fatalf("sent %q", sent)
	}
	if n := len(chatEntries(h)); n != 1 {
		t.Fatalf("entries: %d", n)
	}
}

func TestTelegramClientID(t *testing.T) {
	m := &tgMessage{MessageID: 9}
	m.Chat.ID = tgJoe
	if got := tgClientID(m); got != "tg-7597951120-9" || !clientIDRe.MatchString(got) {
		t.Fatal(got)
	}
	m.MediaGroupID = "13999999999999999"
	if got := tgClientID(m); got != "tg-7597951120-g13999999999999999" || !clientIDRe.MatchString(got) {
		t.Fatal(got)
	}
	m.MediaGroupID = "bad id/x"
	if got := tgClientID(m); got != "tg-7597951120-9" {
		t.Fatal(got)
	}
}
