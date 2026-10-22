package fuel

// The Fuel Telegram bot (spec 24): the fallback of the app when only a
// messaging pass works (a flight). The bot long-polls Telegram, accepts ONE
// user, and sends every message through the app's own POST /fuel/log, in
// process, with the app token and Via "telegram". So a Telegram message is a
// normal chat turn: the same agent, the same feed, the same corrections.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ViaTelegram marks an entry and its user line that came through the bot.
	ViaTelegram = "telegram"
	// telegramMark prefixes a Telegram line in the chat history of a turn.
	telegramMark = "[via Telegram]"
	// telegramDupWindow is the duplicate guard window (spec 24.4).
	telegramDupWindow = 3 * time.Hour

	tgMaxFile      = 20 << 20 // the Bot API download limit
	tgMaxVoiceSecs = 120
	tgMaxText      = 4000 // a Telegram message holds 4096 characters
)

type viaKey struct{}

// WithVia marks a request context as coming from a channel other than the app.
// Only in-process callers can set it; the wire never carries it.
func WithVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

func viaOf(ctx context.Context) string {
	v, _ := ctx.Value(viaKey{}).(string)
	if v == ViaTelegram {
		return v
	}
	return ""
}

// recentTelegram lists the Telegram user lines whose time (the time the food
// was eaten, else the arrival) is within win of at, oldest first, as
// "HH:MM text" lines for the duplicate guard.
func (s *Service) recentTelegram(at time.Time, win time.Duration, loc *time.Location) []string {
	page, _ := s.feed.Page(0, 200)
	var out []string
	for _, it := range page {
		if it.Role != "user" || it.Via != ViaTelegram {
			continue
		}
		t := it.At
		if it.EntryID != nil {
			if e, ok := s.journal.Entry(*it.EntryID); ok && !e.EatenAt.IsZero() {
				t = e.EatenAt
			}
		}
		if d := t.Sub(at); d > win || d < -win {
			continue
		}
		text := ""
		if it.Text != nil {
			text = strings.Join(strings.Fields(*it.Text), " ")
		}
		if len(it.PhotoIDs) > 0 {
			text = strings.TrimSpace(fmt.Sprintf("[%d photo(s)] %s", len(it.PhotoIDs), text))
		}
		out = append(out, t.In(loc).Format("15:04")+" "+clipRunes(text, 300))
	}
	if len(out) > 30 {
		out = out[len(out)-30:]
	}
	return out
}

// TelegramOptions configures the bot.
type TelegramOptions struct {
	Token     string       // the bot token
	AllowUser int64        // the ONE Telegram user id that is served
	API       string       // "https://api.telegram.org" unless a test sets it
	AppToken  string       // the app bearer for the in-process /fuel/log
	Handler   http.Handler // the fueld handler (svc.Handler())
	StateDir  string       // telegram-offset lives here
	Client    *http.Client // long-poll client; its timeout must exceed PollSecs
	PollSecs  int          // getUpdates timeout (50 unless a test sets it)
	EntryWait time.Duration
	Now       func() time.Time
	// RetryWait is the pause before a message that failed is tried again (30 s).
	RetryWait time.Duration
	// Ready reports that fueld serves requests; the bot polls only after it.
	Ready func() bool
	// Convert turns a voice note (OGG/Opus) into WAV; ffmpeg unless a test sets it.
	Convert func(ctx context.Context, in []byte) ([]byte, error)
}

// TelegramBot is the poller.
type TelegramBot struct {
	o       TelegramOptions
	offset  int64
	replies sync.WaitGroup
}

