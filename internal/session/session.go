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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"log"

	"github.com/kjhnns/agentd/internal/channel"
	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
	"github.com/kjhnns/agentd/internal/media"
	"github.com/kjhnns/agentd/internal/runlog"
	"github.com/kjhnns/agentd/internal/workspace"
)

// Session is one live conversation bound to a harness handle. Its identity
// (ID/Title/Workspace) is stable ACROSS a context reset; only the underlying
// harness handle (the process with its context window) is replaced.
type Session struct {
	ID        string          `json:"id"`
	Harness   string          `json:"harness"`
	Title     string          `json:"title"`
	Workspace string          `json:"workspace,omitempty"` // workspace name homing this session
	Created   time.Time       `json:"created"`
	handle    harness.Handle  `json:"-"`
	adapter   harness.Adapter `json:"-"`
	ws        *workspace.Workspace
	model     string // captured at create, so a reset re-Starts with the same model
	cwd       string // harness working dir (workspace root, or bare cwd), reused on reset

	// turnMu serializes whole turns for this session: Send, the checkpoint-flush,
	// and the handle swap during a reset all take it, so an inbound that arrives
	// mid-turn QUEUES as the next turn (single-active-session policy) and a reset
	// never races a live turn.
	turnMu sync.Mutex

	mu           sync.Mutex
	status       harness.Status
	running      bool
	turns        int       // turns on the CURRENT handle (reset back to 0 on reset)
	handleStart  time.Time // when the current handle was started (for max_wallclock)
	lastActivity time.Time // last turn boundary (for idle GC)
}

// Status snapshots the session for the API.
type Status struct {
	ID             string         `json:"id"`
	Harness        string         `json:"harness"`
	Title          string         `json:"title"`
	Status         harness.Status `json:"status"`
	AgentOK        bool           `json:"agent_ok"` // distinct from transport_ok (failure class 2)
	Turns          int            `json:"turns"`
	Pressure       float64        `json:"pressure"`        // context pressure 0..1 on the live handle
	PressureSource string         `json:"pressure_source"` // real | proxy
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

	// PermissionMode is the ACP-ready permission policy passed to every new
	// session (overrides SkipPermissions on the adapter when set). Derived from
	// SkipPermissions by the wiring in main unless set explicitly.
	PermissionMode harness.PermissionMode

	// Policy holds the session-lifecycle + context-reset tuning ([session]
	// config): pressure threshold, idle timeout, turn/wallclock backstops, GC
	// cadence, checkpoint-flush bound. See Policy.
	Policy Policy

	// Recap bounds the verbatim thread recap injected into the system prompt at
	// harness-process start, so a cold start (reset, idle reclaim, restart) does
	// not lose the literal wording a back-reference like "re 2" points at. Zero
	// fields fall back to DefaultRecapPolicy. See recap.go.
	Recap RecapPolicy

	// RecapDisabled turns that injection off entirely: the escape hatch for
	// tests and for restoring the pre-recap behaviour.
	RecapDisabled bool

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

	// DefaultCwd/DefaultModel are the harness defaults the serve wiring passes
	// to RouteInbound; kept here too so server-initiated session creation (e.g.
	// Continue on a past session) starts with the same parameters.
	DefaultCwd   string
	DefaultModel string
}

