// Package claudecode implements the HarnessAdapter for Claude Code (design 3.2,
// concrete adapter A) as a PERSISTENT, full-continuity streaming session.
//
// Start spawns ONE long-lived process per agentd session:
//
//	claude -p --input-format stream-json --output-format stream-json --verbose \
//	       [--dangerously-skip-permissions] [--model M] [--append-system-prompt S]
//
// The process STAYS ALIVE for the session lifetime. Send writes a user-message
// JSON envelope to the process stdin (never closing it), so every turn runs on
// the same process with full conversation continuity (memory, context, and tools
// carry across turns). A background reader consumes the stdout event stream
// continuously and maps each JSON line to normalized eventbus.Events via
// ParseLine; there is one `result` event per turn over the process lifetime.
//
// A raw PTY transport (design 3.2's universal fallback) is deliberately NOT built
// here; it is a documented future option. A one-shot helper (OneShot) is retained
// for the `smoke` subcommand only.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// DefaultContextWindow is the assumed model context window (tokens) when the
// model id is unknown and no override is configured. Real pressure = measured
// tokens / this window.
const DefaultContextWindow = 200000

// modelWindows maps known model id substrings to their context window so
// pressure divides by the RIGHT denominator when the model is known. Matched by
// substring (case-insensitive) so both aliases and full ids resolve.
var modelWindows = map[string]int{
	"opus":   200000,
	"sonnet": 200000,
	"haiku":  200000,
}

// Adapter drives the claude CLI as a persistent streaming session.
type Adapter struct {
	bin           string // path/name of the claude binary
	contextWindow int    // default context window for real pressure (0 => DefaultContextWindow)
}

// New returns an adapter using the given binary name (default "claude").
func New(bin string) *Adapter {
	if bin == "" {
		bin = "claude"
	}
	return &Adapter{bin: bin, contextWindow: DefaultContextWindow}
}

// WithContextWindow overrides the default context window (tokens) used to
// normalize real token usage into a 0..1 pressure fraction. A known model id
// still takes precedence per session (see resolveWindow). Zero/negative keeps
// the default.
func (a *Adapter) WithContextWindow(tokens int) *Adapter {
	if tokens > 0 {
		a.contextWindow = tokens
	}
	return a
}

// resolveWindow picks the context window for a session: a known model id wins,
// else the adapter's configured/default window.
func (a *Adapter) resolveWindow(model string) int {
	if model != "" {
		lm := strings.ToLower(model)
		for frag, w := range modelWindows {
			if strings.Contains(lm, frag) {
				return w
			}
		}
	}
	if a.contextWindow > 0 {
		return a.contextWindow
	}
	return DefaultContextWindow
}

func (a *Adapter) Name() string { return "claude-code" }

func (a *Adapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{StructuredEvents: true, Interrupt: true, Resume: true, RealContextPressure: true}
}

// handle is the per-session state around ONE long-lived claude process.
type handle struct {
	id  string
	bin string
	cfg harness.SessionConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	events chan eventbus.Event

	procCancel context.CancelFunc
	done       chan struct{} // closed once the process exits / stream ends
	doneOnce   sync.Once

	sendMu sync.Mutex // serializes turns: one turn on the process at a time

	mu              sync.Mutex
	claudeSessionID string
	status          harness.Status
	dead            bool
	waiter          chan eventbus.Event // set while a turn awaits its result/error

	// context-pressure tracking (see Pressure). contextWindow is the real
	// denominator; lastTotalTokens/haveUsage hold the latest measured usage;
	// turns/bytes/startedAt feed the proxy backstop before any usage is seen.
	contextWindow   int
	proxyBudget     harness.ProxyBudget
	startedAt       time.Time
	turns           int
	bytes           int64
	lastTotalTokens int
	haveUsage       bool
}

func (h *handle) ID() string { return h.id }

