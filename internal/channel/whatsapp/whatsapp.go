// Package whatsapp is the WhatsApp ChannelAdapter. It does NOT open its own
// WhatsApp connection: it drives the ALREADY-PAIRED wacli session (whatsmeow,
// store at ~/.wacli) as a serialized subprocess.
//
// Why a subprocess and not a linked library:
//
//  1. Joe's WhatsApp account already has exactly one linked device, owned by
//     wacli. A second whatsmeow client on the same device credentials is not a
//     second reader, it is a takeover: WhatsApp answers with connectionReplaced
//     (440) and the two clients knock each other offline in a loop. Reusing the
//     one authenticated session is the only safe option, and it needs no QR
//     pairing.
//  2. agentd is a pure-stdlib single static binary (design 3.11). Linking
//     go.mau.fi/whatsmeow would drag in protobuf, libsignal and a cgo SQLite
//     driver, which is a much larger decision than one channel.
//
// wacli guards that single session with an EXCLUSIVE store lock, which shapes
// this adapter: exactly one wacli invocation may run at a time, so every call
// goes through a single mutex plus --lock-wait. That is also why inbound is a
// cursor poll of wacli's local DB (a READ, which does not contend) rather than
// `wacli sync --follow --webhook`: follow-mode holds the lock and the WhatsApp
// connection for its whole lifetime, which would starve outbound sends.
//
// Like every adapter here, this one does ACQUISITION only: it hands media bytes
// to media.Ingest and puts raw text in InboundMsg.Text. It MUST NOT pre-bake
// marker text; session.RenderInbound owns that.
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/media"
)

const (
	defaultBin      = "wacli"
	defaultPoll     = 20 * time.Second
	defaultLockWait = 90 * time.Second
	// listLimit caps one poll batch. wacli returns newest-first without --asc;
	// we always pass --asc so the batch is chronological.
	listLimit = 50
	// syncIdleExit is how long `wacli sync --once` waits for quiet before
	// returning. Short, because the poll loop comes back around anyway.
	syncIdleExit = "8s"
)

// runner executes one wacli invocation. Injected so tests never exec anything.
type runner func(ctx context.Context, args []string) ([]byte, error)

// Adapter is the WhatsApp ChannelAdapter.
type Adapter struct {
	bin   string // wacli binary path
	store string // wacli store dir ("" = wacli default, ~/.wacli)
	allow map[string]bool
	media *media.Service // nil = media handling disabled (polite decline)

	poll time.Duration
	// ownSync makes the adapter run `wacli sync --once` itself each tick. Off by
	// default so it composes with an existing external syncer (clawd's
	// monitor-whatsapp launchd job already runs one every 60s). Turning it on
	// REQUIRES removing that other syncer: two sync processes fight the lock.
	ownSync bool

	inbound chan channel.InboundMsg

	// cursor is the timestamp of the newest message already emitted. Poll asks
	// wacli for --after cursor. seen dedups the boundary second, since --after
	// is inclusive-ish at one-second resolution.
	cursor time.Time
	seen   map[string]bool

	// exec serializes every wacli invocation: wacli takes an EXCLUSIVE store
	// lock, so concurrent calls would just fail each other.
	exec sync.Mutex
	run  runner

	reactions bool
	reactOnce sync.Once
	reactQ    chan reactReq
}

type reactReq struct {
	chatID string
	msgID  string
	sender string
	emoji  string
}

// New builds a WhatsApp adapter. allow is the allowlist, server-enforced; each
// entry may be a bare number ("41791234567"), a full JID
// ("41791234567@s.whatsapp.net"), a @lid JID, or a group JID ("...@g.us").
func New(bin string, allow []string) *Adapter {
	if bin == "" {
		bin = defaultBin
	}
	m := make(map[string]bool, len(allow)*2)
	for _, a := range allow {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		m[a] = true
		m[bare(a)] = true // accept the bare form of a full JID too
	}
	a := &Adapter{
		bin:       bin,
		allow:     m,
		poll:      defaultPoll,
		inbound:   make(chan channel.InboundMsg, 64),
		seen:      map[string]bool{},
		reactions: true,
	}
	a.run = a.execWacli
	return a
}

