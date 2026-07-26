// Package cli is agentd's terminal client. A terminal is NOT an inbound
// transport that has to be polled, so the CLI is deliberately NOT a
// channel.Adapter with its own Start/Inbound loop: it is a CLIENT of the
// ALREADY-RUNNING daemon, speaking the same bearer-gated HTTP + WebSocket API
// the web UI uses.
//
// That choice is what makes the CLI cheap: sessions, conversation history,
// past-session browsing, the workspace and its memory injection, context reset,
// the turn-timeout budget and the failure-notice conventions are all inherited
// from the server rather than reimplemented per surface. The CLI only decides
// WHICH session to attach to and how to render a turn in a terminal.
//
//	agentd run "<prompt>"   one-shot for scripts (stdout + exit code)
//	agentd chat             interactive REPL, streams events over the WS
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjhnns/agentd/internal/config"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/session"
	"github.com/kjhnns/agentd/internal/wsutil"
)

// Exit codes. Scripts key off these, so they are part of the contract:
// 0 success, 1 the turn failed, 2 the invocation was wrong, 3 the daemon is not
// reachable, 4 the turn outran its budget.
const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitUnreachable = 3
	ExitTimeout     = 4
)

// ErrUnreachable marks "the daemon is not answering on that address" as
// distinct from "the daemon answered with an error". A raw connection-refused
// from net/http is unreadable for a user who simply has not started the
// service, so every call maps it to this and a sentence saying what to do.
var ErrUnreachable = errors.New("agentd daemon not reachable")

// ErrTimeout marks a client-side deadline (-wait) that expired. The daemon may
// still finish the turn; only this client stopped waiting for it.
var ErrTimeout = errors.New("timed out waiting for the turn")

// Client talks to one running daemon.
type Client struct {
	Addr  string // host:port, e.g. 127.0.0.1:8788
	Token string // API bearer ("" = unauthenticated daemon)
	HTTP  *http.Client
}

// NewClient builds a client for a host:port. timeout <= 0 leaves the HTTP
// client unbounded (a turn legitimately runs for minutes).
func NewClient(addr, token string, timeout time.Duration) *Client {
	return &Client{Addr: addr, Token: token, HTTP: &http.Client{Timeout: timeout}}
}

func (c *Client) base() string { return "http://" + c.Addr }

// do performs one request and returns the body, mapping transport failures to
// ErrUnreachable and non-2xx to a readable error carrying the status line.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if isUnreachable(err) {
			return nil, fmt.Errorf("%w at %s (is the launchd job com.joe_pa.agentd running?): %v", ErrUnreachable, c.Addr, err)
		}
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 400 {
			msg = msg[:400] + " [...]"
		}
		if resp.StatusCode == http.StatusGatewayTimeout {
			return nil, fmt.Errorf("%w: %s %s -> %s: %s", ErrTimeout, method, path, resp.Status, msg)
		}
		return nil, fmt.Errorf("%s %s -> %s: %s", method, path, resp.Status, msg)
	}
	return data, nil
}

// isUnreachable reports whether the transport failed in a way that means "no
// daemon there" rather than "the daemon said no".
func isUnreachable(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	var de *net.DNSError
	return errors.As(err, &de)
}

// Health probes the daemon. It is the first call every command makes so an
// unreachable or mis-tokened daemon is reported once, in words, instead of
// surfacing as a confusing failure three calls later.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/health", nil, "")
	return err
}

// Sessions lists the live sessions (GET /sessions), ID-sorted so a repeated
// lookup is deterministic (the server iterates a map).
func (c *Client) Sessions(ctx context.Context) ([]session.Status, error) {
	data, err := c.do(ctx, http.MethodGet, "/sessions", nil, "")
	if err != nil {
		return nil, err
	}
	var out []session.Status
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode /sessions: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CreateSession starts a new session (POST /sessions) with the given title,
// homed in workspace ("" = the server's default workspace).
func (c *Client) CreateSession(ctx context.Context, title, workspace string) (string, error) {
	body, _ := json.Marshal(map[string]string{"title": title, "workspace": workspace})
	data, err := c.do(ctx, http.MethodPost, "/sessions", bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode POST /sessions: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("daemon created a session with no id")
	}
	return out.ID, nil
}

// Send posts one turn (POST /sessions/:id/input) and returns the answer. The
// call blocks for the whole turn; that is the point (the endpoint now returns
// the result, not just an ack).
func (c *Client) Send(ctx context.Context, id, text string) (string, error) {
	body, _ := json.Marshal(map[string]string{"text": text})
	data, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/input", bytes.NewReader(body), "application/json")
	if err != nil {
		return "", err
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode input response: %w", err)
	}
	return out.Result, nil
}

// SendFile attaches a file to a turn through the SAME core media ingest path
// every channel uses (POST /sessions/:id/media, multipart field "file" plus an
// optional "text" caption): images become a Read-this-path pointer, audio is
// transcribed by whisper, documents are annotated. Nothing multimodal is
// reimplemented here.
func (c *Client) SendFile(ctx context.Context, id, path, text string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if text != "" {
		if err := mw.WriteField("text", text); err != nil {
			return "", err
		}
	}
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	data, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/media", &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode media response: %w", err)
	}
	return out.Result, nil
}