// Start spawns the persistent streaming process. The passed ctx is used only for
// startup; the process lifetime is owned by the handle (Teardown cancels it), so
// a short-lived request context does not kill the session.
func (a *Adapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("claudecode: SessionConfig.SessionID required")
	}
	args := buildArgs(cfg)

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, a.bin, args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = nil // claude's own diagnostics; not part of the event stream

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("claudecode: start %s: %w", a.bin, err)
	}

	h := &handle{
		id:            cfg.SessionID,
		bin:           a.bin,
		cfg:           cfg,
		cmd:           cmd,
		stdin:         stdin,
		stdout:        stdout,
		events:        make(chan eventbus.Event, 256),
		procCancel:    cancel,
		done:          make(chan struct{}),
		status:        harness.StatusIdle,
		contextWindow: a.resolveWindow(cfg.Model),
		proxyBudget:   harness.DefaultProxyBudget(),
		startedAt:     time.Now(),
	}
	go h.readLoop()
	return h, nil
}

// buildArgs assembles the CLI arguments for a persistent streaming session.
// SessionConfig.SystemPrompt (the workspace injection composed by the Session
// Manager: instructions + memory index + handoff) maps to
// --append-system-prompt, the server-owned context hook (design 3.5).
func buildArgs(cfg harness.SessionConfig) []string {
	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	// Permission policy: PermissionMode (ACP-ready value) wins when set, else
	// fall back to the SkipPermissions bool. This keeps the flag from being
	// hard-coded inline so a future ACP adapter can map the same policy to
	// client-side permission handling instead of a CLI flag.
	skip := cfg.SkipPermissions
	switch cfg.PermissionMode {
	case harness.PermissionSkip:
		skip = true
	case harness.PermissionPrompt:
		skip = false
	}
	if skip {
		args = append(args, "--dangerously-skip-permissions")
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.SystemPrompt)
	}
	return args
}

// Attach re-binds by claude session id. In the persistent model there is no live
// process to re-attach to after a server restart; a fresh process would be
// spawned with --resume in a future pass. For now Attach returns a dead handle
// carrying the resume id so callers can decide to re-Start.
func (a *Adapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &handle{
		id:              existing,
		bin:             a.bin,
		events:          make(chan eventbus.Event, 1),
		done:            make(chan struct{}),
		status:          harness.StatusDead,
		dead:            true,
		claudeSessionID: existing,
		contextWindow:   a.resolveWindow(""),
		proxyBudget:     harness.DefaultProxyBudget(),
		startedAt:       time.Now(),
	}, nil
}

func (a *Adapter) Events(h harness.Handle) <-chan eventbus.Event {
	return h.(*handle).events
}

func (a *Adapter) Status(h harness.Handle) harness.Status {
	hh := h.(*handle)
	hh.mu.Lock()
	defer hh.mu.Unlock()
	return hh.status
}

// Pressure reports the live context pressure. If any measured usage has been
// seen it is REAL (latest measured tokens / the session's context window),
// otherwise a PROXY estimate from turns/bytes/wall-clock (the backstop that
// protects an early session or a would-be usage gap).
func (a *Adapter) Pressure(h harness.Handle) harness.ContextPressure {
	hh := h.(*handle)
	hh.mu.Lock()
	defer hh.mu.Unlock()
	if hh.haveUsage && hh.contextWindow > 0 {
		frac := float64(hh.lastTotalTokens) / float64(hh.contextWindow)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		return harness.ContextPressure{
			Fraction: frac,
			Source:   harness.PressureReal,
			Tokens:   hh.lastTotalTokens,
			Window:   hh.contextWindow,
		}
	}
	return hh.proxyBudget.Pressure(hh.turns, hh.bytes, time.Since(hh.startedAt))
}

// usageBlock is the subset of a stream-json usage object we sum. The tokens
// occupying the context window at the end of a turn are the prompt tokens
// (input + cache_read + cache_creation) plus the produced output.
type usageBlock struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u usageBlock) total() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens + u.OutputTokens
}

// usageLine locates a usage block on either a top-level result line
// (`{"type":"result","usage":{...}}`) or an assistant message line
// (`{"type":"assistant","message":{"usage":{...}}}`).
type usageLine struct {
	Usage   *usageBlock `json:"usage"`
	Message *struct {
		Usage *usageBlock `json:"usage"`
	} `json:"message"`
}

