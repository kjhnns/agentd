package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/api"
	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/channel/web"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/notify"
	"github.com/kjhnns/agentd/internal/scheduler"
	"github.com/kjhnns/agentd/internal/session"
	"github.com/kjhnns/agentd/internal/wsutil"
)

// ---- an echo harness so RouteInbound can drive a real turn with no live claude ----

type echoHandle struct {
	id     string
	events chan eventbus.Event
}

func (h *echoHandle) ID() string { return h.id }

type echoAdapter struct{}

func (echoAdapter) Name() string                       { return "echo" }
func (echoAdapter) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (echoAdapter) Interrupt(harness.Handle) error     { return nil }
func (echoAdapter) Status(harness.Handle) harness.Status {
	return harness.StatusIdle
}
func (echoAdapter) Pressure(harness.Handle) harness.ContextPressure {
	return harness.ContextPressure{}
}
func (echoAdapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	return &echoHandle{id: cfg.SessionID, events: make(chan eventbus.Event, 16)}, nil
}
func (echoAdapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &echoHandle{id: existing, events: make(chan eventbus.Event, 16)}, nil
}
func (echoAdapter) Teardown(h harness.Handle) error {
	close(h.(*echoHandle).events)
	return nil
}
func (echoAdapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*echoHandle).events
}
func (echoAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	bh := h.(*echoHandle)
	bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindOutput, Text: "thinking about: " + in.Text}
	bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindResult, Text: "reply: " + in.Text}
	return nil
}

// harness fake package name shim: session.NewManager wants harness.Adapter.
var _ harness.Adapter = echoAdapter{}

// ---- helpers ----

// newStack builds the api server (bearer "tok") with a web channel mounted, plus
// the inbound loop that routes web input through session.RouteInbound. Returns
// the running httptest server and the web adapter.
func newStack(t *testing.T) (*httptest.Server, *web.Adapter) {
	return newStackWith(t, echoAdapter{})
}

func newStackWith(t *testing.T, ha harness.Adapter) (*httptest.Server, *web.Adapter) {
	t.Helper()
	bus := eventbus.New()
	mgr := session.NewManager(ha, bus, nil)
	mgr.Policy = session.Policy{} // no lifecycle triggers

	webCh := web.New(bus, "test-ui")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := webCh.Start(ctx); err != nil {
		t.Fatalf("web Start: %v", err)
	}

	srv := api.New("tok", mgr, bus, func() bool { return true })
	srv.Mount("/ui", webCh.UIHandler())
	srv.Mount("/ws", webCh.WSHandler())
	srv.Mount("/confirm/", webCh.ConfirmHandler())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Inbound loop: exactly what cmd/agentd/main.go wires for the web channel.
	sessionForWeb := map[string]string{}
	go func() {
		for in := range webCh.Inbound() {
			in := in
			go func() { _ = mgr.RouteInbound(ctx, webCh, in, sessionForWeb, "", "") }()
		}
	}()
	return ts, webCh
}

func wsURL(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

func waitClients(t *testing.T, w *web.Adapter, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.Clients() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d ws client(s), have %d", n, w.Clients())
}

// readEnvelopes pumps decoded WS envelopes onto a channel until the conn closes.
func readEnvelopes(conn *wsutil.Conn) <-chan map[string]any {
	out := make(chan map[string]any, 32)
	go func() {
		defer close(out)
		for {
			raw, err := conn.ReadText()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				out <- m
			}
		}
	}()
	return out
}

// ---- tests ----

// TestUIServedBehindBearerGate: GET /ui is 401 without the bearer and 200 (the
// app shell) with it; a ?token= load sets the auth cookie for follow-up calls.
func TestUIServedBehindBearerGate(t *testing.T) {
	ts, _ := newStack(t)

	// No bearer -> 401.
	resp, err := http.Get(ts.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /ui without bearer = %d, want 401", resp.StatusCode)
	}

	// Header bearer -> 200 + app shell.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui with bearer = %d, want 200", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), `id="app"`) {
		t.Fatalf("UI response missing app shell (len=%d)", len(body))
	}

	// ?token= load -> 200 and sets the httponly auth cookie.
	resp3, err := http.Get(ts.URL + "/ui?token=tok")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui?token= = %d, want 200", resp3.StatusCode)
	}
	var gotCookie bool
	for _, c := range resp3.Cookies() {
		if c.Name == "agentd_token" && c.Value == "tok" {
			gotCookie = true
		}
	}
	if !gotCookie {
		t.Fatal("GET /ui?token= did not set the agentd_token cookie")
	}
}

