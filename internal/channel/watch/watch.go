package watch

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/notify"
)

// DefaultUserID keys the wrist conversation in session.RouteInbound's per-user
// map when the channel runs its own session. With share_session the wiring
// overrides it with the Telegram chat id so both surfaces drive ONE session.
const DefaultUserID = "watch"

// Adapter is the wrist ChannelAdapter + notify.Sink + its HTTP surface.
type Adapter struct {
	title   string
	token   string
	userID  string
	store   *Store
	media   *media.Service
	inbound chan channel.InboundMsg
	mirror  func(text string)
	maxWait time.Duration
	now     func() time.Time
}

// New builds a watch adapter gated by its own bearer token (NOT the API
// bearer: this surface is meant to be reachable through a public HTTPS proxy,
// so a leaked token must not unlock the code-executing API).
func New(token string, store *Store) *Adapter {
	return &Adapter{
		title:   "agentd",
		token:   token,
		userID:  DefaultUserID,
		store:   store,
		inbound: make(chan channel.InboundMsg, 64),
		maxWait: 30 * time.Second,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithMedia enables voice uploads (transcribed by the core media service).
func (a *Adapter) WithMedia(svc *media.Service) *Adapter { a.media = svc; return a }

// WithUserID sets the session key inbound messages route under.
func (a *Adapter) WithUserID(id string) *Adapter {
	if id != "" {
		a.userID = id
	}
	return a
}

// WithMirror installs a best-effort echo of every inbound transcript and
// outbound reply to another channel (Joe's Telegram chat), so the phone
// keeps the full record and pushes replies while the watch is asleep.
func (a *Adapter) WithMirror(fn func(text string)) *Adapter { a.mirror = fn; return a }

// WithTitle labels the channel in /watch/ping.
func (a *Adapter) WithTitle(t string) *Adapter {
	if t != "" {
		a.title = t
	}
	return a
}

// Store exposes the conversation store (tests, health).
func (a *Adapter) Store() *Store { return a.store }

// UserID is the session key the channel routes under (exported for wiring).
func (a *Adapter) UserID() string { return a.userID }

// ---- channel.Adapter ----

func (a *Adapter) Name() string                       { return "watch" }
func (a *Adapter) Start(ctx context.Context) error    { return nil }
func (a *Adapter) Inbound() <-chan channel.InboundMsg { return a.inbound }
func (a *Adapter) SupportsMedia() bool                { return false }

// Send stores the turn's reply (or its failure notice) as an agent message
// attributed to the user message that started the turn.
func (a *Adapter) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	msg := Message{Role: RoleAgent, Kind: KindReply, Text: m.Text}
	if u, ok := a.store.Attribute(); ok {
		msg.ReplyTo = u.ID
		if u.Status == StatusFailed {
			msg.Kind = KindFailure
		}
	}
	stored := a.store.Append(msg)
	a.echo("⌚ " + m.Text)
	return channel.SendReceipt{ID: stored.ID}, nil
}

// Ack maps the lifecycle reaction chain onto the user message's status. This
// is what makes the wrist show queued / working / done / failed without a
// second signalling path.
func (a *Adapter) Ack(chatID, msgID, reaction string) error {
	status := ""
	switch reaction {
	case channel.ReactionWorking:
		status = StatusWorking
	case channel.ReactionNeedsInput:
		status = StatusNeedsInput
	case channel.ReactionDone:
		status = StatusDone
	case channel.ReactionError:
		status = StatusFailed
	default:
		return nil
	}
	a.store.Update(msgID, func(m *Message) bool {
		if m.Role != RoleUser || m.Status == status {
			return false
		}
		if Terminal(m.Status) && !Terminal(status) {
			return false // never regress a finished turn
		}
		m.Status = status
		return true
	})
	return nil
}

// ---- notify.Sink ----

// Notify records a system notification (job issues/results) as a notice
// entry so the wrist sees it on its next fetch.
func (a *Adapter) Notify(n notify.Notification) error {
	text := n.Text
	if n.Source != "" {
		text = n.Source + ": " + text
	}
	a.store.Append(Message{Role: RoleSystem, Kind: KindNotice, Text: text, TS: n.Ts})
	return nil
}

func (a *Adapter) echo(text string) {
	if a.mirror == nil {
		return
	}
	go a.mirror(text)
}

// ---- HTTP ----

// Handler serves the wrist API under /watch/. It carries its OWN token gate
// and is mounted on the api server as a PUBLIC route (api.MountPublic), so the
// API bearer is neither required nor accepted here.
//
//	GET  /watch/ping                          {ok, title, pending}
//	GET  /watch/messages?after=N&limit=M&wait=S
//	POST /watch/messages   JSON {text}  |  multipart file (+text)  -> 202 {message}
func (a *Adapter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/watch/ping", a.handlePing)
	mux.HandleFunc("/watch/messages", a.handleMessages)
	return a.auth(mux)
}

func (a *Adapter) authed(r *http.Request) bool {
	if a.token == "" {
		return false // an unset token means CLOSED, never open
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

func (a *Adapter) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *Adapter) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "title": a.title, "pending": a.store.Pending(), "server_time": a.now(),
	})
}

