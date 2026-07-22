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
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/notify"
)

// Adapter is the Telegram channel adapter.
type Adapter struct {
	token   string
	allow   map[string]bool // chat-id allowlist; empty map means "allow none"
	base    string          // Bot API base (overridable for tests)
	client  *http.Client
	inbound chan channel.InboundMsg
	offset  int64
}

// New builds a Telegram adapter. allow is the chat-id allowlist (server-enforced).
func New(token string, allow []string) *Adapter {
	m := make(map[string]bool, len(allow))
	for _, a := range allow {
		m[a] = true
	}
	return &Adapter{
		token:   token,
		allow:   m,
		base:    "https://api.telegram.org",
		client:  &http.Client{Timeout: 65 * time.Second},
		inbound: make(chan channel.InboundMsg, 64),
	}
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
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text           string `json:"text"`
		ReplyToMessage *struct {
			MessageID int64 `json:"message_id"`
		} `json:"reply_to_message"`
	} `json:"message"`
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

// handleUpdate advances the offset, enforces the allowlist, and emits inbound.
func (a *Adapter) handleUpdate(u tgUpdate) {
	if u.UpdateID >= a.offset {
		a.offset = u.UpdateID + 1
	}
	if u.Message == nil || u.Message.Text == "" {
		return
	}
	chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
	if !a.allowed(chatID) {
		log.Printf("telegram: dropping message from non-allowlisted chat %s", chatID)
		return
	}
	msg := channel.InboundMsg{
		Channel: "telegram",
		UserID:  chatID,
		Text:    u.Message.Text,
	}
	if u.Message.ReplyToMessage != nil {
		msg.ReplyTo = strconv.FormatInt(u.Message.ReplyToMessage.MessageID, 10)
	}
	select {
	case a.inbound <- msg:
	default:
		log.Printf("telegram: inbound buffer full, dropping message from %s", chatID)
	}
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

// Ack is a no-op for now (read/typing/react to be added).
func (a *Adapter) Ack(msgID, reaction string) error { return nil }

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