// Policy is the session-lifecycle + context-reset tuning (mirrors config
// [session]). agentd keeps ONE warm session per workspace and, rather than
// compacting, performs a controlled RESET when a threshold trips. A zero field
// disables that trigger (except ContextResetPressure/GCInterval which fall back
// to a default). DefaultPolicy supplies sane values.
type Policy struct {
	ContextResetPressure float64       // reset when pressure crosses this after a turn
	IdleTimeout          time.Duration // idle this long -> checkpoint-flush + reclaim (0 = off)
	MaxTurns             int           // hard backstop: reset after N turns on a handle (0 = off)
	MaxWallclock         time.Duration // hard backstop: reset after this handle age (0 = off)
	GCInterval           time.Duration // sweeper cadence (0 = default 1m)
	CheckpointTimeout    time.Duration // bound on the checkpoint-flush turn (0 = default 120s)
	// TurnTimeout is the QUIET WINDOW for ONE inbound turn (RouteInbound), not a
	// wallclock cap: the turn is abandoned only after this long with no progress
	// event (tool_call / output / result). Progress RESETS it, so a turn that is
	// genuinely working is never reaped for taking a long time. When it does
	// expire the reply is abandoned even if the harness answers a second later,
	// which is exactly how a real research turn silently lost its answer (see
	// TestTurnWithinBudgetDelivers). 0 = default 15m.
	TurnTimeout time.Duration
	// TurnCeiling is the absolute wallclock BACKSTOP on one turn, so making the
	// bound above an inactivity timer cannot make a pathological loop
	// unkillable. It is deliberately far above any real turn (the longest real
	// one observed is 25m) and below MaxWallclock. 0 = default 2h, <0 = off.
	TurnCeiling time.Duration
}

// DefaultPolicy returns the built-in tuning used when none is configured.
func DefaultPolicy() Policy {
	return Policy{
		ContextResetPressure: 0.75,
		IdleTimeout:          30 * time.Minute,
		MaxTurns:             200,
		MaxWallclock:         8 * time.Hour,
		GCInterval:           time.Minute,
		CheckpointTimeout:    120 * time.Second,
		TurnTimeout:          defaultTurnTimeout,
		TurnCeiling:          defaultTurnCeiling,
	}
}

// defaultTurnTimeout is the QUIET window when [session] turn_timeout is unset.
// Deliberately unchanged at 15m even though it stopped being a wallclock cap:
// as an inactivity bound it is ~6.5x the longest silence ever observed inside a
// real working turn (138.6s, 2026-08-10 10:38:09Z to 10:40:28Z), and keeping the
// number means a genuinely hung turn is still reaped no later than it was
// before. Raising it would only slow down reaping the wedged case.
const defaultTurnTimeout = 15 * time.Minute

// defaultTurnCeiling is the absolute per-turn backstop. 2h is far above the
// longest real turn observed (25m: the 2026-08-10 10:25Z booking turn, which
// finished at 10:50:34Z) and far below MaxWallclock (8h, the handle-age reset),
// so it can only ever catch a pathological loop.
const defaultTurnCeiling = 2 * time.Hour

func (p Policy) turnTimeout() time.Duration {
	if p.TurnTimeout > 0 {
		return p.TurnTimeout
	}
	return defaultTurnTimeout
}

// turnCeiling returns the absolute per-turn backstop. A NEGATIVE configured
// value means "no ceiling" (opt out explicitly); zero means "use the default",
// matching how every other Policy knob spells its default.
func (p Policy) turnCeiling() time.Duration {
	if p.TurnCeiling < 0 {
		return 0
	}
	if p.TurnCeiling > 0 {
		return p.TurnCeiling
	}
	return defaultTurnCeiling
}

func (p Policy) checkpointTimeout() time.Duration {
	if p.CheckpointTimeout > 0 {
		return p.CheckpointTimeout
	}
	return 120 * time.Second
}

func (p Policy) gcInterval() time.Duration {
	if p.GCInterval > 0 {
		return p.GCInterval
	}
	return time.Minute
}

