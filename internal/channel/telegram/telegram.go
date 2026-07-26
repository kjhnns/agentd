// Package telegram is a fresh, in-process Telegram ChannelAdapter (design 3.3).
// It does NOT reuse any of clawd's tg-bridge processes: there is no separate
// daemon, no SSE hub, no MCP proxy child, no enabledPlugins flag. It is a single
// goroutine inside the one server process that long-polls getUpdates with backoff,
// enforces the chat-id allowlist, and sends text via the Bot API sendMessage.
// Media send is stubbed (interface present) for a later pass.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/notify"
)

// botAPIMaxDownload is the Bot API's own getFile ceiling: bots cannot download
// files over 20 MB regardless of our configured cap.
const botAPIMaxDownload = 20 << 20

// Adapter is the Telegram channel adapter.
type Adapter struct {
	token   string
	allow   map[string]bool // chat-id allowlist; empty map means "allow none"
	base    string          // Bot API base (overridable for tests)
	client  *http.Client
	inbound chan channel.InboundMsg
	offset  int64
	media   *media.Service // nil = media handling disabled (polite decline)

	// Reaction feedback (setMessageReaction). Calls are queued onto ONE worker
	// so the chain lands in lifecycle order (👀 then ⚡ then 👍) and no caller
	// ever waits on the Bot API. reactions=false switches the whole thing off.
	reactions bool
	reactOnce sync.Once
	reactQ    chan reactReq
}

// reactReq is one queued setMessageReaction call.
type reactReq struct {
	chatID string
	msgID  int64
	emoji  string
}

// New builds a Telegram adapter. allow is the chat-id allowlist (server-enforced).
func New(token string, allow []string) *Adapter {
	m := make(map[string]bool, len(allow))
	for _, a := range allow {
		m[a] = true
	}
	return &Adapter{
		token:     token,
		allow:     m,
		base:      "https://api.telegram.org",
		client:    &http.Client{Timeout: 65 * time.Second},
		inbound:   make(chan channel.InboundMsg, 64),
		reactions: true, // default on; [[channel]] reactions=false parks it
	}
}

// WithReactions toggles emoji progress feedback (config key: reactions).
func (a *Adapter) WithReactions(on bool) *Adapter {
	a.reactions = on
	return a
}

// WithMedia wires the core media service; the adapter then ACQUIRES voice/
// photo/document payloads (getFile + streamed download) and hands them to
// media.Ingest. Without it, media messages get a polite decline.
func (a *Adapter) WithMedia(svc *media.Service) *Adapter {
	a.media = svc
	return a
}

func (a *Adapter) Name() string        { return "telegram" }
func (a *Adapter) SupportsMedia() bool { return false } // send-media stubbed for now

func (a *Adapter) Inbound() <-chan channel.InboundMsg { return a.inbound }

// allowed reports whether a chat id passes the allowlist.
func (a *Adapter) allowed(chatID string) bool { return a.allow[chatID] }

// Start launches the long-poll loop in a goroutine and returns immediately.
func (a *Adapter) Start(ctx context.Context) error {
	if a.token == "" {
		return fmt.Errorf("telegram: empty token (set env or config)")
	}
	go a.pollLoop(ctx)
	return nil
}

// pollLoop is the single supervised reconnect-with-backoff long-poll (design 3.3:
// the long-poll wedge becomes one in-process loop, not a second watchdog process).
func (a *Adapter) pollLoop(ctx context.Context) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		updates, err := a.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("telegram: getUpdates error: %v (backoff %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			a.handleUpdate(u)
		}
	}
}

// tgUpdate is the subset of a Telegram Update we consume.
type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

// tgMessage is the subset of a Telegram Message we consume: text plus the
// supported media kinds (voice / photo / document with optional caption) and
// presence-only markers for the unsupported kinds we politely decline.
type tgMessage struct {
	MessageID int64 `json:"message_id"`
	Date      int64 `json:"date"` // provider send time, unix seconds
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From *struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Text           string        `json:"text"`
	Caption        string        `json:"caption"`
	Voice          *tgVoice      `json:"voice"`
	Photo          []tgPhotoSize `json:"photo"`
	Document       *tgDocument   `json:"document"`
	Video          *tgFileRef    `json:"video"`
	VideoNote      *tgFileRef    `json:"video_note"`
	Sticker        *tgFileRef    `json:"sticker"`
	Audio          *tgFileRef    `json:"audio"`
	Animation      *tgFileRef    `json:"animation"`
	ReplyToMessage *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_to_message"`
}

type tgVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type tgPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type tgDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// tgFileRef marks the presence of an unsupported attachment kind.
type tgFileRef struct {
	FileID string `json:"file_id"`
}

func (a *Adapter) getUpdates(ctx context.Context) ([]tgUpdate, error) {
	q := url.Values{}
	q.Set("timeout", "50")
	q.Set("offset", strconv.FormatInt(a.offset, 10))
	endpoint := fmt.Sprintf("%s/bot%s/getUpdates?%s", a.base, a.token, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getUpdates HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("getUpdates not ok: %s", string(body))
	}
	return out.Result, nil
}

// handleUpdate advances the offset, enforces the allowlist, and dispatches the
// message. Media handling (getFile + download + ingest + transcription) runs in
// a GOROUTINE so a slow Whisper call never stalls the poll loop.
func (a *Adapter) handleUpdate(u tgUpdate) {
	if u.UpdateID >= a.offset {
		a.offset = u.UpdateID + 1
	}
	if u.Message == nil {
		return
	}
	chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
	if !a.allowed(chatID) {
		log.Printf("telegram: dropping message from non-allowlisted chat %s", chatID)
		return
	}
	m := u.Message
	switch {
	case m.Voice != nil, len(m.Photo) > 0, m.Document != nil:
		// Receipt reaction FIRST: media ingest (download + Whisper) can take
		// many seconds, so the 👀 must not wait on it.
		a.enqueueReaction(chatID, m.MessageID, channel.ReactionReceived)
		go a.processMedia(context.Background(), m, chatID)
	case m.Video != nil, m.VideoNote != nil, m.Sticker != nil, m.Audio != nil, m.Animation != nil:
		// Unsupported kinds get a polite one-line decline, never silence (the
		// old code dropped every non-text message on the floor).
		go a.replyText(chatID, "Sorry, I can only handle text, voice messages, photos, and documents right now.")
	case m.Text != "":
		a.enqueueReaction(chatID, m.MessageID, channel.ReactionReceived)
		a.emitInbound(m, chatID, m.Text, nil)
	}
}

// emitInbound builds and queues the normalized InboundMsg.
func (a *Adapter) emitInbound(m *tgMessage, chatID, text string, art *media.Artifact) {
	msg := channel.InboundMsg{
		Channel: "telegram",
		UserID:  chatID,
		MsgID:   strconv.FormatInt(m.MessageID, 10),
		TS:      m.Date,
		Text:    text,
	}
	if m.From != nil {
		msg.Sender = strconv.FormatInt(m.From.ID, 10)
	}
	if art != nil {
		msg.Media = []media.Artifact{*art}
	}
	if m.ReplyToMessage != nil {
		msg.ReplyTo = strconv.FormatInt(m.ReplyToMessage.MessageID, 10)
	}
	select {
	case a.inbound <- msg:
	default:
		log.Printf("telegram: inbound buffer full, dropping message from %s", chatID)
	}
}

// replyText sends a short best-effort service reply (declines, failures).
func (a *Adapter) replyText(chatID, text string) {
	if _, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: chatID, Text: text}); err != nil {
		log.Printf("telegram: service reply to %s failed: %v", chatID, err)
	}
}

