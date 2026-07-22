// Package session is the Session Manager (design 3.6). It holds N sessions, each
// bound to ONE harness adapter instance. It routes an inbound channel message to
// a session's harness, streams the resulting events onto the shared event bus and
// the run-log, and sends the final result back out through the channel. This is
// the one wired end-to-end path: Telegram inbound (allowlisted) -> Claude Code
// turn -> reply text back to Telegram.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"log"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/runlog"
	"github.com/kjhnns/agentd/internal/workspace"
)

// Session is one live conversation bound to a harness handle.
type Session struct {
	ID        string          `json:"id"`
	Harness   string          `json:"harness"`
	Title     string          `json:"title"`
	Workspace string          `json:"workspace,omitempty"` // workspace name homing this session
	Created   time.Time       `json:"created"`
	handle    harness.Handle  `json:"-"`
	adapter   harness.Adapter `json:"-"`
	ws        *workspace.Workspace

	mu      sync.Mutex
	status  harness.Status
	running bool
}

// Status snapshots the session for the API.
type Status struct {
	ID      string         `json:"id"`
	Harness string         `json:"harness"`
	Title   string         `json:"title"`
	Status  harness.Status `json:"status"`
	AgentOK bool           `json:"agent_ok"` // distinct from transport_ok (failure class 2)
}

// Manager owns all sessions plus the shared bus and run-log.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	adapter  harness.Adapter // single harness kind in this scaffold
	bus      *eventbus.Bus
	log      *runlog.Log

	// SkipPermissions is the harness default applied to new sessions (design:
	// run hands-free; agentd's own confirm-gate is the intended safety layer).
	SkipPermissions bool

	// Workspaces homes sessions in named workspaces (design 3.5). When set,
	// Create resolves the requested (or default) workspace, runs the harness
	// with cwd = the workspace root, and injects a composed system prompt:
	// instructions/AGENTS.md + memory/INDEX.md + context.md ("index-in,
	// pages-on-demand": the memory corpus itself is never injected; the agent
	// opens memory/pages/<slug>.md on demand with its file tools). When nil,
	// sessions fall back to the caller-provided cwd with no injection.
	Workspaces *workspace.Store

	// GitAutoCommit commits workspace changes after each completed turn (see
	// workspace/git.go for the commit policy). Config [workspace] git_autocommit.
	GitAutoCommit bool
}

// NewManager builds a Session Manager over one harness adapter.
func NewManager(adapter harness.Adapter, bus *eventbus.Bus, log *runlog.Log) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		adapter:  adapter,
		bus:      bus,
		log:      log,
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create starts a new session bound to the manager's harness adapter.
// workspaceName selects the workspace homing the session ("" = the store's
// default); when the manager has a workspace store, the harness cwd IS the
// workspace root and SessionConfig.SystemPrompt carries the composed
// injection (instructions + memory index + handoff). The cwd argument is only
// honored when no workspace store is configured (legacy/bare mode).
func (m *Manager) Create(ctx context.Context, workspaceName, cwd, model, title string) (*Session, error) {
	id := newID()
	var ws *workspace.Workspace
	systemPrompt := ""
	if m.Workspaces != nil {
		var err error
		ws, err = m.Workspaces.Ensure(workspaceName)
		if err != nil {
			return nil, fmt.Errorf("session: workspace %q: %w", workspaceName, err)
		}
		cwd = ws.Cwd()
		systemPrompt, err = ws.ComposeSystemPrompt()
		if err != nil {
			return nil, fmt.Errorf("session: compose injection for workspace %q: %w", ws.Name, err)
		}
	} else if workspaceName != "" {
		return nil, fmt.Errorf("session: workspace %q requested but no workspace store configured", workspaceName)
	}
	h, err := m.adapter.Start(ctx, harness.SessionConfig{
		SessionID:       id,
		Cwd:             cwd,
		Model:           model,
		SystemPrompt:    systemPrompt,
		SkipPermissions: m.SkipPermissions,
	})
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:      id,
		Harness: m.adapter.Name(),
		Title:   title,
		Created: time.Now().UTC(),
		handle:  h,
		adapter: m.adapter,
		ws:      ws,
		status:  harness.StatusIdle,
	}
	if ws != nil {
		s.Workspace = ws.Name
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	if m.log != nil {
		_ = m.log.Append("session_create", map[string]string{"id": id, "harness": s.Harness, "title": title, "workspace": s.Workspace})
	}
	// Pump the adapter's per-session events onto the shared bus + run-log.
	go m.pump(s)
	return s, nil
}

