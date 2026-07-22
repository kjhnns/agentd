// Package web is agentd's WEB CHANNEL: a self-contained, authenticated local
// single-page UI served by the SAME HTTP server as the rest of the API (no
// second port), plus the WebSocket that streams normalized events and pushes
// notifications to connected browsers.
//
// It is the REFERENCE ChannelAdapter and the foundation the future wrist/phone
// app is a client of: it implements the exact same channel.Adapter contract as
// Telegram (Name/Start/Inbound/Send/SupportsMedia/Ack) AND the notify.Sink
// capability, so the core treats it uniformly. A message typed in the UI is
// posted over the WS, emitted as an ordinary channel.InboundMsg, and driven
// through the SAME session.RouteInbound path as a Telegram message would be
// (one warm web session, reused per web user). Agent output streams back over
// the WS from the shared event bus; scheduler/system notifications arrive over
// the same socket via Notify.
//
// Auth is the existing API bearer, extended for browsers: a ?token= on first
// load sets a same-origin httponly cookie, which then authenticates the page,
// the JSON calls, and the WS handshake. See api.(*Server).authed and the README.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/notify"
	"github.com/kjhnns/agentd/internal/wsutil"
)

//go:embed ui.html
var uiHTML []byte

// webUserID keys the single warm web session in session.RouteInbound's
// per-user map (the same way a Telegram chat id keys its session). One local UI
// = one web user = one warm session, re-hydrated on reclaim like any other.
const webUserID = "web"

// Adapter is the web ChannelAdapter + notify.Sink. It owns the set of connected
// WS clients and fans events/notifications/replies out to all of them.
type Adapter struct {
	bus     *eventbus.Bus
	title   string
	inbound chan channel.InboundMsg

	mu      sync.RWMutex
	clients map[*client]struct{}
}

// client is one connected browser: a socket plus a buffered outbound queue
// drained by a dedicated writer goroutine (so a slow client never blocks a
// broadcast and the socket is only written from one goroutine).
type client struct {
	conn *wsutil.Conn
	send chan []byte
}

// New builds a web adapter. title labels the UI; bus is the shared event bus the
// UI streams from.
func New(bus *eventbus.Bus, title string) *Adapter {
	if title == "" {
		title = "agentd"
	}
	return &Adapter{
		bus:     bus,
		title:   title,
		inbound: make(chan channel.InboundMsg, 64),
		clients: make(map[*client]struct{}),
	}
}

// ---- channel.Adapter ----

func (a *Adapter) Name() string                       { return "web" }
func (a *Adapter) SupportsMedia() bool                { return false }
func (a *Adapter) Inbound() <-chan channel.InboundMsg { return a.inbound }
func (a *Adapter) Ack(msgID, reaction string) error   { return nil }

// UserID is the session key the web channel routes under (exported for wiring).
func (a *Adapter) UserID() string { return webUserID }

// Start begins forwarding every bus event to connected clients. It returns
// immediately; the forwarder stops when ctx is done. There is no long-poll or
// external connection to establish (the UI connects TO us).
func (a *Adapter) Start(ctx context.Context) error {
	subID, events := a.bus.Subscribe()
	go func() {
		defer a.bus.Unsubscribe(subID)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok {
					return
				}
				a.broadcast(envelope{Type: "event", Event: &e})
			}
		}
	}()
	return nil
}

// Send delivers the RouteInbound turn result to the browsers as a "reply"
// message and returns a receipt (the acceptance artifact). The turn's normalized
// events already stream over the WS from the bus; the reply is a belt-and-braces
// final message keyed to the chat/session.
func (a *Adapter) Send(ctx context.Context, m channel.OutboundMsg) (channel.SendReceipt, error) {
	n := a.broadcast(envelope{Type: "reply", Chat: m.ChatID, Text: m.Text})
	return channel.SendReceipt{ID: "web-broadcast-" + strconv.Itoa(n)}, nil
}

// ---- notify.Sink ----

// Notify pushes a notification to every connected browser over the WS.
func (a *Adapter) Notify(nn notify.Notification) error {
	a.broadcast(envelope{Type: "notification", Notification: &nn})
	return nil
}

// ---- HTTP handlers (mounted on the shared api server via api.Mount) ----