func NewTelegramBot(o TelegramOptions) *TelegramBot {
	o.StateDir = expandHome(o.StateDir)
	if o.API == "" {
		o.API = "https://api.telegram.org"
	}
	if o.PollSecs <= 0 {
		o.PollSecs = 50
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: time.Duration(o.PollSecs+20) * time.Second}
	}
	if o.RetryWait <= 0 {
		o.RetryWait = 30 * time.Second
	}
	if o.EntryWait <= 0 {
		o.EntryWait = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Convert == nil {
		dir := filepath.Join(o.StateDir, "tmp")
		o.Convert = func(ctx context.Context, in []byte) ([]byte, error) { return oggToWAV(ctx, dir, in) }
	}
	return &TelegramBot{o: o}
}

// ---- Bot API types (only the fields we read) ----

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	MessageID    int64  `json:"message_id"`
	Date         int64  `json:"date"`
	MediaGroupID string `json:"media_group_id"`
	From         *struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
	Photo   []struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
	} `json:"photo"`
	Voice    *tgFile `json:"voice"`
	Audio    *tgFile `json:"audio"`
	Document *struct {
		FileID   string `json:"file_id"`
		MimeType string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	} `json:"document"`
}

type tgFile struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type tgResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call runs one Bot API method. Its errors never contain the URL, which
// holds the token.
func (b *TelegramBot) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, "POST", b.o.API+"/bot"+b.o.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: bad request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.o.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("telegram %s: %s", method, errClass(err))
	}
	defer resp.Body.Close()
	var r tgResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, bad body", method, resp.StatusCode)
	}
	if !r.OK {
		e := &tgError{method: method, code: r.ErrorCode, desc: r.Description}
		if r.Parameters != nil {
			e.retryAfter = r.Parameters.RetryAfter
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("telegram %s: bad result", method)
		}
	}
	return nil
}

type tgError struct {
	method     string
	code       int
	desc       string
	retryAfter int
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram %s: %d %s", e.method, e.code, e.desc) }

// ---- the loop ----

func (b *TelegramBot) offsetPath() string { return filepath.Join(b.o.StateDir, "telegram-offset") }

func (b *TelegramBot) loadOffset() {
	raw, err := os.ReadFile(b.offsetPath())
	if err != nil {
		return
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
		b.offset = n
	}
}

func (b *TelegramBot) saveOffset(n int64) {
	b.offset = n
	tmp := b.offsetPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)+"\n"), 0o600); err == nil {
		_ = os.Rename(tmp, b.offsetPath())
	}
}