// WithMedia wires the core media service; the adapter then downloads voice
// notes / images / documents and hands them to media.Ingest. Without it, media
// messages get a polite decline.
func (a *Adapter) WithMedia(svc *media.Service) *Adapter { a.media = svc; return a }

// WithReactions toggles emoji progress feedback (config key: reactions).
func (a *Adapter) WithReactions(on bool) *Adapter { a.reactions = on; return a }

// WithStore points wacli at a specific store directory (config key: store).
func (a *Adapter) WithStore(dir string) *Adapter { a.store = dir; return a }

// WithPoll sets the inbound poll interval (config key: poll).
func (a *Adapter) WithPoll(d time.Duration) *Adapter {
	if d > 0 {
		a.poll = d
	}
	return a
}

// WithOwnSync makes this adapter responsible for refreshing wacli's local DB
// (config key: sync). Only enable when nothing else runs `wacli sync`.
func (a *Adapter) WithOwnSync(on bool) *Adapter { a.ownSync = on; return a }

func (a *Adapter) Name() string        { return "whatsapp" }
func (a *Adapter) SupportsMedia() bool { return false } // send-media stubbed, mirroring telegram

func (a *Adapter) Inbound() <-chan channel.InboundMsg { return a.inbound }

// allowed reports whether a chat/sender id passes the allowlist. Both the raw
// JID and its bare form are checked: WhatsApp delivers replies on a @lid JID
// that differs from the phone JID, so matching only one form silently drops
// messages.
func (a *Adapter) allowed(id string) bool {
	if id == "" {
		return false
	}
	return a.allow[id] || a.allow[bare(id)]
}

// bare strips the JID suffix and any device/agent suffix: "4179...:12@s.whatsapp.net" -> "4179...".
func bare(jid string) string {
	if i := strings.IndexByte(jid, '@'); i >= 0 {
		jid = jid[:i]
	}
	if i := strings.IndexByte(jid, ':'); i >= 0 {
		jid = jid[:i]
	}
	return jid
}

func isGroup(jid string) bool { return strings.HasSuffix(jid, "@g.us") }

// Start launches the poll loop in a goroutine and returns immediately.
func (a *Adapter) Start(ctx context.Context) error {
	if a.bin == "" {
		return fmt.Errorf("whatsapp: empty wacli binary path")
	}
	if len(a.allow) == 0 {
		// WhatsApp is a wide-open inbound surface. An empty allowlist here is
		// almost certainly a config mistake, and "allow none" is the safe read,
		// but refuse to start rather than look healthy while dropping everything.
		return fmt.Errorf("whatsapp: empty allow list; refusing to start (set allow = [...] on the [[channel]] block)")
	}
	// Start from now: never replay history into a fresh session.
	a.cursor = time.Now().UTC()
	go a.pollLoop(ctx)
	return nil
}

// pollLoop is the single supervised poll with backoff, mirroring the telegram
// adapter's long-poll loop.
func (a *Adapter) pollLoop(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := a.pollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("whatsapp: poll error: %v (backoff %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 2*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.poll):
		}
	}
}

// waMessage is one row of `wacli messages list --json`.
type waMessage struct {
	ChatJID      string `json:"ChatJID"`
	ChatName     string `json:"ChatName"`
	MsgID        string `json:"MsgID"`
	SenderJID    string `json:"SenderJID"`
	SenderName   string `json:"SenderName"`
	Timestamp    string `json:"Timestamp"`
	FromMe       bool   `json:"FromMe"`
	Text         string `json:"Text"`
	MediaType    string `json:"MediaType"`
	MediaCaption string `json:"MediaCaption"`
	Filename     string `json:"Filename"`
	MimeType     string `json:"MimeType"`
	LocalPath    string `json:"LocalPath"`
	ReactionToID string `json:"ReactionToID"`
	Revoked      bool   `json:"Revoked"`
	DeletedForMe bool   `json:"DeletedForMe"`
	// NOTE: `wacli messages list --json` exposes no quoted/reply-to field
	// (verified against wacli 0.11.1: the row keys are exactly the fields
	// above). InboundMsg.ReplyTo therefore stays empty on this channel, so a
	// WhatsApp quote-reply arrives without its parent. Lifting that needs a
	// wacli change; do not fake it here.
}