// NewManager builds a Session Manager over one harness adapter.
func NewManager(adapter harness.Adapter, bus *eventbus.Bus, log *runlog.Log) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		adapter:  adapter,
		bus:      bus,
		log:      log,
		Policy:   DefaultPolicy(),
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
	// Continuity: a brand-new session id does NOT mean a brand-new conversation.
	// A Telegram thread is one chat to the user and has been carried by dozens of
	// session ids; append the verbatim tail of THIS TITLE's thread so the fresh
	// process can resolve back-references. See recap.go.
	systemPrompt = m.composeInjection(systemPrompt, title)
	h, err := m.adapter.Start(ctx, harness.SessionConfig{
		SessionID:       id,
		Cwd:             cwd,
		Model:           model,
		SystemPrompt:    systemPrompt,
		SkipPermissions: m.SkipPermissions,
		PermissionMode:  m.PermissionMode,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	s := &Session{
		ID:           id,
		Harness:      m.adapter.Name(),
		Title:        title,
		Created:      now,
		handle:       h,
		adapter:      m.adapter,
		ws:           ws,
		model:        model,
		cwd:          cwd,
		status:       harness.StatusIdle,
		handleStart:  now,
		lastActivity: now,
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
	// Pump the adapter's per-session events onto the shared bus + run-log. The
	// handle is passed explicitly so a reset can start a fresh pump for the new
	// handle while the old pump drains and exits on its closed event channel.
	go m.pump(s, h)
	return s, nil
}

// pump forwards ONE handle's harness events to the bus and durable log until
// that handle's event channel closes (teardown). A reset spawns a new pump for
// the replacement handle.
func (m *Manager) pump(s *Session, h harness.Handle) {
	for e := range s.adapter.Events(h) {
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

// FindByWorkspace returns the live session homed in the named workspace, if
// any ("" resolves to the store default). Used by the scheduler to REUSE the
// warm session for a job run (single-active-session per workspace): the job
// turn then serializes behind any in-progress interactive turn via turnMu.
func (m *Manager) FindByWorkspace(name string) (*Session, bool) {
	if name == "" && m.Workspaces != nil {
		name = m.Workspaces.Default
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Workspace == name && s.Workspace != "" {
			return s, true
		}
	}
	return nil, false
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
	cp := s.adapter.Pressure(s.handle)
	return Status{
		ID:             s.ID,
		Harness:        s.Harness,
		Title:          s.Title,
		Status:         s.status,
		AgentOK:        agentOK,
		Turns:          s.turns,
		Pressure:       cp.Fraction,
		PressureSource: string(cp.Source),
	}
}

// SendResult holds the outcome of one turn for a caller (channel/API).
type SendResult struct {
	Result string           // the final result text
	Events []eventbus.Event // all normalized events from the turn
}

// Send routes one user turn to the session's harness and returns the result. It
// takes the per-session turn lock so concurrent inbound serializes as the NEXT
// turn (single-active-session policy) rather than spawning a parallel session.
// After a COMPLETED turn it evaluates the context-reset triggers (pressure /
// max_turns / max_wallclock) at that natural boundary and, if one trips, kicks
// off a controlled reset in the background so the just-produced reply is not
// delayed. It never checks mid-turn.
func (m *Manager) Send(ctx context.Context, id, text string) (*SendResult, error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if m.log != nil {
		_ = m.log.Append("input", map[string]string{"session": id, "text": text})
	}

	s.turnMu.Lock()

	s.mu.Lock()
	s.running = true
	s.status = harness.StatusThinking
	handle := s.handle
	s.mu.Unlock()

	err := s.adapter.Send(ctx, handle, harness.Input{Text: text})

	now := time.Now().UTC()
	s.mu.Lock()
	s.running = false
	s.status = s.adapter.Status(s.handle)
	if err != nil {
		s.status = harness.StatusError
	}
	s.turns++
	s.lastActivity = now
	s.mu.Unlock()

	if err != nil {
		s.turnMu.Unlock()
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

	tripped, reason := m.evalTriggers(s)
	s.turnMu.Unlock()

	if tripped {
		// Background so the reply goes out promptly; Reset re-takes turnMu and
		// serializes against any queued turn. Use a detached context so an
		// ending request context does not abort the reset.
		go func() {
			if rerr := m.Reset(context.Background(), id, reason); rerr != nil {
				log.Printf("session %s: post-turn reset (%s) failed: %v", id, reason, rerr)
			}
		}()
	}
	return &SendResult{}, nil
}

// TurnOptions tunes ONE collected turn (SendAndCollect).
type TurnOptions struct {
	// Timeout is the QUIET window, not a wallclock cap on the turn: the turn is
	// abandoned only after this long with NO progress event from the session.
	// Every tool_call / output / result RESETS it, so a turn that is genuinely
	// working is never reaped no matter how long the work takes. <=0 means "no
	// bound beyond ctx", which is what a caller already running under a bounded
	// context (the scheduler) wants. On expiry the error wraps ErrTurnTimeout.
	//
	// It used to be a hard cap, and that killed real work: 2026-08-10T10:40:28Z
	// a hotel-booking turn was abandoned at exactly 900.1s having emitted 45
	// progress events (38 tool_call, 7 output), its longest quiet stretch being
	// 138.6s. The harness finished the job at 10:50:34Z and the answer had
	// nowhere to go. See TestLongProgressingTurnSurvivesPastTheOldHardCap.
	Timeout time.Duration
	// Ceiling is the absolute wallclock BACKSTOP measured from the start of the
	// turn. It exists so an inactivity timer can never make a pathological loop
	// (one that keeps emitting tool calls forever) unkillable. <=0 means "no
	// ceiling". On expiry the error wraps ErrTurnCeiling (which itself wraps
	// ErrTurnTimeout, so existing errors.Is(ErrTurnTimeout) callers still see a
	// timeout and still map to 504).
	Ceiling time.Duration
	// OnEvent, when set, is called for EVERY bus event belonging to this
	// session while the turn runs. It is the hook for interim feedback (a
	// needs-input glyph on Telegram, a streamed line in the CLI) and must not
	// block: it runs on the collect loop.
	OnEvent func(eventbus.Event)
}

// isProgress reports whether an event is EVIDENCE OF WORK, i.e. whether it may
// reset the quiet timer. Only events the harness emits because it actually did
// something count: a tool invocation, model text, the final result.
//
// status and needs_input deliberately do NOT count. A status heartbeat would
// keep a wedged turn alive forever (that is the exact failure mode an
// inactivity timeout has to avoid), and needs_input means the turn is blocked
// on a human who has no way to answer mid-turn, so it SHOULD age out.
func isProgress(k eventbus.Kind) bool {
	switch k {
	case eventbus.KindOutput, eventbus.KindToolCall, eventbus.KindResult:
		return true
	}
	return false
}

// SendAndCollect sends one user turn to a session and returns the turn's
// USER-VISIBLE result text. It is the single implementation of the
// subscribe-then-send-then-drain dance that every caller of Send needs (the
// channel router, the scheduler runner, the HTTP /input endpoint): subscribing
// BEFORE the send so the result event cannot be missed, mapping a blanked
// result text back onto the turn's last streamed output (the harness blanks a
// result that merely duplicates it, so there is one visible reply), and
// draining briefly after Send returns because bus delivery can lag the call.
//
// The returned text is "" when the turn produced nothing; callers that must
// say SOMETHING substitute their own placeholder.
func (m *Manager) SendAndCollect(ctx context.Context, id, text string, opt TurnOptions) (string, error) {
	if _, ok := m.Get(id); !ok {
		return "", fmt.Errorf("session %s not found", id)
	}

	// Subscribe BEFORE sending so the turn's result event cannot be missed.
	subID, events := m.bus.Subscribe()
	defer m.bus.Unsubscribe(subID)

	turnErr := make(chan error, 1)
	go func() {
		_, err := m.Send(ctx, id, text)
		turnErr <- err
	}()

	started := time.Now()
	var result, lastOutput string
	gotResult := false
	steps := 0
	lastStep := ""
	lastProgress := started

	// The quiet timer. A zero/negative Timeout means "no bound beyond ctx": a
	// nil channel blocks forever in the select, which is exactly that.
	var quiet *time.Timer
	var quietC <-chan time.Time
	if opt.Timeout > 0 {
		quiet = time.NewTimer(opt.Timeout)
		defer quiet.Stop()
		quietC = quiet.C
	}
	// The absolute backstop. Armed once and never reset: that is the point.
	var ceilingC <-chan time.Time
	if opt.Ceiling > 0 {
		ct := time.NewTimer(opt.Ceiling)
		defer ct.Stop()
		ceilingC = ct.C
	}

	collect := func(e eventbus.Event) {
		if e.SessionID != id {
			return
		}
		if opt.OnEvent != nil {
			opt.OnEvent(e)
		}
		// Progress resets the quiet timer. This is the whole fix: work that is
		// visibly happening buys more time; silence does not.
		if quiet != nil && isProgress(e.Kind) {
			if !quiet.Stop() {
				// Already fired: drain so the stale tick cannot end the turn on
				// the next loop iteration even though we just saw progress.
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(opt.Timeout)
		}
		if isProgress(e.Kind) {
			steps++
			lastProgress = time.Now()
			if e.Kind == eventbus.KindToolCall && e.Tool != "" {
				lastStep = "tool " + e.Tool
			} else if e.Kind == eventbus.KindOutput && e.Text != "" {
				lastStep = firstLine(e.Text)
			}
		}
		switch e.Kind {
		case eventbus.KindOutput:
			lastOutput = e.Text
		case eventbus.KindResult:
			gotResult = true
			if e.Text != "" {
				result = e.Text
			} else {
				result = lastOutput
			}
		}
	}

	// abandon builds the timeout error. The PARTIAL work is not thrown away: the
	// last streamed output and a count of completed steps ride along so the user
	// is told what was accomplished instead of getting a bare error.
	abandon := func(ceiling bool) (string, error) {
		te := &TurnTimeoutError{
			SessionID: id,
			Ceiling:   ceiling,
			Elapsed:   time.Since(started),
			Quiet:     time.Since(lastProgress),
			Budget:    opt.Timeout,
			Steps:     steps,
			LastStep:  lastStep,
			Partial:   lastOutput,
		}
		if ceiling {
			te.Budget = opt.Ceiling
		}
		return lastOutput, te
	}

	for {
		select {
		case e := <-events:
			collect(e)
		case err := <-turnErr:
			if err != nil {
				return "", err
			}
			// Send returning can race the bus delivery of the turn's events;
			// drain briefly until the result event lands so the reply is not
			// built from a partial turn.
			grace := time.After(resultGrace)
			for !gotResult {
				select {
				case e := <-events:
					collect(e)
				case <-grace:
					return result, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return result, nil
		case <-quietC:
			return abandon(false)
		case <-ceilingC:
			return abandon(true)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// firstLine returns the first non-empty line of s, truncated, for use in a
// human-facing "last thing I was doing" summary.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if len(ln) > 120 {
			return ln[:117] + "..."
		}
		return ln
	}
	return ""
}

// resultGrace is how long SendAndCollect keeps draining the bus after Send
// returned but before the turn's result event arrived.
const resultGrace = 2 * time.Second

// TurnTimeout exposes the configured per-turn budget (Policy.TurnTimeout, else
// the built-in default) so callers outside this package (the HTTP API, the
// CLI client) bound a turn with the SAME number the channels use.
func (m *Manager) TurnTimeout() time.Duration { return m.Policy.turnTimeout() }

// TurnCeiling exposes the configured absolute per-turn backstop so callers
// outside this package arm the SAME backstop the channels do.
func (m *Manager) TurnCeiling() time.Duration { return m.Policy.turnCeiling() }

// evalTriggers returns whether a context reset should fire after a completed
// turn, and why. Order: hard backstops first (they protect proxy-only harnesses
// and runaway sessions), then the pressure threshold. Caller holds turnMu.
func (m *Manager) evalTriggers(s *Session) (bool, string) {
	p := m.Policy
	s.mu.Lock()
	turns := s.turns
	age := time.Since(s.handleStart)
	handle := s.handle
	s.mu.Unlock()

	if p.MaxTurns > 0 && turns >= p.MaxTurns {
		return true, fmt.Sprintf("max_turns (%d)", turns)
	}
	if p.MaxWallclock > 0 && age >= p.MaxWallclock {
		return true, fmt.Sprintf("max_wallclock (%s)", age.Round(time.Second))
	}
	if p.ContextResetPressure > 0 {
		cp := s.adapter.Pressure(handle)
		if cp.Fraction >= p.ContextResetPressure {
			return true, fmt.Sprintf("pressure %.2f>=%.2f (%s)", cp.Fraction, p.ContextResetPressure, cp.Source)
		}
	}
	return false, ""
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

// RenderInbound builds the canonical turn text from an inbound message's Text
// plus its media artifact references. This is THE session-layer injection point
// for multimodal input (channel-agnostic by design): every channel's artifacts
// render identically, and adapters never pre-bake these markers themselves.
//   - image:    a pointer to the saved file the agent Reads with its own tools
//   - document: the same pointer, annotated with name/mime/size
//   - audio:    the transcript inline (the file is already deleted on success)
func RenderInbound(in channel.InboundMsg) string {
	parts := make([]string, 0, 1+len(in.Media))
	if t := strings.TrimSpace(in.Text); t != "" {
		parts = append(parts, t)
	}
	for _, a := range in.Media {
		switch a.Kind {
		case media.KindImage:
			parts = append(parts, fmt.Sprintf(
				"[The user sent an image saved at %s. Use the Read tool to view it, then respond.]", a.Path))
		case media.KindAudio:
			parts = append(parts, fmt.Sprintf(
				"[voice message, %ds, transcribed]: %s", a.DurationS, a.Transcript))
		default: // document
			parts = append(parts, fmt.Sprintf(
				"[The user sent a document saved at %s (name: %s, type: %s, size: %d bytes). Use the Read tool to view it, then respond.]",
				a.Path, a.Name, a.Mime, a.Size))
		}
	}
	return strings.Join(parts, "\n\n")
}

// ErrTurnTimeout marks a turn that was abandoned on a time bound. Since the
// bound became an INACTIVITY window (Policy.TurnTimeout), this specifically
// means the session went quiet: no tool call, no output, no result for the whole
// window. The harness may still answer later; that answer has nowhere to go,
// which is why the user is told explicitly instead of left with a bare glyph.
var ErrTurnTimeout = errors.New("turn timed out")

// ErrTurnCeiling marks the BACKSTOP firing: the turn was still emitting progress
// but blew the absolute wallclock ceiling (Policy.TurnCeiling). It wraps
// ErrTurnTimeout so callers that only care "was this a timeout" (the HTTP API's
// 504 mapping, the CLI) keep working unchanged, while the user-facing notice can
// say something truthful and different.
var ErrTurnCeiling = fmt.Errorf("turn hit its absolute ceiling: %w", ErrTurnTimeout)

// TurnTimeoutError is the rich form of a turn abandonment. It carries what the
// turn ACCOMPLISHED before it was cut off (step count, last step, last streamed
// output) so the failure notice can report progress instead of discarding it.
type TurnTimeoutError struct {
	SessionID string
	Ceiling   bool          // true = absolute backstop, false = went quiet
	Elapsed   time.Duration // wallclock since the turn started
	Quiet     time.Duration // since the last progress event
	Budget    time.Duration // the bound that fired
	Steps     int           // progress events seen (tool calls + output + result)
	LastStep  string        // human summary of the last thing it did
	Partial   string        // last streamed output, the salvageable work
}

func (e *TurnTimeoutError) Error() string {
	// Both forms lead with "timed out after <the CONFIGURED bound>" so an
	// operator reads the knob that fired, not a jittery measured value.
	if e.Ceiling {
		return fmt.Sprintf("session %s turn timed out after %s: hit the absolute ceiling while still working (ran %s, %d steps): %v",
			e.SessionID, dur(e.Budget), dur(e.Elapsed), e.Steps, ErrTurnCeiling)
	}
	return fmt.Sprintf("session %s turn timed out after %s with no progress (ran %s, %d steps): %v",
		e.SessionID, dur(e.Budget), dur(e.Elapsed), e.Steps, ErrTurnTimeout)
}

// dur renders a duration for humans without rounding a sub-second budget away to
// "0s", which made the configured budget unreadable in the error text.
func dur(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// Unwrap routes errors.Is to the right sentinel. Ceiling unwraps to
// ErrTurnCeiling, which in turn unwraps to ErrTurnTimeout, so a ceiling error
// IS a timeout for every existing caller while still being distinguishable.
func (e *TurnTimeoutError) Unwrap() error {
	if e.Ceiling {
		return ErrTurnCeiling
	}
	return ErrTurnTimeout
}

// failureNotice renders the short plain-text notice a user gets when their turn
// failed and no reply was produced. Plain text on purpose: Telegram renders
// markdown asterisks literally, and it is read on a phone. It names the INPUT so
// a failed voice note reads as a failed voice note rather than a generic error.
//
// The branches are a TAXONOMY, ordered so they are mutually exclusive in
// practice and so the most actionable cause always wins. A user who cannot tell
// a dead login from a timeout from a restart cannot fix any of them, which is
// its own bug: on 2026-08-09 an expired Claude OAuth credential surfaced as
// "Sorry, I could not process that voice message: claudecode: turn error:
// Failed to authenticate: OAuth session expired and could not be refreshed",
// which buries the one fact that mattered and the one action that fixes it.
//
// Order matters. Auth is checked FIRST so a credential failure can never be
// reported as anything else; ceiling before quiet-timeout because a ceiling
// error also satisfies errors.Is(err, ErrTurnTimeout) by design.
func failureNotice(in channel.InboundMsg, err error) string {
	what := "that message"
	if len(in.Media) > 0 {
		switch in.Media[0].Kind {
		case media.KindAudio:
			what = "that voice message"
		case media.KindImage:
			what = "that photo"
		default:
			what = "that file"
		}
	}

	switch {
	// 1. The Claude login died. Nothing agentd, Telegram or the network can do
	// about it, and only an interactive login clears it (the CLI tombstones the
	// shared credential on an invalid_grant refresh). Say all three things: what
	// broke, what to do, and that the request was NOT processed.
	case errors.Is(err, harness.ErrAuth):
		return "Your Claude login has expired, so I could not run " + what + ". " +
			"This is the Claude login, not agentd or Telegram. " +
			"Run /login in a terminal, then send it again."

	// 2. Cancelled: agentd was shut down or restarted while the turn was in
	// flight, or the caller went away. Nothing was wrong with the request.
	case errors.Is(err, context.Canceled):
		return "I was stopped while working on " + what + ", so it did not finish. " +
			"Nothing was wrong with the request. Send it again."

	// 3. The absolute backstop fired: it WAS still working, just far too long.
	case errors.Is(err, ErrTurnCeiling):
		return withPartial("I worked on "+what+" for "+timeoutDetail(err)+
			" and hit my hard limit, so I stopped it. It was still running, not stuck. "+
			"Send it again in smaller pieces.", err)

	// 4. Went quiet: no tool call, no output, nothing, for the whole window.
	// This is the "genuinely hung" case the inactivity timer exists to catch.
	case errors.Is(err, ErrTurnTimeout):
		return withPartial("I stopped working on "+what+" because it went silent "+timeoutDetail(err)+
			". That usually means it got stuck rather than that it was slow. Send it again.", err)

	// 5. The agent process died or was restarted under the turn.
	case errors.Is(err, harness.ErrProcessGone):
		return "My agent process died while handling " + what + ", so it did not finish. " +
			"It restarts itself. Send it again in a moment."
	}

	return fmt.Sprintf("Sorry, I could not process %s: %v", what, err)
}

// withPartial appends the salvageable work to a timeout notice. An abandoned
// turn has usually produced real output already; throwing it away and returning
// a bare error wastes it and leaves the user with nothing to act on.
func withPartial(msg string, err error) string {
	var te *TurnTimeoutError
	if !errors.As(err, &te) {
		return msg
	}
	p := strings.TrimSpace(te.Partial)
	if p == "" {
		return msg
	}
	if len(p) > partialNoticeMax {
		p = p[:partialNoticeMax] + "..."
	}
	return msg + "\n\nHere is what I had so far:\n" + p
}

// partialNoticeMax caps the salvaged partial so the notice stays readable on a
// phone; the full output is still in the run log.
const partialNoticeMax = 1200

// timeoutDetail renders the progress a timed-out turn made, so the notice
// reports what was accomplished rather than discarding it silently.
func timeoutDetail(err error) string {
	var te *TurnTimeoutError
	if !errors.As(err, &te) {
		return "for too long"
	}
	if te.Ceiling {
		s := dur(te.Elapsed)
		if te.Steps > 0 {
			s += " and " + fmt.Sprintf("%d", te.Steps) + " steps"
		}
		return s
	}
	s := "for " + dur(te.Quiet)
	if te.Steps > 0 {
		s += " after " + fmt.Sprintf("%d", te.Steps) + " steps"
		if te.LastStep != "" {
			s += " (last: " + te.LastStep + ")"
		}
	}
	return s
}

// RouteInbound wires a channel inbound message to a session and sends the harness
// result back through the channel. It reuses one session per chat id (created on
// first message). This is the end-to-end path.
func (m *Manager) RouteInbound(ctx context.Context, ch channel.Adapter, in channel.InboundMsg, sessionForChat map[string]string, cwd, model string) (rerr error) {
	if m.log != nil {
		_ = m.log.Append("inbound", in)
	}

	// Silence is never an outcome. Every error exit below leaves the user with
	// nothing but the 😱 glyph unless we say what went wrong, so a deferred
	// notice sends one short plain-text line whenever the turn failed AND no
	// reply was delivered. Detached ctx: the notice must still go out when the
	// turn died because its own context was cancelled.
	sessionID := ""
	replied := false
	defer func() {
		if rerr == nil || replied {
			return
		}
		text := failureNotice(in, rerr)
		nctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := ch.Send(nctx, channel.OutboundMsg{ChatID: in.UserID, Text: text}); err != nil {
			log.Printf("session: failure notice to %s/%s failed: %v", in.Channel, in.UserID, err)
			return
		}
		if m.log != nil {
			_ = m.log.Append("outbound", map[string]string{
				"session": sessionID, "chat": in.UserID, "text": text, "kind": "failure-notice",
			})
		}
	}()

	// Emoji progress feedback on the user's own message. Best-effort by
	// contract: an adapter that cannot react no-ops, and a reaction failure
	// never changes the turn's outcome. The receipt glyph (👀) is set by the
	// adapter at receipt; this function owns the rest of the chain.
	react := func(emoji string) {
		if in.MsgID == "" || emoji == "" {
			return
		}
		if err := ch.Ack(in.UserID, in.MsgID, emoji); err != nil {
			log.Printf("session: reaction %q on %s/%s failed: %v", emoji, in.Channel, in.MsgID, err)
		}
	}

	id, ok := sessionForChat[in.UserID]
	if ok {
		// A session the idle-GC reclaimed (process freed) no longer exists; the
		// next inbound lazily re-hydrates a FRESH one from the workspace
		// artifacts (context.md + memory), which is exactly the reclaim contract.
		if _, alive := m.Get(id); !alive {
			ok = false
		}
	}
	if !ok {
		s, err := m.Create(ctx, "", cwd, model, "telegram:"+in.UserID)
		if err != nil {
			react(channel.ReactionError)
			return err
		}
		id = s.ID
		sessionForChat[in.UserID] = id
	}
	sessionID = id

	// A session has the prompt: the turn is in flight. Every exit below flips
	// this to the done or error glyph via the deferred final reaction.
	react(channel.ReactionWorking)
	finalReaction := channel.ReactionError
	defer func() { react(finalReaction) }()

	// One turn, collected by the shared SendAndCollect: the needs-input glyph is
	// the only channel-specific piece, delivered through the event hook.
	result, err := m.SendAndCollect(ctx, id, RenderInbound(in), TurnOptions{
		Timeout: m.Policy.turnTimeout(),
		Ceiling: m.Policy.turnCeiling(),
		OnEvent: func(e eventbus.Event) {
			if e.Kind == eventbus.KindNeedsInput {
				// Interim state: the agent is blocked on the user. The terminal
				// done/error glyph still replaces it when the turn resolves.
				react(channel.ReactionNeedsInput)
			}
		},
	})
	if err != nil {
		return err
	}
	if result == "" {
		result = "(no result)"
	}
	rcpt, err := ch.Send(ctx, channel.OutboundMsg{ChatID: in.UserID, Text: result})
	if err != nil {
		return err
	}
	replied = true
	if m.log != nil {
		_ = m.log.Append("outbound", map[string]string{"session": id, "chat": in.UserID, "msg_id": rcpt.ID, "text": result})
	}
	finalReaction = channel.ReactionDone
	return nil
}