// extractUsage parses the total context tokens from one stream-json line,
// returning (0,false) when the line carries no usage. This is what makes Claude
// pressure REAL rather than a proxy.
func extractUsage(line []byte) (int, bool) {
	var ul usageLine
	if err := json.Unmarshal(line, &ul); err != nil {
		return 0, false
	}
	if ul.Usage != nil {
		if t := ul.Usage.total(); t > 0 {
			return t, true
		}
	}
	if ul.Message != nil && ul.Message.Usage != nil {
		if t := ul.Message.Usage.total(); t > 0 {
			return t, true
		}
	}
	return 0, false
}

// resultDeduper blanks the redundant text on a turn's success result event when
// the identical text was already streamed as the turn's final output event.
// claude's stream-json terminal result line repeats the final assistant text
// verbatim; forwarding both means every channel renders the reply twice (the
// web UI showed an OUTPUT card and an identical RESULT card). The result event
// still flows as the turn-terminal marker (kind, status, session id); only the
// duplicated text is dropped. Error events are never blanked. Consumers that
// want the turn's final reply text use the result text when present, else the
// last output text of the turn (see session.RouteInbound).
type resultDeduper struct{ lastOutput string }

// Apply rewrites evs in place, blanking a success result's text that duplicates
// the last streamed output, and resets its state at each turn boundary.
func (d *resultDeduper) Apply(evs []eventbus.Event) []eventbus.Event {
	for i := range evs {
		switch evs[i].Kind {
		case eventbus.KindOutput:
			d.lastOutput = evs[i].Text
		case eventbus.KindResult:
			if evs[i].Text != "" && evs[i].Text == d.lastOutput {
				evs[i].Text = ""
			}
			d.lastOutput = "" // turn boundary: next turn starts fresh
		case eventbus.KindError:
			d.lastOutput = ""
		}
	}
	return evs
}

// readLoop consumes the stdout stream for the whole process lifetime, publishing
// normalized events and signaling any in-flight turn's waiter on result/error.
func (h *handle) readLoop() {
	defer close(h.events)
	defer h.markDead()

	dedup := &resultDeduper{}
	sc := bufio.NewScanner(h.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		line := make([]byte, len(raw))
		copy(line, raw)

		// Track I/O bytes (proxy backstop) and any measured token usage (real
		// pressure). Usage appears on assistant + result lines; latest wins.
		if total, ok := extractUsage(line); ok {
			h.mu.Lock()
			h.bytes += int64(len(line))
			h.lastTotalTokens = total
			h.haveUsage = true
			h.mu.Unlock()
		} else {
			h.mu.Lock()
			h.bytes += int64(len(line))
			h.mu.Unlock()
		}

		evs, sess := ParseLine(line, h.id)
		evs = dedup.Apply(evs)
		if sess != "" {
			h.mu.Lock()
			h.claudeSessionID = sess
			h.mu.Unlock()
		}
		for _, e := range evs {
			if e.TS.IsZero() {
				e.TS = time.Now().UTC()
			}
			select {
			case h.events <- e:
			default:
			}
			if e.Kind == eventbus.KindResult || e.Kind == eventbus.KindError {
				h.signalWaiter(e)
			}
		}
	}
}

// signalWaiter delivers a turn-terminating event to a waiting Send, if any.
func (h *handle) signalWaiter(e eventbus.Event) {
	h.mu.Lock()
	w := h.waiter
	h.mu.Unlock()
	if w != nil {
		select {
		case w <- e:
		default:
		}
	}
}

func (h *handle) markDead() {
	h.mu.Lock()
	h.dead = true
	h.status = harness.StatusDead
	h.mu.Unlock()
	h.doneOnce.Do(func() { close(h.done) })
}

