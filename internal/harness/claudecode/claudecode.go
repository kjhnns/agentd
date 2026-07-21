// Package claudecode implements the HarnessAdapter for Claude Code (design 3.2,
// concrete adapter A) in its CLAUDE-CODE-NATIVE mode: it drives
//
//	claude -p --output-format stream-json --verbose [--resume <id>]
//
// and parses the structured JSON events off stdout into normalized
// eventbus.Events. This is the "it works" vertical: SendPrompt below sends a
// prompt to headless claude and returns the parsed response as normalized
// events. Turn-to-turn continuity is carried with --resume using the session_id
// claude reports.
//
// A raw PTY transport (design 3.2's universal fallback) is deliberately NOT built
// here; it is a documented future option. This adapter is native-only.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// Adapter drives the claude CLI headless.
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

// handle is the per-session state. Each user turn spawns one `claude -p` process.
type handle struct {
	id  string
	cfg harness.SessionConfig
	bin string

	events chan eventbus.Event

	mu              sync.Mutex
	claudeSessionID string // captured from stream to --resume the next turn
	status          harness.Status
	cur             *exec.Cmd // the in-flight turn's process (for Interrupt)
}

func (h *handle) ID() string { return h.id }

func (a *Adapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("claudecode: SessionConfig.SessionID required")
	}
	return &handle{
		id:     cfg.SessionID,
		cfg:    cfg,
		bin:    a.bin,
		events: make(chan eventbus.Event, 256),
		status: harness.StatusIdle,
	}, nil
}

// Attach re-binds by claude session id. In native -p mode there is no live
// process to re-attach to; we restore the resume id so the next Send continues
// the prior claude conversation.
func (a *Adapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	return &handle{
		id:              existing,
		bin:             a.bin,
		events:          make(chan eventbus.Event, 256),
		status:          harness.StatusIdle,
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

func (a *Adapter) Interrupt(h harness.Handle) error {
	hh := h.(*handle)
	hh.mu.Lock()
	cur := hh.cur
	hh.mu.Unlock()
	if cur == nil || cur.Process == nil {
		return nil
	}
	return cur.Process.Kill()
}

func (a *Adapter) Teardown(h harness.Handle) error {
	hh := h.(*handle)
	_ = a.Interrupt(h)
	hh.mu.Lock()
	defer hh.mu.Unlock()
	if hh.status != harness.StatusDead {
		hh.status = harness.StatusDead
		// events channel is closed by the last Send; if no turn ran, close now.
		select {
		case <-hh.events:
		default:
		}
	}
	return nil
}

// Send runs one turn. It spawns claude, streams stdout, publishes normalized
// events to the handle's events channel, and returns when the turn completes.
// A control key (in.Key) is not meaningful in one-shot -p mode and is rejected.
func (a *Adapter) Send(ctx context.Context, h harness.Handle, in harness.Input) error {
	hh := h.(*handle)
	if in.Text == "" {
		return fmt.Errorf("claudecode: native -p mode requires Input.Text (control keys need the PTY transport, not built)")
	}
	_, err := a.runTurn(ctx, hh, in.Text)
	return err
}

// SendPrompt is the directly-testable vertical: it runs one headless turn and
// returns the normalized events it produced (also publishing them to the handle
// stream). Used by Send and by tests/smoke commands.
func (a *Adapter) SendPrompt(ctx context.Context, h harness.Handle, prompt string) ([]eventbus.Event, error) {
	return a.runTurn(ctx, h.(*handle), prompt)
}

func (a *Adapter) runTurn(ctx context.Context, hh *handle, prompt string) ([]eventbus.Event, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	hh.mu.Lock()
	if hh.claudeSessionID != "" {
		args = append(args, "--resume", hh.claudeSessionID)
	}
	if hh.cfg.Model != "" {
		args = append(args, "--model", hh.cfg.Model)
	}
	if hh.cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", hh.cfg.SystemPrompt)
	}
	hh.mu.Unlock()
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, hh.bin, args...)
	if hh.cfg.Cwd != "" {
		cmd.Dir = hh.cfg.Cwd
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // stderr is claude's own diagnostics; not part of the event stream

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: start %s: %w", hh.bin, err)
	}
	hh.mu.Lock()
	hh.cur = cmd
	hh.status = harness.StatusThinking
	hh.mu.Unlock()

	var out []eventbus.Event
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		evs, sessID := ParseLine(line, hh.id)
		if sessID != "" {
			hh.mu.Lock()
			hh.claudeSessionID = sessID
			hh.mu.Unlock()
		}
		for _, e := range evs {
			if e.TS.IsZero() {
				e.TS = time.Now().UTC()
			}
			out = append(out, e)
			select {
			case hh.events <- e:
			default:
			}
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()

	hh.mu.Lock()
	hh.cur = nil
	if hh.status != harness.StatusDead {
		hh.status = harness.StatusIdle
	}
	hh.mu.Unlock()

	if scanErr != nil {
		return out, fmt.Errorf("claudecode: read stream: %w", scanErr)
	}
	if waitErr != nil {
		// Surface a synthesized error event so consumers see a normalized failure.
		e := eventbus.Event{SessionID: hh.id, TS: time.Now().UTC(), Source: eventbus.SourceNative,
			Kind: eventbus.KindError, Text: waitErr.Error()}
		out = append(out, e)
		select {
		case hh.events <- e:
		default:
		}
		return out, fmt.Errorf("claudecode: claude exited: %w", waitErr)
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
// returns the claude session_id found on the line (for --resume continuity), or
// "" if none. Unknown line types produce no events (forward-compatible).
//
// Mapping (design 3.2 adapter A):
//   - system/init, rate_limit_event   -> no user-facing event (session_id captured)
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
	case "system", "rate_limit_event":
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