// processMedia is ACQUISITION ONLY (design: media is a core capability;
// channels just fetch bytes): resolve the file reference, stream-download it
// with the size cap, hand it to media.Ingest, and emit the artifact on the
// inbound channel with the caption as the text. Every failure produces a short
// reply to the sender; silence is never an outcome.
func (a *Adapter) processMedia(ctx context.Context, m *tgMessage, chatID string) {
	if a.media == nil {
		a.replyText(chatID, "Sorry, media handling is not enabled on this bot yet.")
		return
	}
	var (
		fileID   string
		filename string
		mimeType string
		declared int64
		duration int
	)
	switch {
	case m.Voice != nil:
		fileID = m.Voice.FileID
		filename = fmt.Sprintf("voice-%d.oga", m.MessageID)
		mimeType = m.Voice.MimeType
		if mimeType == "" {
			mimeType = "audio/ogg"
		}
		declared = m.Voice.FileSize
		duration = m.Voice.Duration
	case len(m.Photo) > 0:
		largest := m.Photo[0]
		for _, p := range m.Photo[1:] {
			if p.FileSize > largest.FileSize ||
				(p.FileSize == largest.FileSize && p.Width*p.Height > largest.Width*largest.Height) {
				largest = p
			}
		}
		fileID = largest.FileID
		filename = fmt.Sprintf("photo-%d.jpg", m.MessageID)
		mimeType = "image/jpeg"
		declared = largest.FileSize
	case m.Document != nil:
		fileID = m.Document.FileID
		filename = m.Document.FileName
		if filename == "" {
			filename = fmt.Sprintf("document-%d", m.MessageID)
		}
		mimeType = m.Document.MimeType
		declared = m.Document.FileSize
	default:
		return
	}

	cap := a.media.MaxBytes
	if cap <= 0 || cap > botAPIMaxDownload {
		cap = botAPIMaxDownload // Bot API hard limit
	}
	if declared > cap {
		a.replyText(chatID, fmt.Sprintf("Sorry, that file is too large for me (limit %d MB).", cap>>20))
		return
	}

	body, err := a.downloadFile(ctx, fileID)
	if err != nil {
		log.Printf("telegram: media download for chat %s failed: %v", chatID, err)
		a.replyText(chatID, "Sorry, I couldn't download that file from Telegram. Please try again.")
		return
	}
	defer body.Close()

	art, err := a.media.Ingest(ctx, media.Request{
		Reader:    io.LimitReader(body, cap+1),
		Filename:  filename,
		Mime:      mimeType,
		Source:    "telegram",
		ChatLabel: chatID,
		DurationS: duration,
	})
	if err != nil {
		log.Printf("telegram: media ingest for chat %s failed: %v", chatID, err)
		switch {
		case errors.Is(err, media.ErrTooLarge):
			a.replyText(chatID, fmt.Sprintf("Sorry, that file is too large for me (limit %d MB).", cap>>20))
		case errors.Is(err, media.ErrUnsupported):
			a.replyText(chatID, "Sorry, I can't process that file type.")
		case errors.Is(err, media.ErrTranscribe):
			a.replyText(chatID, "Sorry, I couldn't transcribe that voice message. I kept the audio; please try again or type it out.")
		default:
			a.replyText(chatID, "Sorry, something went wrong handling that file.")
		}
		return
	}
	a.emitInbound(m, chatID, m.Caption, &art)
}

// downloadFile resolves a file_id via getFile and opens a streamed download.
func (a *Adapter) downloadFile(ctx context.Context, fileID string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("file_id", fileID)
	endpoint := fmt.Sprintf("%s/bot%s/getFile?%s", a.base, a.token, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getFile HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if !out.OK || out.Result.FilePath == "" {
		return nil, fmt.Errorf("getFile not ok: %s", string(body))
	}
	dl := fmt.Sprintf("%s/file/bot%s/%s", a.base, a.token, out.Result.FilePath)
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, dl, nil)
	if err != nil {
		return nil, err
	}
	dresp, err := a.client.Do(dreq)
	if err != nil {
		return nil, err
	}
	if dresp.StatusCode != http.StatusOK {
		dresp.Body.Close()
		return nil, fmt.Errorf("file download HTTP %d", dresp.StatusCode)
	}
	return dresp.Body, nil
}