// Run polls until ctx ends, then waits for the open replies. An update is
// acknowledged (the offset moves past it) only after its message was handled,
// so a crash re-sends it: its client_id is derived from the message id, so
// the repeat replays and never writes twice. Telegram returns every
// unacknowledged update again on the next poll; the bot relies on that for
// albums and for retries.
func (b *TelegramBot) Run(ctx context.Context) {
	defer b.replies.Wait()
	for b.o.Ready != nil && !b.o.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	b.loadOffset()
	log.Printf("fuel: telegram bot polling (user %d)", b.o.AllowUser)
	backoff := time.Second
	albums := map[string]int{} // media_group_id -> photos seen at the last poll
	tries := map[int64]int{}   // first update id of a job -> failed rounds
	short := false
	for ctx.Err() == nil {
		secs := b.o.PollSecs
		if short {
			// An album is still arriving: look again shortly. A poll with
			// unacknowledged updates returns at once, so wait first.
			select {
			case <-ctx.Done():
				continue
			case <-time.After(1500 * time.Millisecond):
			}
			secs = 0
		}
		short = false
		var ups []tgUpdate
		err := b.call(ctx, "getUpdates", map[string]any{"offset": b.offset + 1, "timeout": secs, "allowed_updates": []string{"message"}}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var te *tgError
			if errors.As(err, &te) && te.code == 409 {
				log.Printf("fuel: telegram: another poller uses this bot token (409); retry in 60s")
				backoff = 60 * time.Second
			} else {
				log.Printf("fuel: %v; retry in %s", err, backoff)
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		jobs := tgJobs(ups, b.allowed)
		for k, j := range jobs {
			if ctx.Err() != nil {
				break
			}
			if j.msgs != nil {
				if g := j.msgs[0].MediaGroupID; g != "" && albums[g] != j.size {
					// The album grew since the last poll (or is new): wait for
					// the rest. Nothing from here on is acknowledged.
					albums[g] = j.size
					short = true
					break
				}
				if !b.handle(ctx, j.msgs) {
					tries[j.first]++
					if tries[j.first] < 5 && ctx.Err() == nil {
						log.Printf("fuel: telegram: message not handled (try %d); retry in %s", tries[j.first], b.o.RetryWait)
						select {
						case <-ctx.Done():
						case <-time.After(b.o.RetryWait):
						}
						break // not acknowledged: the next poll returns it
					}
					if ctx.Err() != nil {
						break
					}
					b.send(ctx, j.msgs[0].Chat.ID, j.msgs[0].MessageID, "Not logged: Fuel could not take this message after several tries. Send it again later or log it in the app.")
				}
				delete(tries, j.first)
				if g := j.msgs[0].MediaGroupID; g != "" {
					delete(albums, g)
				}
			} else {
				log.Printf("fuel: telegram: ignored a message from another chat")
			}
			b.saveOffset(ackAfter(jobs, k, ups))
		}
	}
	log.Printf("fuel: telegram bot stopped")
}

// tgJob is one unit of work: one message, one album, or one ignored update
// (msgs nil). Jobs are in the order of their first update.
type tgJob struct {
	first int64 // update id of the first update
	size  int   // updates of an album
	msgs  []*tgMessage
}

func tgJobs(ups []tgUpdate, allowed func(*tgMessage) bool) []tgJob {
	var jobs []tgJob
	album := map[string]int{} // media_group_id -> index in jobs
	for _, u := range ups {
		m := u.Message
		switch {
		case m == nil || !allowed(m):
			jobs = append(jobs, tgJob{first: u.UpdateID})
		case m.MediaGroupID != "":
			if i, ok := album[m.MediaGroupID]; ok {
				jobs[i].size++
				if len(jobs[i].msgs) < maxImages {
					jobs[i].msgs = append(jobs[i].msgs, m) // a fifth photo is dropped
				}
				continue
			}
			album[m.MediaGroupID] = len(jobs)
			jobs = append(jobs, tgJob{first: u.UpdateID, size: 1, msgs: []*tgMessage{m}})
		default:
			jobs = append(jobs, tgJob{first: u.UpdateID, msgs: []*tgMessage{m}})
		}
	}
	return jobs
}

// ackAfter is the offset to save once jobs[:k+1] are done: everything below
// the next job's first update, or the whole batch.
func ackAfter(jobs []tgJob, k int, ups []tgUpdate) int64 {
	if k+1 < len(jobs) {
		return jobs[k+1].first - 1
	}
	return ups[len(ups)-1].UpdateID
}

func (b *TelegramBot) allowed(m *tgMessage) bool {
	return m.From != nil && m.From.ID == b.o.AllowUser && m.Chat.Type == "private" && m.Chat.ID == b.o.AllowUser
}

const tgHelp = "Fuel on Telegram, for when the app has no connection (a flight).\n\n" +
	"Send what you ate or drank as text, a voice note or a photo. Ask \"how am I doing today?\" or any food question. " +
	"Corrections work as in the app: \"no, it was 100 g\", \"remove the coffee\".\n\n" +
	"Everything lands in the Fuel app too, marked via Telegram."

// handle sends one message (or one album) to /fuel/log and replies. It
// returns false when the message should be tried again later (a network
// failure, fueld busy); every other outcome has a reply and returns true.
func (b *TelegramBot) handle(ctx context.Context, msgs []*tgMessage) bool {
	first := msgs[0]
	chat := first.Chat.ID
	reply := func(text string) bool { b.send(ctx, chat, first.MessageID, text); return true }
	text := strings.TrimSpace(first.Text)
	if text == "/start" || text == "/help" {
		return reply(tgHelp)
	}
	at := time.Unix(first.Date, 0)
	if b.o.Now().Sub(at) > 47*time.Hour {
		return reply("This message is older than 2 days. Nothing was logged. Send it again with the day, e.g. \"yesterday at 13:00 ...\".")
	}
	in := tgInput{clientID: tgClientID(first), localTime: at.UTC().Format(time.RFC3339)}
	for _, m := range msgs {
		if t := strings.TrimSpace(m.Text + m.Caption); t != "" && in.text == "" {
			in.text = t
		}
	}
	// fetch downloads a file; a permanent failure replies, a transient one
	// asks for a retry.
	fetch := func(id string, size int64, what string) ([]byte, bool, bool) {
		raw, err := b.download(ctx, id, size)
		if err == nil {
			return raw, true, true
		}
		log.Printf("fuel: telegram %s download: %v", what, err)
		var te *tgError
		if errors.As(err, &te) && te.code >= 400 && te.code < 500 && te.code != 429 || errors.Is(err, errTGTooBig) {
			return nil, false, reply("I could not load the " + what + " (" + shortErr(err) + "). Nothing was logged.")
		}
		return nil, false, false
	}
	for _, m := range msgs {
		switch {
		case len(m.Photo) > 0:
			p := m.Photo[len(m.Photo)-1] // the largest size
			img, ok, done := fetch(p.FileID, p.FileSize, "photo")
			if !ok {
				return done
			}
			in.images = append(in.images, img)
		case m.Document != nil && (m.Document.MimeType == "image/jpeg" || m.Document.MimeType == "image/png"):
			img, ok, done := fetch(m.Document.FileID, m.Document.FileSize, "image")
			if !ok {
				return done
			}
			in.images = append(in.images, img)
		case m.Voice != nil || m.Audio != nil:
			f := m.Voice
			if f == nil {
				f = m.Audio
			}
			if f.Duration > tgMaxVoiceSecs {
				return reply("Voice notes up to 2 minutes, please. Nothing was logged.")
			}
			raw, ok, done := fetch(f.FileID, f.FileSize, "voice note")
			if !ok {
				return done
			}
			wav, err := b.o.Convert(ctx, raw)
			if err != nil {
				log.Printf("fuel: telegram voice: %v", err)
				return reply("I could not read the voice note. Nothing was logged. Send it as text.")
			}
			in.audio = wav
		}
	}
	if in.text == "" && in.audio == nil && len(in.images) == 0 {
		return reply("I can read text, voice notes and photos. Nothing was logged.")
	}
	if r := []rune(in.text); len(r) > maxTextChars {
		in.text = string(r[:maxTextChars])
	}
	_ = b.call(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
	code, body := b.post(ctx, in)
	if code == http.StatusServiceUnavailable || code == http.StatusTooManyRequests || code == 0 {
		return false
	}
	b.replies.Add(1)
	go func() {
		defer b.replies.Done()
		b.finish(ctx, chat, first.MessageID, code, body)
	}()
	return true
}

// tgClientID is the idempotency key of a message: the album id for an album
// (stable when Telegram re-delivers only part of it), else the message id.
func tgClientID(m *tgMessage) string {
	if g := m.MediaGroupID; tgGroupRe.MatchString(g) {
		return fmt.Sprintf("tg-%d-g%s", m.Chat.ID, g)
	}
	return fmt.Sprintf("tg-%d-%d", m.Chat.ID, m.MessageID)
}

var (
	errTGTooBig = errors.New("file over 20 MB")
	tgGroupRe   = regexp.MustCompile(`^[0-9A-Za-z]{1,40}$`)
)

type tgInput struct {
	clientID, localTime, text string
	audio                     []byte
	images                    [][]byte
}

// post sends the message through the fueld handler, in process. A busy answer
// is retried a few times; the client_id makes every retry safe.
func (b *TelegramBot) post(ctx context.Context, in tgInput) (int, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("client_id", in.clientID)
	_ = mw.WriteField("local_time", in.localTime)
	if in.text != "" {
		_ = mw.WriteField("text", in.text)
	}
	if in.audio != nil {
		w, _ := mw.CreateFormFile("audio", "voice.wav")
		_, _ = w.Write(in.audio)
	}
	for i, im := range in.images {
		w, _ := mw.CreateFormFile("image", fmt.Sprintf("photo-%d.jpg", i+1))
		_, _ = w.Write(im)
	}
	_ = mw.Close()
	var code int
	var body []byte
	for attempt := 0; attempt < 3; attempt++ {
		req := httptest.NewRequest("POST", "/fuel/log", bytes.NewReader(buf.Bytes())).WithContext(WithVia(context.WithoutCancel(ctx), ViaTelegram))
		req.RemoteAddr = "127.0.0.1:0"
		req.Header.Set("Authorization", "Bearer "+b.o.AppToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		b.o.Handler.ServeHTTP(rec, req)
		code, body = rec.Code, rec.Body.Bytes()
		if code != http.StatusServiceUnavailable && code != http.StatusTooManyRequests {
			break
		}
		wait := 5 * time.Second
		if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err == nil && ra > 0 && ra <= 60 {
			wait = time.Duration(ra) * time.Second
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case <-time.After(wait):
		}
	}
	return code, body
}

// finish waits for the answer of a pending turn and sends it.
func (b *TelegramBot) finish(ctx context.Context, chat, msgID int64, code int, body []byte) {
	if code != http.StatusOK && code != http.StatusAccepted {
		b.send(ctx, chat, msgID, "Not logged: "+apiErrText(code, body))
		return
	}
	var r struct {
		Status  string  `json:"status"`
		EntryID string  `json:"entry_id"`
		Blocks  []Block `json:"blocks"`
	}
	if json.Unmarshal(body, &r) != nil {
		b.send(ctx, chat, msgID, "Fuel answered something I cannot read. Check the app.")
		return
	}
	deadline := time.Now().Add(b.o.EntryWait)
	lastTyping := time.Time{}
	for r.Status == StatusPending && r.EntryID != "" {
		if time.Now().After(deadline) {
			b.send(ctx, chat, msgID, "Fuel is still working on this. The answer will be in the app.")
			return
		}
		if time.Since(lastTyping) > 4*time.Second {
			_ = b.call(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
			lastTyping = time.Now()
		}
		select {
		case <-ctx.Done():
			// fueld stops: the turn ends without an answer here.
			b.send(ctx, chat, msgID, "Fuel restarted before the answer came. Check the app or ask again.")
			return
		case <-time.After(1500 * time.Millisecond):
		}
		req := httptest.NewRequest("GET", "/fuel/entry/"+url.PathEscape(r.EntryID), nil).WithContext(ctx)
		req.RemoteAddr = "127.0.0.1:0"
		req.Header.Set("Authorization", "Bearer "+b.o.AppToken)
		rec := httptest.NewRecorder()
		b.o.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
			continue // busy or starting: ask again
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
	}
	b.send(ctx, chat, msgID, renderTelegram(r.Blocks))
}

// renderTelegram turns reply blocks into plain text: the text blocks, then one
// totals line from the macros widget.
func renderTelegram(blocks []Block) string {
	var parts []string
	for _, bl := range blocks {
		switch {
		case bl.Type == "text" && strings.TrimSpace(bl.Text) != "":
			parts = append(parts, strings.TrimSpace(bl.Text))
		case bl.Type == "widget" && bl.Widget == "macros_today":
			if l := totalsLine(bl.Data); l != "" {
				parts = append(parts, l)
			}
		}
	}
	if len(parts) == 0 {
		return "Done. Details are in the app."
	}
	return strings.Join(parts, "\n\n")
}

func totalsLine(data any) string {
	raw, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	var d struct {
		Macros []struct {
			Key      string   `json:"key"`
			Label    string   `json:"label"`
			Unit     string   `json:"unit"`
			Consumed float64  `json:"consumed"`
			Target   *float64 `json:"target"`
		} `json:"macros"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return ""
	}
	order := map[string]int{"kcal": 0, "protein_g": 1, "sat_fat_g": 2, "fiber_g": 3}
	cells := make([]string, len(order))
	for _, m := range d.Macros {
		i, ok := order[m.Key]
		if !ok || m.Target == nil {
			continue
		}
		label := strings.ToLower(m.Label)
		if m.Key == "kcal" {
			label = "energy"
		}
		cells[i] = fmt.Sprintf("%s %s of %s %s", label, fmtNum(tgRound(m.Consumed)), fmtNum(*m.Target), m.Unit)
	}
	var out []string
	for _, c := range cells {
		if c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "Today: " + strings.Join(out, ", ")
}

// tgRound shows 478.7 kcal as 479 and 7.46 g as 7.5.
func tgRound(f float64) float64 {
	if f >= 100 {
		return math.Round(f)
	}
	return math.Round(f*10) / 10
}

func apiErrText(code int, body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return fmt.Sprintf("HTTP %d", code)
}

func shortErr(err error) string {
	var te *tgError
	if errors.As(err, &te) {
		return te.desc
	}
	s := err.Error()
	if i := strings.Index(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return clipRunes(s, 80)
}

func (b *TelegramBot) send(ctx context.Context, chat, replyTo int64, text string) {
	if r := []rune(text); len(r) > tgMaxText {
		text = string(r[:tgMaxText]) + " ..."
	}
	p := map[string]any{"chat_id": chat, "text": text, "link_preview_options": map[string]any{"is_disabled": true}}
	if replyTo != 0 {
		p["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	// At most about 40 s, also while fueld stops: a reply is worth a short
	// wait, not a stuck shutdown.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 40*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		err := b.call(sctx, "sendMessage", p, nil)
		if err == nil {
			return
		}
		log.Printf("fuel: %v", err)
		wait := 3 * time.Second
		var te *tgError
		if errors.As(err, &te) {
			if te.code == 400 || te.code == 403 {
				return // not retryable
			}
			if te.retryAfter > 0 && te.retryAfter <= 10 {
				wait = time.Duration(te.retryAfter) * time.Second
			}
		}
		select {
		case <-sctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// download fetches a file the bot received (at most 20 MB, the Bot API limit).
func (b *TelegramBot) download(ctx context.Context, fileID string, size int64) ([]byte, error) {
	if size > tgMaxFile {
		return nil, errTGTooBig
	}
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := b.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	if f.FilePath == "" {
		return nil, errors.New("no file path")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", b.o.API+"/file/bot"+b.o.Token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, errors.New("bad file path")
	}
	resp, err := b.o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %s", errClass(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tgMaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("download: %s", errClass(err))
	}
	if len(raw) > tgMaxFile {
		return nil, errTGTooBig
	}
	return raw, nil
}

// oggToWAV converts a voice note to 16 kHz mono WAV with ffmpeg (fueld's
// audio check takes m4a or WAV only). The output goes to a file in dir: on a
// pipe ffmpeg cannot write the RIFF length, and fueld rejects such a file.
func oggToWAV(ctx context.Context, dir string, in []byte) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("tmp dir")
	}
	f, err := os.CreateTemp(dir, "tg-voice-*.wav")
	if err != nil {
		return nil, errors.New("tmp file")
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg", "-nostdin", "-y", "-loglevel", "error", "-i", "pipe:0", "-t", strconv.Itoa(tgMaxVoiceSecs),
		"-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-map_metadata", "-1", "-f", "wav", path)
	cmd.Stdin = bytes.NewReader(in)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %s", clipRunes(strings.TrimSpace(errb.String()), 120))
	}
	out, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("tmp read")
	}
	return out, nil
}