// Interrupt cancels the session's in-flight turn WITHOUT ending the session
// (POST /sessions/:id/interrupt). This is what Ctrl-C is wired to in the REPL.
func (c *Client) Interrupt(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/interrupt", nil, "")
	return err
}

// Events opens the session's event WebSocket (GET /sessions/:id/events) and
// streams normalized events until the connection closes or stop is called.
// The bearer rides as a ?token= query param: a WebSocket handshake from a
// browser cannot set a header and the server accepts both forms.
func (c *Client) Events(id string) (<-chan eventbus.Event, func(), error) {
	u := "ws://" + c.Addr + "/sessions/" + url.PathEscape(id) + "/events"
	if c.Token != "" {
		u += "?token=" + url.QueryEscape(c.Token)
	}
	conn, err := wsutil.Dial(u, http.Header{})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: event stream: %v", ErrUnreachable, err)
	}
	out := make(chan eventbus.Event, 64)
	go func() {
		defer close(out)
		for {
			payload, err := conn.ReadText()
			if err != nil {
				return
			}
			var e eventbus.Event
			if json.Unmarshal(payload, &e) != nil {
				continue
			}
			select {
			case out <- e:
			default: // never block the socket reader on a slow renderer
			}
		}
	}()
	return out, func() { _ = conn.Close() }, nil
}

// ---- endpoint resolution ----

// Endpoint resolves the daemon address + bearer exactly the way `agentd jobs`
// does (bind + api_bearer from config.toml), with two additions a terminal
// needs: $AGENTD_TOKEN / $AGENTD_ADDR override the file, and the config path
// itself is discovered (explicit flag, then $AGENTD_CONFIG, then
// ~/.agentd/config.toml, then ./config.toml) so a bare `agentd run` works on a
// configured machine.
func Endpoint(cfgPath string) (addr, token string, err error) {
	addr = os.Getenv("AGENTD_ADDR")
	token = os.Getenv("AGENTD_TOKEN")

	path := configPath(cfgPath)
	if path != "" {
		cfg, cerr := config.Load(path)
		if cerr != nil {
			if cfgPath != "" {
				return "", "", fmt.Errorf("config %s: %w", path, cerr)
			}
		} else {
			if addr == "" {
				addr = cfg.Server.Bind
			}
			if token == "" {
				token = config.ResolveToken(cfg.Server.APIBearer)
			}
		}
	}
	if addr == "" {
		return "", "", fmt.Errorf("no daemon address: set $AGENTD_ADDR or [server] bind in %s",
			firstNonEmpty(path, "config.toml"))
	}
	// A bind of ":8788" or "0.0.0.0:8788" is a listen spec, not a dial target.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	addr = strings.Replace(addr, "0.0.0.0:", "127.0.0.1:", 1)
	return addr, token, nil
}

// configPath applies the config discovery precedence.
func configPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("AGENTD_CONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".agentd", "config.toml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("config.toml"); err == nil {
		return "config.toml"
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- session targeting ----

// DefaultTitle is the title of the CLI's persistent session, per local user, so
// context carries across separate invocations and the session is recognisable
// in GET /sessions, GET /sessions/past and the web UI alongside the
// "telegram:<chat>" ones.
func DefaultTitle() string {
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if name == "" {
		name = "local"
	}
	return "cli:" + name
}

// Target selects which session a command talks to.
type Target struct {
	ID        string // explicit session id (-session); wins over everything
	New       bool   // force a fresh session (-new)
	Title     string // title of the persistent CLI session ("" = DefaultTitle)
	Workspace string // workspace for a newly created session ("" = server default)
}

// Resolve finds-or-creates the session for a target and reports whether it had
// to create one. Reuse is by TITLE among the live sessions, which is the same
// "one warm session per conversation" contract the channels get: the daemon's
// idle GC may reclaim it, and the next invocation then transparently opens a
// fresh one (re-hydrated from the workspace artifacts).
func (c *Client) Resolve(ctx context.Context, t Target) (id string, created bool, err error) {
	if t.ID != "" {
		sessions, err := c.Sessions(ctx)
		if err != nil {
			return "", false, err
		}
		for _, s := range sessions {
			if s.ID == t.ID {
				return s.ID, false, nil
			}
		}
		return "", false, fmt.Errorf("session %s is not live (see agentd run -list, or drop -session to use the persistent CLI session)", t.ID)
	}
	title := t.Title
	if title == "" {
		title = DefaultTitle()
	}
	if !t.New {
		sessions, err := c.Sessions(ctx)
		if err != nil {
			return "", false, err
		}
		for _, s := range sessions {
			if s.Title == title {
				return s.ID, false, nil
			}
		}
	}
	newID, err := c.CreateSession(ctx, title, t.Workspace)
	if err != nil {
		return "", false, err
	}
	return newID, true, nil
}

// exitCode maps an error to the process exit code contract.
func exitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrUnreachable):
		return ExitUnreachable
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return ExitTimeout
	default:
		return ExitFailure
	}
}