// Send posts text via the Bot API sendMessage and returns the message id.
func (a *Adapter) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	if len(m.Media) > 0 {
		// Media path is intentionally stubbed in this scaffold.
		return channel.SendReceipt{}, fmt.Errorf("telegram: media send not implemented (stub)")
	}
	payload, _ := json.Marshal(map[string]any{
		"chat_id": m.ChatID,
		"text":    m.Text,
	})
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", a.base, a.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return channel.SendReceipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return channel.SendReceipt{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return channel.SendReceipt{}, fmt.Errorf("sendMessage HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return channel.SendReceipt{}, err
	}
	if !out.OK {
		return channel.SendReceipt{}, fmt.Errorf("sendMessage not ok: %s", string(body))
	}
	return channel.SendReceipt{ID: strconv.FormatInt(out.Result.MessageID, 10)}, nil
}

// Ack sets an emoji reaction on an inbound message (Bot API setMessageReaction).
// It ENQUEUES and returns immediately: a reaction is progress feedback, never a
// reason for a turn to stall or fail. Failures are logged by the worker.
//
// Telegram only accepts reactions from its own fixed whitelist (👍 👎 ❤ 🔥 🎉 👀
// ⚡ 🤔 😱 ... ; ✅ and ⚠️ are NOT on it, they come back REACTION_INVALID), so the
// lifecycle glyphs in internal/channel are all picked from that set and a
// rejection is logged and swallowed rather than raised.
func (a *Adapter) Ack(chatID, msgID, reaction string) error {
	if !a.reactions || reaction == "" || chatID == "" || msgID == "" {
		return nil
	}
	mid, err := strconv.ParseInt(msgID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: ack: bad message id %q: %w", msgID, err)
	}
	a.enqueueReaction(chatID, mid, reaction)
	return nil
}

// enqueueReaction hands one reaction to the single-worker queue. The worker is
// started lazily so an adapter that never reacts never spawns a goroutine. A
// full queue drops (and logs) rather than blocking the poll loop.
func (a *Adapter) enqueueReaction(chatID string, msgID int64, emoji string) {
	if !a.reactions || emoji == "" || chatID == "" || msgID == 0 {
		return
	}
	a.reactOnce.Do(func() {
		a.reactQ = make(chan reactReq, 64)
		go a.reactLoop()
	})
	select {
	case a.reactQ <- reactReq{chatID: chatID, msgID: msgID, emoji: emoji}:
	default:
		log.Printf("telegram: reaction queue full, dropping %q for %s/%d", emoji, chatID, msgID)
	}
}

// reactLoop serializes reaction calls so the chain lands in order.
func (a *Adapter) reactLoop() {
	for r := range a.reactQ {
		if err := a.setReaction(r.chatID, r.msgID, r.emoji); err != nil {
			log.Printf("telegram: reaction %q on %s/%d failed: %v", r.emoji, r.chatID, r.msgID, err)
		}
	}
}

// setReaction is the synchronous Bot API call. A single-element reaction list
// REPLACES the bot's previous reaction on that message, which is what makes the
// lifecycle chain read as one changing marker instead of a pile of emoji.
func (a *Adapter) setReaction(chatID string, msgID int64, emoji string) error {
	payload, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": msgID,
		"reaction":   []map[string]string{{"type": "emoji", "emoji": emoji}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("%s/bot%s/setMessageReaction", a.base, a.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		// Most commonly REACTION_INVALID (emoji not on Telegram's whitelist).
		return fmt.Errorf("setMessageReaction HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("setMessageReaction not ok: %s", string(body))
	}
	return nil
}

// Notify implements the notify.Sink capability so the generic notification hub
// treats Telegram uniformly with the web channel: a dispatched notification is
// sent as a message to every allowlisted chat id. Telegram is not live without a
// token, but the method exists so registration + fan-out are uniform across
// channels (the hub never special-cases a channel kind). Sends are best-effort;
// the first send error is returned (and logged by the hub).
func (a *Adapter) Notify(n notify.Notification) error {
	if a.token == "" {
		return fmt.Errorf("telegram: notify skipped, empty token")
	}
	text := "[" + string(n.Level) + "] " + n.Source + ": " + n.Text
	var firstErr error
	for chatID := range a.allow {
		if _, err := a.Send(context.Background(), channel.OutboundMsg{ChatID: chatID, Text: text}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
