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
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// Adapter drives the claude CLI as a persistent streaming session.
type Adapter struct {
	bin string // path/name of the claude binary
}

// New returns an adapter using the given binary name (default "claude").
func New(bin string) *Adapter {
	if bin == "" {
		bin = "claude"
	}
	return &Adapter{bin: bin}
}

func (a *Adapter) Name() string { return "claude-code" }

func (a *Adapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{StructuredEvents: true, Interrupt: true, Resume: true}
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
		id:         cfg.SessionID,
		bin:        a.bin,
		cfg:        cfg,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		events:     make(chan eventbus.Event, 256),
		procCancel: cancel,
		done:       make(chan struct{}),
		status:     harness.StatusIdle,
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
	if cfg.SkipPermissions {
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

// readLoop consumes the stdout stream for the whole process lifetime, publishing
// normalized events and signaling any in-flight turn's waiter on result/error.
func (h *handle) readLoop() {
	defer close(h.events)
	defer h.markDead()

	sc := bufio.NewScanner(h.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		line := make([]byte, len(raw))
		copy(line, raw)

		evs, sess := ParseLine(line, h.id)
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
		return fmt.Errorf("claudecode: session %s process is not alive", hh.id)
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

	select {
	case e := <-waiter:
		if e.Kind == eventbus.KindError {
			return fmt.Errorf("claudecode: turn error: %s", e.Text)
		}
		return nil
	case <-hh.done:
		return fmt.Errorf("claudecode: session %s process exited before result", hh.id)
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
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, _ := ParseLine(line, "smoke")
		out = append(out, evs...)
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
//   - result/success                  -> KindResult (Text=result)
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
