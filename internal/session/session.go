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
	// TurnTimeout bounds ONE inbound turn (RouteInbound). It must be generous:
	// when it expires the reply is abandoned even if the harness answers a
	// second later, which is exactly how a real research turn silently lost its
	// answer (see TestTurnWithinBudgetDelivers). 0 = default 15m, matching the
	// scheduler's per-job bound.
	TurnTimeout time.Duration
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
	}
}

// defaultTurnTimeout bounds one inbound turn when [session] turn_timeout is
// unset. 15m matches the scheduler's per-job bound; the previous hardcoded 150s
// was shorter than an ordinary web-research turn and silently discarded the
// answer when it expired.
const defaultTurnTimeout = 15 * time.Minute

func (p Policy) turnTimeout() time.Duration {
	if p.TurnTimeout > 0 {
		return p.TurnTimeout
	}
	return defaultTurnTimeout
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

// ErrTurnTimeout marks a turn that was abandoned because it outran the
// configured budget (Policy.TurnTimeout). The harness may well answer a moment
// later; that answer has nowhere to go, which is why the budget is generous and
// why the user is told explicitly instead of left with a bare error reaction.
var ErrTurnTimeout = errors.New("turn timed out")

// failureNotice renders the single plain-text line a user gets when their turn
// failed and no reply was produced. Plain text on purpose: Telegram renders
// markdown asterisks literally. It names the INPUT so a failed voice note reads
// as a failed voice note rather than a generic error.
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
	if errors.Is(err, ErrTurnTimeout) {
		return fmt.Sprintf("Sorry, I could not finish %s: the turn ran past my time budget, "+
			"so I gave up on it. Please send it again, or narrow the question.", what)
	}
	return fmt.Sprintf("Sorry, I could not process %s: %v", what, err)
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

	// Collect the result by subscribing to the bus for this session's result event.
	subID, events := m.bus.Subscribe()
	defer m.bus.Unsubscribe(subID)

	turnErr := make(chan error, 1)
	turnText := RenderInbound(in)
	go func() {
		_, err := m.Send(ctx, id, turnText)
		turnErr <- err
	}()

	// The harness blanks a result text that merely duplicates the turn's final
	// streamed output (single user-visible reply on the bus); the reply is then
	// that last output text.
	var result, lastOutput string
	gotResult := false
	collectEv := func(e eventbus.Event) {
		if e.SessionID != id {
			return
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
		case eventbus.KindNeedsInput:
			// Interim state: the agent is blocked on the user. The terminal
			// done/error glyph still replaces it when the turn resolves.
			react(channel.ReactionNeedsInput)
		}
	}
	budget := m.Policy.turnTimeout()
	timeout := time.After(budget)
collect:
	for {
		select {
		case e := <-events:
			collectEv(e)
		case err := <-turnErr:
			if err != nil {
				return err
			}
			// Send returning can race the bus delivery of the turn's events;
			// drain briefly until the result event lands (same grace as the
			// scheduler runner) so the reply is not built from a partial turn.
			grace := time.After(2 * time.Second)
			for !gotResult {
				select {
				case e := <-events:
					collectEv(e)
				case <-grace:
					break collect
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			break collect
		case <-timeout:
			return fmt.Errorf("session %s turn timed out after %s: %w", id, budget, ErrTurnTimeout)
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
	replied = true
	if m.log != nil {
		_ = m.log.Append("outbound", map[string]string{"session": id, "chat": in.UserID, "msg_id": rcpt.ID, "text": result})
	}
	finalReaction = channel.ReactionDone
	return nil
}
