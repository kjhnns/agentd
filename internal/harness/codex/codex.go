// Package codex runs Codex's JSONL exec interface, resuming the exact saved
// thread on subsequent turns. Processes are per-turn; conversation state is not.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

type Adapter struct{ bin string }

var _ harness.Adapter = (*Adapter)(nil)

func New(bin string) *Adapter {
	if bin == "" {
		bin = "codex"
	}
	return &Adapter{bin: bin}
}
func (*Adapter) Name() string { return "codex" }
func (*Adapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{StructuredEvents: true, Interrupt: true, Resume: true}
}

type handle struct {
	cfg     harness.SessionConfig
	mu      sync.Mutex
	sendMu  sync.Mutex
	events  chan eventbus.Event
	closed  bool
	done    chan struct{}
	cancel  context.CancelFunc
	thread  string
	status  harness.Status
	started time.Time
	turns   int
	bytes   int64
}

func (h *handle) ID() string { return h.cfg.SessionID }
func (a *Adapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("codex: session id required")
	}
	if _, err := exec.LookPath(a.bin); err != nil {
		return nil, fmt.Errorf("codex: executable: %w", err)
	}
	return &handle{cfg: cfg, events: make(chan eventbus.Event, 256), done: make(chan struct{}), status: harness.StatusIdle, started: time.Now()}, nil
}
func (a *Adapter) Attach(ctx context.Context, existing string) (harness.Handle, error) {
	h, err := a.Start(ctx, harness.SessionConfig{SessionID: existing})
	if err == nil {
		h.(*handle).thread = existing
	}
	return h, err
}
func (*Adapter) Events(h harness.Handle) <-chan eventbus.Event { return h.(*handle).events }
func (*Adapter) Status(h harness.Handle) harness.Status {
	hh := h.(*handle)
	hh.mu.Lock()
	defer hh.mu.Unlock()
	return hh.status
}
func (*Adapter) Pressure(h harness.Handle) harness.ContextPressure {
	hh := h.(*handle)
	hh.mu.Lock()
	defer hh.mu.Unlock()
	// exec usage is accumulated across model calls, not current context occupancy.
	return harness.DefaultProxyBudget().Pressure(hh.turns, hh.bytes, time.Since(hh.started))
}
func (*Adapter) Interrupt(h harness.Handle) error {
	hh := h.(*handle)
	hh.mu.Lock()
	defer hh.mu.Unlock()
	if hh.cancel != nil {
		hh.cancel()
	}
	return nil
}
func (*Adapter) Teardown(h harness.Handle) error {
	hh := h.(*handle)
	hh.mu.Lock()
	if hh.status != harness.StatusDead {
		hh.status = harness.StatusDead
		close(hh.done)
	}
	if hh.cancel != nil {
		hh.cancel()
	}
	hh.mu.Unlock()
	// Only Send publishes. Joining it prevents sends racing channel closure.
	hh.sendMu.Lock()
	defer hh.sendMu.Unlock()
	hh.mu.Lock()
	defer hh.mu.Unlock()
	if !hh.closed {
		close(hh.events)
		hh.closed = true
	}
	return nil
}
func buildArgs(cfg harness.SessionConfig, thread string) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check", "-c", "approval_policy=\"never\""}
	skip := cfg.SkipPermissions
	if cfg.PermissionMode != "" {
		skip = cfg.PermissionMode == harness.PermissionSkip
	}
	if skip {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		// exec has no interactive approval transport. Keep it read-only and never
		// inherit a user's unrestricted sandbox configuration by accident.
		args = append(args, "--sandbox", "read-only")
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.SystemPrompt != "" {
		v, _ := json.Marshal(cfg.SystemPrompt)
		args = append(args, "-c", "developer_instructions="+string(v))
	}
	if thread != "" {
		args = append(args, "resume", thread)
	}
	return append(args, "-") // user text goes over stdin, never argv or a shell
}
func (h *handle) emit(ctx context.Context, e eventbus.Event) {
	e.SessionID = h.ID()
	e.TS = time.Now().UTC()
	e.Source = eventbus.SourceNative
	select {
	case h.events <- e:
	case <-ctx.Done():
	case <-h.done:
	}
}