type waListResp struct {
	Success bool `json:"success"`
	Data    struct {
		Messages []waMessage `json:"messages"`
	} `json:"data"`
	Error any `json:"error"`
}

// pollOnce refreshes the local DB (when we own the sync) and drains any new
// inbound messages past the cursor.
func (a *Adapter) pollOnce(ctx context.Context) error {
	if a.ownSync {
		if _, err := a.wacli(ctx, "sync", "--once", "--idle-exit", syncIdleExit); err != nil {
			// A failed refresh is not fatal: the DB may still hold new messages
			// put there by another syncer. Log and read anyway.
			log.Printf("whatsapp: sync refresh failed: %v", err)
		}
	}
	out, err := a.wacli(ctx, "messages", "list", "--json", "--from-them", "--asc",
		"--after", a.cursor.Format(time.RFC3339), "--limit", fmt.Sprint(listLimit))
	if err != nil {
		return err
	}
	var resp waListResp
	if err := json.Unmarshal(out, &resp); err != nil {
		return fmt.Errorf("whatsapp: parse messages list: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("whatsapp: messages list reported failure: %v", resp.Error)
	}
	for _, m := range resp.Data.Messages {
		a.handleMessage(ctx, m)
	}
	a.pruneSeen()
	return nil
}

// handleMessage enforces the allowlist and dispatches one message. Media
// handling runs in a GOROUTINE so a slow download or Whisper call never stalls
// the poll loop.
func (a *Adapter) handleMessage(ctx context.Context, m waMessage) {
	if m.FromMe || m.Revoked || m.DeletedForMe || m.MsgID == "" {
		return
	}
	// Inbound reactions are metadata, not turns.
	if m.ReactionToID != "" {
		return
	}
	if a.seen[m.MsgID] {
		return
	}

	ts, err := time.Parse(time.RFC3339, m.Timestamp)
	if err != nil {
		log.Printf("whatsapp: unparsable timestamp %q on %s; skipping", m.Timestamp, m.MsgID)
		return
	}
	// Advance the cursor even for messages we drop, so a non-allowlisted
	// chatterbox cannot pin the cursor and make us re-read forever.
	if ts.After(a.cursor) {
		a.cursor = ts
	}
	a.seen[m.MsgID] = true

	// Allowlist. For a group the CHAT is the gate (a group is allowed as a
	// whole); for a DM either form of the peer JID is the gate.
	gate := m.ChatJID
	if !a.allowed(gate) {
		log.Printf("whatsapp: dropping message from non-allowlisted chat %s (sender %s)", m.ChatJID, m.SenderJID)
		return
	}

	if m.MediaType != "" {
		go a.processMedia(context.WithoutCancel(ctx), m)
		return
	}
	if strings.TrimSpace(m.Text) == "" {
		return
	}
	a.emitInbound(m, m.Text, nil)
}

// emitInbound builds and queues the normalized InboundMsg.
//
// UserID is the CHAT jid (the allowlist + session key), Sender is the
// PARTICIPANT jid, which in a group is a different person from the chat. That
// split is what lets a group thread keep one session while still attributing
// each turn.
func (a *Adapter) emitInbound(m waMessage, text string, art *media.Artifact) {
	ts, _ := time.Parse(time.RFC3339, m.Timestamp)
	msg := channel.InboundMsg{
		Channel: "whatsapp",
		UserID:  m.ChatJID,
		MsgID:   m.MsgID,
		Sender:  m.SenderJID,
		TS:      ts.Unix(),
		Text:    text,
	}
	if art != nil {
		msg.Media = []media.Artifact{*art}
	}
	select {
	case a.inbound <- msg:
	default:
		log.Printf("whatsapp: inbound buffer full, dropping message from %s", m.ChatJID)
	}
}

// processMedia downloads the attachment via wacli and hands the bytes to
// media.Ingest. ACQUISITION ONLY: the caption becomes Text, and no marker text
// is baked here (session.RenderInbound owns that).
func (a *Adapter) processMedia(ctx context.Context, m waMessage) {
	caption := m.MediaCaption
	if caption == "" {
		caption = m.Text
	}
	if a.media == nil {
		a.replyText(m.ChatJID, "Sorry, media handling is not enabled on this bot yet.")
		return
	}
	// The MediaType values wacli 0.11.1 actually emits are: image, audio,
	// document, sticker, gif, video (verified against the live store). Voice
	// notes arrive as "audio". The rest get a polite decline, never silence.
	switch m.MediaType {
	case "image", "audio", "document":
	default:
		a.replyText(m.ChatJID, "Sorry, I can only handle text, voice messages, photos, and documents right now.")
		return
	}

	path, err := a.downloadMedia(ctx, m)
	if err != nil {
		log.Printf("whatsapp: media download for %s failed: %v", m.ChatJID, err)
		a.replyText(m.ChatJID, "Sorry, I couldn't download that file from WhatsApp. Please try again.")
		return
	}
	defer os.Remove(path)

	f, err := os.Open(path)
	if err != nil {
		log.Printf("whatsapp: opening downloaded media %s failed: %v", path, err)
		a.replyText(m.ChatJID, "Sorry, something went wrong handling that file.")
		return
	}
	defer f.Close()

	cap := a.media.MaxBytes
	if cap <= 0 {
		cap = 20 << 20
	}
	name := m.Filename
	if name == "" {
		name = filepath.Base(path)
	}
	art, err := a.media.Ingest(ctx, media.Request{
		Reader:    io.LimitReader(f, cap+1),
		Filename:  name,
		Mime:      m.MimeType,
		Source:    "whatsapp",
		ChatLabel: m.ChatJID,
	})
	if err != nil {
		log.Printf("whatsapp: media ingest for %s failed: %v", m.ChatJID, err)
		switch {
		case errors.Is(err, media.ErrTooLarge):
			a.replyText(m.ChatJID, fmt.Sprintf("Sorry, that file is too large for me (limit %d MB).", cap>>20))
		case errors.Is(err, media.ErrUnsupported):
			a.replyText(m.ChatJID, "Sorry, I can't process that file type.")
		case errors.Is(err, media.ErrTranscribe):
			a.replyText(m.ChatJID, "Sorry, I couldn't transcribe that voice message. I kept the audio; please try again or type it out.")
		default:
			a.replyText(m.ChatJID, "Sorry, something went wrong handling that file.")
		}
		return
	}
	a.emitInbound(m, caption, &art)
}

// downloadMedia runs `wacli media download` into a temp dir and returns the
// resulting file path.
func (a *Adapter) downloadMedia(ctx context.Context, m waMessage) (string, error) {
	dir, err := os.MkdirTemp("", "agentd-wa-*")
	if err != nil {
		return "", err
	}
	if _, err := a.wacli(ctx, "media", "download", "--chat", m.ChatJID, "--id", m.MsgID, "--output", dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		os.RemoveAll(dir)
		return "", fmt.Errorf("whatsapp: media download produced no file for %s", m.MsgID)
	}
	return filepath.Join(dir, entries[0].Name()), nil
}

// waSendResp is the shape of `wacli send text --json`.
type waSendResp struct {
	Success bool `json:"success"`
	Data    struct {
		MsgID     string `json:"MsgID"`
		MessageID string `json:"message_id"`
		ID        string `json:"id"`
	} `json:"data"`
}

// Send delivers a text message and returns the provider message id.
func (a *Adapter) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	if len(m.Media) > 0 {
		// Media path is intentionally stubbed, matching the telegram adapter.
		return channel.SendReceipt{}, fmt.Errorf("whatsapp: media send not implemented (stub)")
	}
	if m.ChatID == "" {
		return channel.SendReceipt{}, fmt.Errorf("whatsapp: send with empty chat id")
	}
	// Never send anywhere the allowlist does not cover, even if some other part
	// of the system asks us to.
	if !a.allowed(m.ChatID) {
		return channel.SendReceipt{}, fmt.Errorf("whatsapp: refusing to send to non-allowlisted chat %s", m.ChatID)
	}
	out, err := a.wacli(ctx, "send", "text", "--to", m.ChatID, "--message", m.Text, "--json")
	if err != nil {
		return channel.SendReceipt{}, err
	}
	var resp waSendResp
	if err := json.Unmarshal(out, &resp); err != nil {
		return channel.SendReceipt{}, fmt.Errorf("whatsapp: parse send response: %w", err)
	}
	id := resp.Data.MsgID
	if id == "" {
		id = resp.Data.MessageID
	}
	if id == "" {
		id = resp.Data.ID
	}
	if id == "" {
		return channel.SendReceipt{}, fmt.Errorf("whatsapp: send returned no message id")
	}
	return channel.SendReceipt{ID: id}, nil
}

