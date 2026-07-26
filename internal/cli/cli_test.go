package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/session"
)

// fakeDaemon is an httptest stand-in for the running agentd: the same
// bearer-gated routes the CLI uses (/health, /sessions, /sessions/:id/input,
// /sessions/:id/media, /sessions/:id/interrupt). Behaviour is scriptable so the
// tests can drive a slow turn, a failing turn, or a missing session.
type fakeDaemon struct {
	mu       sync.Mutex
	bearer   string
	titles   map[string]string // session id -> title
	nextID   int
	creates  int
	inputs   []inputCall
	uploads  []string // filenames received on /media
	reply    string
	delay    time.Duration
	inputErr int // when non-zero, /input answers this status
}

type inputCall struct{ session, text string }

func newFakeDaemon(t *testing.T) (*fakeDaemon, *httptest.Server) {
	t.Helper()
	d := &fakeDaemon{bearer: "tok", titles: map[string]string{}, reply: "PONG"}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return d, srv
}

// addr returns the host:port the CLI dials.
func addrOf(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

func (d *fakeDaemon) snapshotInputs() []inputCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]inputCall, len(d.inputs))
	copy(out, d.inputs)
	return out
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != d.bearer &&
		r.URL.Query().Get("token") != d.bearer {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/health":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case r.URL.Path == "/sessions" && r.Method == http.MethodGet:
		d.mu.Lock()
		list := make([]session.Status, 0, len(d.titles))
		for id, title := range d.titles {
			list = append(list, session.Status{ID: id, Title: title, Status: "idle", AgentOK: true})
		}
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(list)
	case r.URL.Path == "/sessions" && r.Method == http.MethodPost:
		var body struct{ Title, Workspace string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		d.nextID++
		d.creates++
		id := "sess" + string(rune('0'+d.nextID))
		d.titles[id] = body.Title
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case strings.HasSuffix(r.URL.Path, "/input"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/input")
		var body struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		d.inputs = append(d.inputs, inputCall{id, body.Text})
		delay, code, reply := d.delay, d.inputErr, d.reply
		d.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if code != 0 {
			http.Error(w, "harness exploded", code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "sent", "id": id, "result": reply})
	case strings.HasSuffix(r.URL.Path, "/media"):
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.uploads = append(d.uploads, hdr.Filename)
		d.inputs = append(d.inputs, inputCall{"media", r.FormValue("text")})
		reply := d.reply
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "routed", "result": reply})
	case strings.HasSuffix(r.URL.Path, "/interrupt"):
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "interrupted"})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// point aims the CLI at a fake daemon through the documented env overrides.
func point(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("AGENTD_ADDR", addrOf(srv))
	t.Setenv("AGENTD_TOKEN", "tok")
}

// runCLI invokes `agentd run` with captured streams.
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb, strings.NewReader(stdin))
	return code, out.String(), errb.String()
}

// TestRunOneShotPrintsAnswer: the money path for scripts. The answer goes to
// stdout, exit code is 0, and the turn landed on the PERSISTENT cli session
// (created on first use because the daemon had none).
func TestRunOneShotPrintsAnswer(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)

	code, stdout, stderr := runCLI(t, "", "Reply with exactly the word PONG and nothing else")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if stdout != "PONG\n" {
		t.Fatalf("stdout = %q, want \"PONG\\n\"", stdout)
	}
	inputs := d.snapshotInputs()
	if len(inputs) != 1 || !strings.Contains(inputs[0].text, "PONG") {
		t.Fatalf("daemon saw %v", inputs)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if got := d.titles[inputs[0].session]; got != DefaultTitle() {
		t.Fatalf("turn went to session titled %q, want %q", got, DefaultTitle())
	}
}

// TestRunReusesPersistentSession: two separate invocations must land on the
// SAME session so context carries across them (the whole point of a stable
// title); only the first one creates.
func TestRunReusesPersistentSession(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)

	if code, _, e := runCLI(t, "", "first"); code != ExitOK {
		t.Fatalf("first exit = %d: %s", code, e)
	}
	if code, _, e := runCLI(t, "", "second"); code != ExitOK {
		t.Fatalf("second exit = %d: %s", code, e)
	}
	inputs := d.snapshotInputs()
	if len(inputs) != 2 || inputs[0].session != inputs[1].session {
		t.Fatalf("invocations hit different sessions: %v", inputs)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creates != 1 {
		t.Fatalf("created %d sessions, want 1 reused across invocations", d.creates)
	}
}