// TestWSInputRoundTrip: an authenticated Go WS client connects, sends a typed
// input, and receives the session's normalized events back (proving the web
// inbound path drives a session via RouteInbound and streams events over WS).
// Uses the echo harness, no live claude.
func TestWSInputRoundTrip(t *testing.T) {
	ts, webCh := newStack(t)

	conn, err := wsutil.Dial(wsURL(ts, "/ws?token=tok"), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitClients(t, webCh, 1)

	envs := readEnvelopes(conn)
	if err := conn.WriteText([]byte(`{"type":"input","text":"ping"}`)); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	// The bus forwarder and the reply broadcast are separate goroutines, so the
	// reply envelope can legally arrive before the streamed events: wait until
	// BOTH the intermediate output and a terminal (result or reply) landed.
	var sawOutput, sawResult, sawReply bool
	deadline := time.After(5 * time.Second)
	for !sawOutput || !(sawResult || sawReply) {
		select {
		case m := <-envs:
			switch m["type"] {
			case "event":
				ev, _ := m["event"].(map[string]any)
				if ev["kind"] == "output" && strings.Contains(str(ev["text"]), "ping") {
					sawOutput = true
				}
				if ev["kind"] == "result" && strings.Contains(str(ev["text"]), "ping") {
					sawResult = true
				}
			case "reply":
				if strings.Contains(str(m["text"]), "ping") {
					sawReply = true
				}
			}
		case <-deadline:
			t.Fatalf("timeout; sawOutput=%v sawResult=%v sawReply=%v", sawOutput, sawResult, sawReply)
		}
	}
	if !sawOutput {
		t.Error("did not receive the intermediate output event over WS")
	}
}

// claudeLikeAdapter mimics the REAL claudecode harness contract after the
// dedupe fix: the turn's reply text streams as ONE output event and the result
// event is a text-less turn-terminal marker (its text was blanked because
// claude's terminal result line duplicated the final assistant text).
type claudeLikeAdapter struct{ echoAdapter }

func (claudeLikeAdapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	bh := h.(*echoHandle)
	bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindOutput, Text: "PONG " + in.Text}
	bh.events <- eventbus.Event{SessionID: bh.id, Kind: eventbus.KindResult, Text: "", Status: "idle"}
	return nil
}

// TestSingleVisibleReplyPerTurn is the regression test for the doubled web UI
// reply (every answer rendered once as OUTPUT and again as RESULT): with the
// harness emitting the deduped stream, the reply text must reach the WS in
// EXACTLY ONE event envelope, the result envelope must arrive text-less (so
// the UI renders nothing for it), and the RouteInbound reply envelope (which
// the UI ignores; Telegram consumes its equivalent) must still carry the full
// reply text via the last-output fallback.
func TestSingleVisibleReplyPerTurn(t *testing.T) {
	ts, webCh := newStackWith(t, claudeLikeAdapter{})

	conn, err := wsutil.Dial(wsURL(ts, "/ws?token=tok"), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitClients(t, webCh, 1)

	envs := readEnvelopes(conn)
	if err := conn.WriteText([]byte(`{"type":"input","text":"ping"}`)); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	visibleEvents := 0
	replyText := ""
	sawResultMarker := false
	handleEnv := func(m map[string]any) {
		switch m["type"] {
		case "event":
			ev, _ := m["event"].(map[string]any)
			if strings.Contains(str(ev["text"]), "PONG") {
				visibleEvents++
			}
			if ev["kind"] == "result" {
				sawResultMarker = true
				if str(ev["text"]) != "" {
					t.Errorf("result event still carries reply text %q over the WS", ev["text"])
				}
			}
		case "reply":
			replyText = str(m["text"])
		}
	}
	deadline := time.After(5 * time.Second)
	for replyText == "" || !sawResultMarker {
		select {
		case m := <-envs:
			handleEnv(m)
		case <-deadline:
			t.Fatalf("timeout; visibleEvents=%d sawResultMarker=%v replyText=%q", visibleEvents, sawResultMarker, replyText)
		}
	}
	// Drain a short quiet window to catch any late duplicate.
	quiet := time.After(300 * time.Millisecond)
drain:
	for {
		select {
		case m := <-envs:
			handleEnv(m)
		case <-quiet:
			break drain
		}
	}

	if visibleEvents != 1 {
		t.Fatalf("reply text arrived in %d event envelopes, want exactly 1", visibleEvents)
	}
	if !sawResultMarker {
		t.Error("turn-terminal result event never arrived over the WS")
	}
	if !strings.Contains(replyText, "PONG ping") {
		t.Fatalf("reply envelope = %q, want the turn's reply text via last-output fallback", replyText)
	}
}

// TestNotificationOverWS: a notification dispatched to the web channel (as a
// notify.Sink) is pushed to the connected WS client.
func TestNotificationOverWS(t *testing.T) {
	ts, webCh := newStack(t)

	conn, err := wsutil.Dial(wsURL(ts, "/ws?token=tok"), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitClients(t, webCh, 1)

	envs := readEnvelopes(conn)

	// Dispatch through a hub so we exercise the real Sink registration too.
	hub := notify.NewHub()
	hub.Register(webCh)
	hub.Dispatch(notify.Notification{Source: "job:nightly", Level: notify.LevelIssue, Text: "disk almost full"})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-envs:
			if m["type"] == "notification" {
				n, _ := m["notification"].(map[string]any)
				if str(n["text"]) == "disk almost full" && str(n["source"]) == "job:nightly" {
					return // success
				}
			}
		case <-deadline:
			t.Fatal("notification never arrived over the WS")
		}
	}
}