// replyText sends a short best-effort service reply (declines, failures).
func (a *Adapter) replyText(chatID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := a.Send(ctx, channel.OutboundMsg{ChatID: chatID, Text: text}); err != nil {
		log.Printf("whatsapp: service reply to %s failed: %v", chatID, err)
	}
}

// Ack sets the lifecycle reaction on the user's own message. WhatsApp supports
// exactly one reaction per message per sender, and a new one REPLACES the last,
// so the 👀 -> ⚡ -> 👍 chain reads as one changing marker, same as Telegram.
func (a *Adapter) Ack(chatID, msgID, reaction string) error {
	if !a.reactions || reaction == "" || chatID == "" || msgID == "" {
		return nil
	}
	a.enqueueReaction(chatID, msgID, "", reaction)
	return nil
}

func (a *Adapter) enqueueReaction(chatID, msgID, sender, emoji string) {
	if !a.reactions || emoji == "" || chatID == "" || msgID == "" {
		return
	}
	a.reactOnce.Do(func() {
		a.reactQ = make(chan reactReq, 64)
		go a.reactLoop()
	})
	select {
	case a.reactQ <- reactReq{chatID: chatID, msgID: msgID, sender: sender, emoji: emoji}:
	default:
		log.Printf("whatsapp: reaction queue full, dropping %q for %s/%s", emoji, chatID, msgID)
	}
}