// TestRunNewStartsFreshSession: -new opts out of the persistent session.
func TestRunNewStartsFreshSession(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)

	runCLI(t, "", "first")
	runCLI(t, "", "-new", "second")
	inputs := d.snapshotInputs()
	if len(inputs) != 2 || inputs[0].session == inputs[1].session {
		t.Fatalf("-new reused the session: %v", inputs)
	}
}

// TestRunSessionTargeting: -session picks an existing live session, and an id
// the daemon does not hold fails rather than silently opening a new one.
func TestRunSessionTargeting(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)
	d.mu.Lock()
	d.titles["telegram-a"] = "telegram:7597951120"
	d.mu.Unlock()

	code, _, stderr := runCLI(t, "", "-session", "telegram-a", "hello")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got := d.snapshotInputs(); len(got) != 1 || got[0].session != "telegram-a" {
		t.Fatalf("turn went to %v, want telegram-a", got)
	}

	code, _, stderr = runCLI(t, "", "-session", "does-not-exist", "hello")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if !strings.Contains(stderr, "not live") {
		t.Fatalf("stderr = %q, want a not-live explanation", stderr)
	}
}

// TestRunFailureExitCode: a daemon-side turn failure exits non-zero with the
// reason on stderr and NOTHING on stdout (a script must not mistake an error
// for an answer).
func TestRunFailureExitCode(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)
	d.mu.Lock()
	d.inputErr = http.StatusInternalServerError
	d.mu.Unlock()

	code, stdout, stderr := runCLI(t, "", "boom")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "500") {
		t.Fatalf("stderr = %q, want the status line", stderr)
	}
}

// TestRunTimeoutExitCode: -wait bounds the client; expiry is its own exit code
// so a script can retry a slow turn differently from a failed one.
func TestRunTimeoutExitCode(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)
	d.mu.Lock()
	d.delay = 2 * time.Second
	d.mu.Unlock()

	code, stdout, stderr := runCLI(t, "", "-wait", "150ms", "slow")
	if code != ExitTimeout {
		t.Fatalf("exit = %d, want %d (stderr %s)", code, ExitTimeout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty on timeout", stdout)
	}
}