// UIHandler serves the embedded single-page app. If ?token= is present it stores
// it as a same-origin httponly cookie so the follow-up API/WS calls authenticate
// without a header (the browser cannot set Authorization on navigation or WS).
// Reaching this handler at all means the bearer already passed the api gate.
func (a *Adapter) UIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := r.URL.Query().Get("token"); tok != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     "agentd_token",
				Value:    tok,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(uiHTML)
	}
}

// RootHandler redirects "/" to "/ui" (preserving a ?token=) and 404s anything
// else that falls through to it.
func (a *Adapter) RootHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		target := "/ui"
		if tok := r.URL.Query().Get("token"); tok != "" {
			target += "?token=" + tok
		}
		http.Redirect(w, r, target, http.StatusFound)
	}
}

// WSHandler upgrades to a WebSocket, registers the client for broadcasts, and
// reads typed input from the browser, emitting it as a normalized InboundMsg
// (which main drives through session.RouteInbound, exactly like Telegram).
func (a *Adapter) WSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsutil.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c := &client{conn: conn, send: make(chan []byte, 64)}
		a.register(c)
		defer a.unregister(c)

		// writer: the SOLE goroutine that writes to the socket. It drains the send
		// queue and, once the queue is closed (by unregister), closes the conn
		// itself, so the close frame is never written concurrently with a data
		// frame (which would race the shared bufio.Writer).
		go func() {
			for b := range c.send {
				if err := conn.WriteText(b); err != nil {
					break
				}
			}
			_ = conn.Close()
		}()

		// reader: typed input -> normalized inbound.
		for {
			raw, err := conn.ReadText()
			if err != nil {
				return
			}
			var in struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &in) != nil {
				continue
			}
			if in.Type == "input" && strings.TrimSpace(in.Text) != "" {
				msg := channel.InboundMsg{Channel: "web", UserID: webUserID, Text: in.Text}
				select {
				case a.inbound <- msg:
				default:
					// buffer full: drop, matching the Telegram adapter's policy.
				}
			}
		}
	}
}

// ConfirmHandler handles POST /confirm/:token from the UI's Approve/Deny
// buttons. NOTE (stub): agentd does not yet EMIT confirm-gate tokens from the
// harness (no needs_input event carries a token today), so this endpoint records
// the decision and echoes it back to the UI as a notification, but there is no
// pending gate to resolve yet. It is wired end to end on the UI+HTTP side so that
// when the confirm-gate plumbing lands, only the resolution target changes.
func (a *Adapter) ConfirmHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := strings.TrimPrefix(r.URL.Path, "/confirm/")
		if token == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			return
		}
		var body struct {
			Approve bool `json:"approve"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		decision := "denied"
		if body.Approve {
			decision = "approved"
		}
		a.Notify(notify.Notification{
			Source: "confirm",
			Level:  notify.LevelInfo,
			Text:   "confirm " + token + " " + decision + " (no live gate to resolve yet: stub)",
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "recorded", "token": token, "approve": body.Approve})
	}
}

// ---- client registry + broadcast ----

func (a *Adapter) register(c *client) {
	a.mu.Lock()
	a.clients[c] = struct{}{}
	a.mu.Unlock()
}

// unregister removes a client and closes its send queue. It does NOT touch the
// socket: the writer goroutine closes the conn once the drained queue ends, so
// the socket has exactly one writer (no close-vs-data-frame race).
func (a *Adapter) unregister(c *client) {
	a.mu.Lock()
	if _, ok := a.clients[c]; ok {
		delete(a.clients, c)
		close(c.send)
	}
	a.mu.Unlock()
}

// Clients reports the number of connected browsers (health/tests).
func (a *Adapter) Clients() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.clients)
}

// envelope is the server->client WS message shape.
type envelope struct {
	Type         string               `json:"type"`
	Event        *eventbus.Event      `json:"event,omitempty"`
	Notification *notify.Notification `json:"notification,omitempty"`
	Chat         string               `json:"chat,omitempty"`
	Text         string               `json:"text,omitempty"`
}

// broadcast marshals the envelope once and non-blockingly queues it to every
// connected client, returning how many clients it reached. A full client queue
// drops the message for that client (bounded backpressure, like the event bus).
func (a *Adapter) broadcast(env envelope) int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.clients) == 0 {
		return 0 // nothing connected: skip the marshal entirely (hot path: bus events)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return 0
	}
	sent := 0
	for c := range a.clients {
		select {
		case c.send <- b:
			sent++
		default:
		}
	}
	return sent
}