// Send writes one user turn to the persistent process stdin and blocks until that
// turn's result (or error) event arrives, the process exits, or ctx is done. It
// does NOT close stdin, so the session continues across turns.
func (a *Adapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	hh := h.(*handle)
	if in.Text == "" {
		return fmt.Errorf("claudecode: streaming mode requires Input.Text (control keys need the PTY transport, not built)")
	}
	hh.sendMu.Lock()
	defer hh.sendMu.Unlock()

	hh.mu.Lock()
	if hh.dead {
		hh.mu.Unlock()
		return fmt.Errorf("claudecode: session %s process is not alive: %w", hh.id, harness.ErrProcessGone)
	}
	waiter := make(chan eventbus.Event, 1)
	hh.waiter = waiter
	hh.status = harness.StatusThinking
	hh.mu.Unlock()

	defer func() {
		hh.mu.Lock()
		hh.waiter = nil
		if hh.status == harness.StatusThinking {
			hh.status = harness.StatusIdle
		}
		hh.mu.Unlock()
	}()

	env := userEnvelope(in.Text)
	if _, err := hh.stdin.Write(append(env, '\n')); err != nil {
		return fmt.Errorf("claudecode: write stdin: %w", err)
	}
	hh.mu.Lock()
	hh.turns++
	hh.bytes += int64(len(env) + 1)
	hh.mu.Unlock()

	select {
	case e := <-waiter:
		if e.Kind == eventbus.KindError {
			// Auth is resolved ONCE, at claude process startup: the CLI loads the
			// OAuth credential store into memory and never re-reads it for the life
			// of the process. So a claude spawned while the store was bad (e.g. the
			// CLI tombstones it -- refreshToken:"" -- after a refresh_token grant
			// comes back invalid_grant) stays broken forever, and every later turn
			// on this session fails even after the store has been repaired by an
			// interactive /login. Observed 2026-08-09: session f7dd41b6f621830e kept
			// returning "Not logged in" from a child spawned one minute before the
			// re-login, while a session started after it answered normally.
			//
			// Marking the handle dead hands the session to the EXISTING dead-process
			// recovery in session.gcSweep/recoverDead, which tears the process down
			// and restarts it fresh from the committed artifacts under the same
			// session id. The replacement re-reads the credential store on startup,
			// so the session self-heals on the next sweep instead of needing a
			// human to notice and start a new session. No token is cached, read, or
			// logged here: the recovery is purely "throw away the stale process".
			//
			// The SAME classifier also decides what the USER is told. The core
			// cannot parse CLI prose and must not grow a second detector that
			// could disagree with this one, so the returned error WRAPS
			// harness.ErrAuth and session.failureNotice keys off errors.Is.
			// One classifier, two consumers.
			if isAuthError(e.Text) {
				log.Printf("session %s: auth failure on turn; marking process dead so it is respawned with fresh credentials", hh.id)
				hh.markDead()
				return fmt.Errorf("claudecode: turn error: %s: %w", e.Text, harness.ErrAuth)
			}
			return fmt.Errorf("claudecode: turn error: %s", e.Text)
		}
		return nil
	case <-hh.done:
		return fmt.Errorf("claudecode: session %s process exited before result: %w", hh.id, harness.ErrProcessGone)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Interrupt cancels the current turn. In streaming -p mode there is no in-band
// cancel wired here, so this signals the process (which ends the session). A
// finer stream-json control-request interrupt is a future refinement.
func (a *Adapter) Interrupt(h harness.Handle) error {
	hh := h.(*handle)
	hh.mu.Lock()
	cmd := hh.cmd
	hh.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

// Teardown closes stdin and terminates the process cleanly.
func (a *Adapter) Teardown(h harness.Handle) error {
	hh := h.(*handle)
	if hh.stdin != nil {
		_ = hh.stdin.Close()
	}
	if hh.procCancel != nil {
		hh.procCancel()
	}
	if hh.cmd != nil {
		_ = hh.cmd.Wait()
	}
	hh.markDead()
	return nil
}

// authErrorMarkers are the claude CLI's credential-layer failure messages. They
// share one property that makes them safe to key on: they are decided BEFORE any
// request leaves the machine, from the credential store the process loaded at
// startup, so they can never be transient/model/rate-limit errors that a retry on
// the same process would clear. Matching is on the CLI's own wording only; e.Text
// is a CLI diagnostic string and never carries token material.
var authErrorMarkers = []string{
	"OAuth session expired and could not be refreshed", // -p mode, OAuthRefreshDeadError
	"Not logged in",                                    // no usable credential in the store
	"Please run /login",                                // interactive-mode phrasing of the same
	"OAuth token has expired",
}

// isAuthError reports whether a turn error came from the CLI's credential layer
// rather than from the model or the transport. Only these justify discarding the
// process: the credential state is fixed at process startup, so the ONLY way a
// session recovers is a fresh process.
func isAuthError(text string) bool {
	for _, m := range authErrorMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// userEnvelope builds the stream-json user message the CLI expects on stdin. This
// is the same envelope shape the Agent SDK writes.
func userEnvelope(text string) []byte {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string    `json:"role"`
		Content []content `json:"content"`
	}
	type user struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}
	b, _ := json.Marshal(user{
		Type:    "user",
		Message: message{Role: "user", Content: []content{{Type: "text", Text: text}}},
	})
	return b
}

// OneShot runs a single headless turn in classic `claude -p` mode and returns the
// parsed normalized events. Used by the `smoke` subcommand only; the real session
// path is the persistent Start/Send above.
func OneShot(ctx context.Context, bin, prompt string, skipPermissions bool) ([]eventbus.Event, error) {
	if bin == "" {
		bin = "claude"
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if skipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: start %s: %w", bin, err)
	}
	var out []eventbus.Event
	dedup := &resultDeduper{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, _ := ParseLine(line, "smoke")
		out = append(out, dedup.Apply(evs)...)
	}
	if err := cmd.Wait(); err != nil {
		return out, fmt.Errorf("claudecode: claude exited: %w", err)
	}
	return out, nil
}

// ---- stream-json parsing (the mapping table, design 3.2 adapter A) ----

// streamMsg is the subset of claude's stream-json line schema we consume.
type streamMsg struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Message   json.RawMessage `json:"message"` // assistant/user message envelope
}

type innerMessage struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"` // "text" | "thinking" | "tool_use"
	Text string `json:"text"`
	Name string `json:"name"` // tool name for tool_use
}