// TestWSDeliversUnsolicitedReply: the web adapter's Send (the RouteInbound reply
// path) broadcasts a reply envelope to connected clients.
func TestWSDeliversReplyEnvelope(t *testing.T) {
	ts, webCh := newStack(t)
	conn, err := wsutil.Dial(wsURL(ts, "/ws?token=tok"), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitClients(t, webCh, 1)
	envs := readEnvelopes(conn)

	rcpt, err := webCh.Send(context.Background(), channel.OutboundMsg{ChatID: "web", Text: "hello world"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.HasPrefix(rcpt.ID, "web-broadcast-") {
		t.Fatalf("receipt id = %q, want web-broadcast-*", rcpt.ID)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-envs:
			if m["type"] == "reply" && str(m["text"]) == "hello world" {
				return
			}
		case <-deadline:
			t.Fatal("reply envelope never arrived over the WS")
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// ---- end-to-end money demo: POST /jobs/:name/run -> scheduler -> notifier ->
//      web channel -> connected WS client, over the REAL http/ws/api/scheduler
//      stack. The harness runner is faked (no live claude) so it is fast and
//      deterministic; every other hop is the production code path. ----

type demoRunner struct{ result string }

func (r demoRunner) RunTurn(ctx context.Context, ws, prompt, title string) (string, error) {
	return r.result, nil
}

func TestEndToEndSchedulerNotifyToWebClient(t *testing.T) {
	bus := eventbus.New()
	mgr := session.NewManager(nil, bus, nil) // no sessions created in this path
	hub := notify.NewHub()

	// A notify=always webhook job so a clean run still surfaces.
	job := &scheduler.Job{Name: "demo", Enabled: true, Prompt: "p",
		Notify: scheduler.NotifyAlways, Trigger: scheduler.Trigger{Kind: scheduler.TriggerWebhook}}
	statePath := t.TempDir() + "/sched.jsonl"
	sched, err := scheduler.New([]*scheduler.Job{job}, demoRunner{result: "workspace is healthy"}, statePath, nil)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	defer sched.Close()
	sched.OnNotify = func(j *scheduler.Job, run scheduler.JobRun) {
		lvl := notify.LevelResult
		if run.Status != "ok" {
			lvl = notify.LevelIssue
		}
		hub.Dispatch(notify.Notification{Source: "job:" + j.Name, Level: lvl,
			Text: scheduler.NotifyText(j, run), Ts: run.End})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)

	webCh := web.New(bus, "demo")
	if err := webCh.Start(ctx); err != nil {
		t.Fatalf("web Start: %v", err)
	}
	hub.Register(webCh)

	srv := api.New("tok", mgr, bus, func() bool { return true })
	srv.AttachScheduler(sched)
	srv.Mount("/ws", webCh.WSHandler())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	conn, err := wsutil.Dial(wsURL(ts, "/ws?token=tok"), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	waitClients(t, webCh, 1)
	envs := readEnvelopes(conn)

	// Fire the job via the real HTTP surface (bearer-gated), as an operator would.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/jobs/demo/run", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs/demo/run: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /jobs/demo/run = %d, want 202", resp.StatusCode)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-envs:
			if m["type"] == "notification" {
				n, _ := m["notification"].(map[string]any)
				if str(n["source"]) == "job:demo" && strings.Contains(str(n["text"]), "workspace is healthy") {
					t.Logf("MONEY DEMO: web WS client received job notification: %v", n)
					return
				}
			}
		case <-deadline:
			t.Fatal("scheduler job notification never reached the web WS client")
		}
	}
}