func (a *Adapter) handleMessages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleFetch(w, r)
	case http.MethodPost:
		a.handlePost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFetch answers "everything after seq N", optionally long-polling up to
// wait seconds (capped) when nothing new is there yet.
func (a *Adapter) handleFetch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	waitS, _ := strconv.Atoi(q.Get("wait"))
	if waitS > 0 && after > 0 {
		d := time.Duration(waitS) * time.Second
		if d > a.maxWait {
			d = a.maxWait
		}
		a.store.Wait(r.Context(), after, d)
	}
	msgs, latest, reset := a.store.Since(after, limit)
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":    msgs,
		"latest_seq":  latest,
		"reset":       reset,
		"pending":     a.store.Pending(),
		"server_time": a.now(),
	})
}

// handlePost accepts one delegation: JSON {"text"} or multipart with a "file"
// (voice, transcribed synchronously so the receipt already carries the text)
// and an optional "text" field. It answers 202 with the stored user message
// as soon as the turn is QUEUED; the reply arrives later through fetch.
func (a *Adapter) handlePost(w http.ResponseWriter, r *http.Request) {
	var (
		text string
		art  *media.Artifact
	)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "multipart/"):
		cap := int64(20 << 20)
		if a.media != nil && a.media.MaxBytes > 0 {
			cap = a.media.MaxBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, cap+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, "bad multipart body: "+err.Error())
			return
		}
		text = strings.TrimSpace(r.FormValue("text"))
		file, hdr, err := r.FormFile("file")
		if err == nil {
			defer file.Close()
			if a.media == nil {
				writeErr(w, http.StatusServiceUnavailable, "media not configured")
				return
			}
			dur, _ := strconv.Atoi(r.FormValue("duration_s"))
			got, ierr := a.media.Ingest(r.Context(), media.Request{
				Reader:    io.LimitReader(file, cap+1),
				Filename:  hdr.Filename,
				Mime:      hdr.Header.Get("Content-Type"),
				Source:    "watch",
				ChatLabel: a.userID,
				DurationS: dur,
			})
			if ierr != nil {
				switch {
				case errors.Is(ierr, media.ErrTooLarge):
					writeErr(w, http.StatusRequestEntityTooLarge, ierr.Error())
				case errors.Is(ierr, media.ErrUnsupported):
					writeErr(w, http.StatusUnsupportedMediaType, ierr.Error())
				case errors.Is(ierr, media.ErrTranscribe):
					writeErr(w, http.StatusBadGateway, "transcription failed; the audio was kept, please try again")
				default:
					writeErr(w, http.StatusInternalServerError, ierr.Error())
				}
				return
			}
			art = &got
		} else if !errors.Is(err, http.ErrMissingFile) {
			writeErr(w, http.StatusBadRequest, "bad file field: "+err.Error())
			return
		}
	default:
		var body struct {
			Text string `json:"text"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		text = strings.TrimSpace(body.Text)
	}
	if text == "" && art == nil {
		writeErr(w, http.StatusBadRequest, "nothing to send: give text or a file")
		return
	}

	// The stored user message is the RECEIPT of the delegation: what was
	// understood (the transcript for voice), shown on the wrist at once.
	msg := Message{Role: RoleUser, Kind: KindText, Text: text, Status: StatusQueued}
	if art != nil {
		msg.DurationS = art.DurationS
		switch art.Kind {
		case media.KindAudio:
			msg.Kind = KindVoice
			msg.Text = joinText(text, art.Transcript)
		case media.KindImage:
			msg.Text = joinText(text, "[photo]")
		default:
			msg.Text = joinText(text, "[file: "+art.Name+"]")
		}
	}
	stored := a.store.Append(msg)

	in := channel.InboundMsg{
		Channel: "watch",
		UserID:  a.userID,
		MsgID:   stored.ID,
		Sender:  "watch",
		TS:      a.now().Unix(),
		Text:    text,
	}
	if art != nil {
		in.Media = []media.Artifact{*art}
	}
	select {
	case a.inbound <- in:
	default:
		a.store.Update(stored.ID, func(m *Message) bool { m.Status = StatusFailed; return true })
		writeErr(w, http.StatusServiceUnavailable, "inbound queue full, try again")
		return
	}
	a.echo("⌚ You: " + stored.Text)
	writeJSON(w, http.StatusAccepted, map[string]any{"message": stored, "pending": a.store.Pending()})
}

func joinText(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}

var _ channel.Adapter = (*Adapter)(nil)
var _ notify.Sink = (*Adapter)(nil)

// String renders a message for logs (never the whole text).
func (m Message) String() string {
	t := m.Text
	if len(t) > 40 {
		t = t[:40] + "…"
	}
	return fmt.Sprintf("%s/%s#%d %q %s", m.Role, m.Kind, m.Seq, t, m.Status)
}