// pump forwards a session's harness events to the bus and durable log.
func (m *Manager) pump(s *Session) {
	for e := range s.adapter.Events(s.handle) {
		m.bus.Publish(e)
		if m.log != nil {
			_ = m.log.Append("event", e)
		}
	}
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List returns a status snapshot of every session.
func (m *Manager) List() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.statusSnapshot())
	}
	return out
}

func (s *Session) statusSnapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	agentOK := s.status != harness.StatusError && s.status != harness.StatusDead
	return Status{ID: s.ID, Harness: s.Harness, Title: s.Title, Status: s.status, AgentOK: agentOK}
}

// SendResult holds the outcome of one turn for a caller (channel/API).
type SendResult struct {
	Result string            // the final result text
	Events []eventbus.Event  // all normalized events from the turn
}

// Send routes one user turn to the session's harness and returns the result. It
// runs synchronously (the turn) and records inbound to the run-log.
func (m *Manager) Send(ctx context.Context, id, text string) (*SendResult, error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if m.log != nil {
		_ = m.log.Append("input", map[string]string{"session": id, "text": text})
	}
	s.mu.Lock()
	s.running = true
	s.status = harness.StatusThinking
	s.mu.Unlock()

	err := s.adapter.Send(ctx, s.handle, harness.Input{Text: text})

	s.mu.Lock()
	s.running = false
	s.status = s.adapter.Status(s.handle)
	if err != nil {
		s.status = harness.StatusError
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// End-of-turn workspace commit: if the turn changed anything under the
	// workspace (memory re-syntheses, context.md handoff, working files),
	// commit it now so history accrues at natural boundaries. Best-effort:
	// autocommit failure is logged, never fails the turn.
	if m.GitAutoCommit && s.ws != nil {
		sha, cerr := s.ws.AutoCommit("turn " + id)
		if cerr != nil {
			log.Printf("session %s: workspace autocommit failed: %v", id, cerr)
		} else if sha != "" && m.log != nil {
			_ = m.log.Append("workspace_commit", map[string]string{"session": id, "workspace": s.ws.Name, "commit": sha})
		}
	}
	return &SendResult{}, nil
}

// Interrupt cancels the current turn.
func (m *Manager) Interrupt(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	if m.log != nil {
		_ = m.log.Append("interrupt", map[string]string{"session": id})
	}
	return s.adapter.Interrupt(s.handle)
}

// Teardown stops and removes a session.
func (m *Manager) Teardown(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	err := s.adapter.Teardown(s.handle)
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	if m.log != nil {
		_ = m.log.Append("session_teardown", map[string]string{"session": id})
	}
	return err
}

// RouteInbound wires a channel inbound message to a session and sends the harness
// result back through the channel. It reuses one session per chat id (created on
// first message). This is the end-to-end path.
func (m *Manager) RouteInbound(ctx context.Context, ch channel.Adapter, in channel.InboundMsg, sessionForChat map[string]string, cwd, model string) error {
	if m.log != nil {
		_ = m.log.Append("inbound", in)
	}
	id, ok := sessionForChat[in.UserID]
	if !ok {
		s, err := m.Create(ctx, "", cwd, model, "telegram:"+in.UserID)
		if err != nil {
			return err
		}
		id = s.ID
		sessionForChat[in.UserID] = id
	}

	// Collect the result by subscribing to the bus for this session's result event.
	subID, events := m.bus.Subscribe()
	defer m.bus.Unsubscribe(subID)

	turnErr := make(chan error, 1)
	go func() {
		_, err := m.Send(ctx, id, in.Text)
		turnErr <- err
	}()

	var result string
	timeout := time.After(150 * time.Second)
collect:
	for {
		select {
		case e := <-events:
			if e.SessionID == id && e.Kind == eventbus.KindResult {
				result = e.Text
			}
		case err := <-turnErr:
			if err != nil {
				return err
			}
			break collect
		case <-timeout:
			return fmt.Errorf("session %s turn timed out", id)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if result == "" {
		result = "(no result)"
	}
	rcpt, err := ch.Send(ctx, channel.OutboundMsg{ChatID: in.UserID, Text: result})
	if err != nil {
		return err
	}
	if m.log != nil {
		_ = m.log.Append("outbound", map[string]string{"session": id, "chat": in.UserID, "msg_id": rcpt.ID, "text": result})
	}
	return nil
}