// tailBuffer bounds diagnostics even when a failing CLI writes indefinitely.
type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = append([]byte(nil), b.data[len(b.data)-8192:]...)
	}
	return n, nil
}
func (a *Adapter) Send(ctx context.Context, opaque harness.Handle, in harness.Input) (err error) {
	h := opaque.(*handle)
	if in.Key == "ctrl-c" {
		return a.Interrupt(h)
	}
	if in.Key != "" {
		return fmt.Errorf("codex: interactive key %q unsupported", in.Key)
	}
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	h.mu.Lock()
	if h.status == harness.StatusDead {
		h.mu.Unlock()
		return fmt.Errorf("codex: %w", harness.ErrProcessGone)
	}
	runCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.status = harness.StatusThinking
	thread := h.thread
	h.bytes += int64(len(in.Text))
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		h.cancel = nil
		if h.status != harness.StatusDead {
			if err != nil {
				h.status = harness.StatusError
			} else {
				h.status = harness.StatusIdle
				h.turns++
			}
		}
	}()
	cmd := exec.CommandContext(runCtx, a.bin, buildArgs(h.cfg, thread)...)
	cmd.Dir = h.cfg.Cwd
	cmd.Stdin = strings.NewReader(in.Text)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return fmt.Errorf("codex: start: %w", e)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	complete := false
	failure := ""
	for sc.Scan() {
		raw := sc.Bytes()
		var wire wireEvent
		if e = json.Unmarshal(raw, &wire); e != nil {
			failure = "invalid JSON event: " + e.Error()
			cancel()
			break
		}
		h.mu.Lock()
		h.bytes += int64(len(raw))
		if wire.ThreadID != "" {
			h.thread = wire.ThreadID
		}
		h.mu.Unlock()
		switch wire.Type {
		case "turn.completed":
			complete = true
		case "turn.failed":
			failure = wire.Error.Message
			if failure == "" {
				failure = "turn failed"
			}
		default:
			for _, ev := range wire.events() {
				h.emit(runCtx, ev)
			}
		}
	}
	if e = sc.Err(); e != nil {
		failure = "read stream: " + e.Error()
		cancel()
	}
	waitErr := cmd.Wait()
	if runCtx.Err() != nil && failure == "" {
		return runCtx.Err()
	}
	if failure == "" && waitErr != nil {
		failure = fmt.Sprintf("process exited: %v: %s", waitErr, strings.TrimSpace(string(stderr.data)))
	}
	h.mu.Lock()
	hasThread := h.thread != ""
	h.mu.Unlock()
	if failure == "" && (!complete || !hasThread) {
		failure = "process ended without a completed turn and thread id"
	}
	if failure != "" {
		h.emit(ctx, eventbus.Event{Kind: eventbus.KindError, Text: failure, Status: "error"})
		return fmt.Errorf("codex: %s", failure)
	}
	h.emit(ctx, eventbus.Event{Kind: eventbus.KindResult, Status: "idle"})
	return nil
}

type wireEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Item struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
		Tool    string `json:"tool"`
		Status  string `json:"status"`
	} `json:"item"`
}

func (w wireEvent) events() []eventbus.Event {
	e := eventbus.Event{}
	switch w.Type {
	case "turn.started":
		e.Kind = eventbus.KindStatus
		e.Status = "thinking"
	case "error":
		e.Kind = eventbus.KindStatus
		e.Status = "thinking"
		e.Text = w.Message // may be a retry notification
	case "item.started", "item.updated", "item.completed":
		switch w.Item.Type {
		case "agent_message":
			if w.Type != "item.completed" {
				return nil
			}
			e.Kind = eventbus.KindOutput
			e.Text = w.Item.Text
		case "command_execution", "mcp_tool_call", "file_change", "web_search", "collab_tool_call":
			e.Kind = eventbus.KindToolCall
			e.Tool = w.Item.Type
			e.Text = w.Item.Command
			if w.Item.Tool != "" {
				e.Tool = w.Item.Tool
			}
		default:
			return nil
		}
	default:
		return nil
	}
	return []eventbus.Event{e}
}