// reactLoop serializes reaction calls so the chain lands in lifecycle order and
// no caller ever waits on wacli.
func (a *Adapter) reactLoop() {
	for r := range a.reactQ {
		if err := a.setReaction(r); err != nil {
			log.Printf("whatsapp: reaction %q on %s/%s failed: %v", r.emoji, r.chatID, r.msgID, err)
		}
	}
}

func (a *Adapter) setReaction(r reactReq) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"send", "react", "--to", r.chatID, "--id", r.msgID, "--reaction", r.emoji}
	// wacli needs the original sender JID to address a reaction inside a group.
	if isGroup(r.chatID) && r.sender != "" {
		args = append(args, "--sender", r.sender)
	}
	_, err := a.wacli(ctx, args...)
	return err
}

// wacli runs one wacli invocation under the store-lock mutex.
func (a *Adapter) wacli(ctx context.Context, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+4)
	full = append(full, args...)
	full = append(full, "--lock-wait", lockWaitFor(ctx).String())
	if a.store != "" {
		full = append(full, "--store", a.store)
	}
	a.exec.Lock()
	defer a.exec.Unlock()
	return a.run(ctx, full)
}

// lockWaitFor sizes --lock-wait to the caller's remaining budget. Without this
// a caller on a short deadline (session.RouteInbound sends its failure notice
// on a DETACHED 20s context) would have wacli killed by CommandContext while it
// was still politely waiting for the store lock, and the user would get the
// silence that notice exists to prevent. Leave headroom so wacli reports a
// lock-timeout itself rather than dying mid-wait.
func lockWaitFor(ctx context.Context) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return defaultLockWait
	}
	budget := time.Duration(float64(time.Until(dl)) * 0.8)
	if budget < time.Second {
		return time.Second
	}
	if budget > defaultLockWait {
		return defaultLockWait
	}
	return budget
}

func (a *Adapter) execWacli(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, a.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("whatsapp: wacli %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// pruneSeen keeps the dedup set from growing without bound. Only ids at or
// after the cursor can still come back in a poll batch.
func (a *Adapter) pruneSeen() {
	if len(a.seen) <= 4*listLimit {
		return
	}
	a.seen = map[string]bool{}
}