// TestRunJSONShape: -json is the structured contract for scripts, on success
// and on failure.
func TestRunJSONShape(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)

	code, stdout, _ := runCLI(t, "", "-json", "ping")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var ok struct {
		OK         bool   `json:"ok"`
		Session    string `json:"session"`
		Result     string `json:"result"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal([]byte(stdout), &ok); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if !ok.OK || ok.Result != "PONG" || ok.Session == "" {
		t.Fatalf("json = %+v", ok)
	}

	d.mu.Lock()
	d.inputErr = http.StatusInternalServerError
	d.mu.Unlock()
	code, stdout, _ = runCLI(t, "", "-json", "boom")
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	var bad struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Exit  int    `json:"exit"`
	}
	if err := json.Unmarshal([]byte(stdout), &bad); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if bad.OK || bad.Error == "" || bad.Exit != ExitFailure {
		t.Fatalf("json = %+v", bad)
	}
}

// TestRunDaemonNotRunning: a dead port must produce a sentence about the
// daemon and exit code 3, not a raw dial error.
func TestRunDaemonNotRunning(t *testing.T) {
	_, srv := newFakeDaemon(t)
	addr := addrOf(srv)
	srv.Close() // nothing listening now
	t.Setenv("AGENTD_ADDR", addr)
	t.Setenv("AGENTD_TOKEN", "tok")

	code, _, stderr := runCLI(t, "", "hello")
	if code != ExitUnreachable {
		t.Fatalf("exit = %d, want %d", code, ExitUnreachable)
	}
	if !strings.Contains(stderr, "not reachable") {
		t.Fatalf("stderr = %q, want a readable not-reachable message", stderr)
	}
}

// TestRunReadsPromptFromStdin: the pipe form scripts use.
func TestRunReadsPromptFromStdin(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)

	code, stdout, stderr := runCLI(t, "summarize this please\n")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if stdout != "PONG\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if got := d.snapshotInputs(); len(got) != 1 || got[0].text != "summarize this please" {
		t.Fatalf("daemon saw %v", got)
	}
}

// TestRunFileGoesThroughMediaIngest: -file uses the SAME core media endpoint
// the channels use, with the prompt as the caption.
func TestRunFileGoesThroughMediaIngest(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("not really a png"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, "", "-file", path, "what is this")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if stdout != "PONG\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.uploads) != 1 || d.uploads[0] != "shot.png" {
		t.Fatalf("uploads = %v", d.uploads)
	}
}

// TestRunNoPromptIsUsageError: nothing to send is a usage error, distinct from
// a failed turn.
func TestRunNoPromptIsUsageError(t *testing.T) {
	_, srv := newFakeDaemon(t)
	point(t, srv)
	if code, _, _ := runCLI(t, ""); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
}

// TestSessionsCommandLists: the id-finding surface for -session.
func TestSessionsCommandLists(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)
	d.mu.Lock()
	d.titles["abc123"] = "telegram:7597951120"
	d.mu.Unlock()

	var out, errb bytes.Buffer
	if code := Sessions(nil, &out, &errb); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "abc123") || !strings.Contains(out.String(), "telegram:7597951120") {
		t.Fatalf("listing = %q", out.String())
	}
}

// TestChatREPLTurns: the REPL against a fake daemon (streaming disabled, since
// stdin here is a pipe and there is no live event socket). Two lines produce
// two turns on ONE session, /exit quits cleanly.
func TestChatREPLTurns(t *testing.T) {
	d, srv := newFakeDaemon(t)
	point(t, srv)

	var out, errb bytes.Buffer
	stdin := strings.NewReader("first question\nsecond question\n/exit\n")
	if code := Chat([]string{"-no-stream"}, &out, &errb, stdin); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	inputs := d.snapshotInputs()
	if len(inputs) != 2 || inputs[0].session != inputs[1].session {
		t.Fatalf("REPL turns = %v", inputs)
	}
	if !strings.Contains(out.String(), "PONG") {
		t.Fatalf("REPL never printed the answer: %q", out.String())
	}
	if !strings.Contains(out.String(), "session "+inputs[0].session) {
		t.Fatalf("REPL did not name the session it attached to: %q", out.String())
	}
	// The working / done indicator is the CLI analogue of the emoji chain.
	if !strings.Contains(errb.String(), "[working]") || !strings.Contains(errb.String(), "[done]") {
		t.Fatalf("no progress indicator: %q", errb.String())
	}
}

// TestEndpointResolution: bind + bearer come from config.toml exactly as
// `agentd jobs` reads them, $AGENTD_TOKEN / $AGENTD_ADDR override the file, and
// a listen-only bind is normalized into something dialable.
func TestEndpointResolution(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nbind = \"127.0.0.1:8788\"\napi_bearer = \"file-token\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("from config.toml", func(t *testing.T) {
		t.Setenv("AGENTD_ADDR", "")
		t.Setenv("AGENTD_TOKEN", "")
		addr, token, err := Endpoint(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if addr != "127.0.0.1:8788" || token != "file-token" {
			t.Fatalf("got %q %q", addr, token)
		}
	})

	t.Run("env overrides the file", func(t *testing.T) {
		t.Setenv("AGENTD_ADDR", "127.0.0.1:9999")
		t.Setenv("AGENTD_TOKEN", "env-token")
		addr, token, err := Endpoint(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if addr != "127.0.0.1:9999" || token != "env-token" {
			t.Fatalf("got %q %q", addr, token)
		}
	})

	t.Run("AGENTD_CONFIG is honored", func(t *testing.T) {
		t.Setenv("AGENTD_ADDR", "")
		t.Setenv("AGENTD_TOKEN", "")
		t.Setenv("AGENTD_CONFIG", cfg)
		addr, token, err := Endpoint("")
		if err != nil {
			t.Fatal(err)
		}
		if addr != "127.0.0.1:8788" || token != "file-token" {
			t.Fatalf("got %q %q", addr, token)
		}
	})

	t.Run("listen-only bind is made dialable", func(t *testing.T) {
		wildcard := filepath.Join(dir, "wild.toml")
		if err := os.WriteFile(wildcard, []byte("[server]\nbind = \":8788\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTD_ADDR", "")
		t.Setenv("AGENTD_TOKEN", "")
		addr, _, err := Endpoint(wildcard)
		if err != nil {
			t.Fatal(err)
		}
		if addr != "127.0.0.1:8788" {
			t.Fatalf("addr = %q", addr)
		}
	})

	t.Run("explicit missing config is an error", func(t *testing.T) {
		t.Setenv("AGENTD_ADDR", "")
		t.Setenv("AGENTD_TOKEN", "")
		if _, _, err := Endpoint(filepath.Join(dir, "nope.toml")); err == nil {
			t.Fatal("want an error for an explicit missing config")
		}
	})
}