// ParseLine maps one stream-json line to zero or more normalized events. It also
// returns the claude session_id found on the line (for continuity/resume), or ""
// if none. Unknown line types produce no events (forward-compatible). Over a
// persistent session this is called for every line of every turn; each turn ends
// with one `result` event.
//
// Mapping (design 3.2 adapter A):
//   - system/init, rate_limit_event, echoed user  -> no user-facing event
//   - assistant text block            -> KindOutput
//   - assistant tool_use block        -> KindToolCall (Tool=name)
//   - assistant thinking block        -> dropped (internal)
//   - result/success                  -> KindResult (Text=result; NOTE the
//     streaming paths (readLoop/OneShot) then blank that text via resultDeduper
//     when it duplicates the turn's final output event, so the reply reaches
//     channels exactly once)
//   - result with is_error, or type=="error" -> KindError
func ParseLine(line []byte, sessionID string) ([]eventbus.Event, string) {
	var m streamMsg
	if err := json.Unmarshal(line, &m); err != nil {
		// Not JSON we understand: treat as raw output so nothing is silently lost.
		return []eventbus.Event{{
			SessionID: sessionID, Source: eventbus.SourceNative,
			Kind: eventbus.KindOutput, Text: string(line),
		}}, ""
	}

	var evs []eventbus.Event
	switch m.Type {
	case "system", "rate_limit_event", "user":
		// no user-facing event; caller uses returned session_id.

	case "assistant":
		if len(m.Message) > 0 {
			var im innerMessage
			if err := json.Unmarshal(m.Message, &im); err == nil {
				for _, b := range im.Content {
					switch b.Type {
					case "text":
						if b.Text != "" {
							evs = append(evs, eventbus.Event{
								SessionID: sessionID, Source: eventbus.SourceNative,
								Kind: eventbus.KindOutput, Text: b.Text,
							})
						}
					case "tool_use":
						evs = append(evs, eventbus.Event{
							SessionID: sessionID, Source: eventbus.SourceNative,
							Kind: eventbus.KindToolCall, Tool: b.Name,
						})
					case "thinking":
						// internal reasoning; not surfaced.
					}
				}
			}
		}

	case "result":
		if m.IsError {
			evs = append(evs, eventbus.Event{
				SessionID: sessionID, Source: eventbus.SourceNative,
				Kind: eventbus.KindError, Text: m.Result,
			})
		} else {
			evs = append(evs, eventbus.Event{
				SessionID: sessionID, Source: eventbus.SourceNative,
				Kind: eventbus.KindResult, Text: m.Result, Status: string(harness.StatusIdle),
			})
		}

	case "error":
		evs = append(evs, eventbus.Event{
			SessionID: sessionID, Source: eventbus.SourceNative,
			Kind: eventbus.KindError, Text: string(line),
		})
	}
	return evs, m.SessionID
}
