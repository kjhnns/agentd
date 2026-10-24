package session

// TURN OWNERSHIP (D1).
//
// RunTurn is the ONE owner of a session's turnMu. It takes the lock, then
// re-resolves the session (it may have been reclaimed while the turn waited),
// assigns a turn id, subscribes to the bus, sends, collects the result and
// only then releases the lock. The pump stamps the CURRENT turn id on every
// event when it reads it from the harness, and the collector only accepts
// events with its own turn id, so a turn queued behind another can never
// collect the earlier turn's result. The quiet timeout starts when the turn
// owns the session, so queue time does not count against it. On a timeout the
// turn is interrupted and its end awaited before the lock is released.
//
// Every caller goes through RunTurn: RouteInbound, the scheduler runner, the
// HTTP API (through SendAndCollect) and the legacy Send.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/eventbus"
	"github.com/kjhnns/agentd/internal/harness"
)

// turnLock is a mutex whose Lock honours a context, so a turn that waits in
// the queue can still be cancelled. The zero value is ready to use.
type turnLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *turnLock) c() chan struct{} {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	return l.ch
}

// Lock waits for the lock or for ctx to end.
func (l *turnLock) Lock(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case l.c() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryLock takes the lock only when it is free.
func (l *turnLock) TryLock() bool {
	select {
	case l.c() <- struct{}{}:
		return true
	default:
		return false
	}
}

// Unlock releases the lock.
func (l *turnLock) Unlock() {
	select {
	case <-l.c():
	default:
		panic("session: unlock of an unlocked turnLock")
	}
}

// ErrSessionGone means the session was reclaimed or removed while the turn
// waited for it. The caller resolves (or creates) the session again.
var ErrSessionGone = errors.New("session is gone")

// TurnAttachment is one structured attachment of a turn (a media file today,
// a conversation artifact later). It is recorded in the turn record.
type TurnAttachment struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
}

// TurnRequest is one turn for RunTurn.
type TurnRequest struct {
	SessionID string
	// Text is the turn text handed to the harness (already rendered).
	Text string
	// UserText is what the user wrote, for the turn record. "" = Text.
	UserText string
	// Identity of the message that started the turn (all optional).
	RequestID   string
	MessageID   string
	Chat        string
	Conv        string
	Attachments []TurnAttachment
	// Options bounds and observes the collection.
	Options TurnOptions
	// NoCollect returns as soon as the harness finished the send, without
	// collecting a result (the legacy Send). The turn still owns the lock
	// for the whole send.
	NoCollect bool
}

// TurnResult is the outcome of RunTurn.
type TurnResult struct {
	Text       string
	TurnID     string
	Generation uint64
}

// abortWait bounds how long an abandoned turn is awaited after the interrupt
// (and again after its context is cancelled) before the lock is released.
var abortWait = 10 * time.Second

// lockTurn takes the locks a turn needs, in the fixed order (see locks.go),
// and returns the release function.
func (m *Manager) lockTurn(ctx context.Context, s *Session) (func(), error) {
	return m.lockSession(ctx, s)
}

// RunTurn runs one turn on a session and owns it from the send through the
// collection of its result. See the file comment.
func (m *Manager) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	id := req.SessionID
	s, ok := m.Get(id)
	if !ok {
		return TurnResult{}, fmt.Errorf("session %s not found: %w", id, ErrSessionGone)
	}
	unlock, err := m.lockTurn(ctx, s)
	if err != nil {
		return TurnResult{}, err
	}
	locked := true
	release := func() {
		if locked {
			locked = false
			unlock()
		}
	}
	defer release()

	// Re-resolve under the lock: the GC may have reclaimed the session while
	// this turn waited, and a reclaimed session must not run a turn.
	if cur, ok := m.Get(id); !ok || cur != s {
		return TurnResult{}, fmt.Errorf("session %s: %w", id, ErrSessionGone)
	}

	if m.log != nil {
		_ = m.log.Append("input", map[string]string{"session": id, "text": req.Text})
	}

	turnID := newID()
	s.mu.Lock()
	s.curTurn = turnID
	s.running = true
	s.status = harness.StatusThinking
	handle := s.handle
	gen := s.gen
	s.mu.Unlock()

	var events <-chan eventbus.Event
	if !req.NoCollect {
		subID, ch := m.bus.Subscribe()
		defer m.bus.Unsubscribe(subID)
		events = ch
	}

	sctx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()
	sendErr := make(chan error, 1)
	go func() { sendErr <- s.adapter.Send(sctx, handle, harness.Input{Text: req.Text}) }()

	var text string
	var turnErr error
	sendDone := false
	if req.NoCollect {
		select {
		case turnErr = <-sendErr:
			sendDone = true
		case <-ctx.Done():
			turnErr = ctx.Err()
		}
	} else {
		text, turnErr, sendDone = m.collect(ctx, s, turnID, events, sendErr, req.Options)
	}
	if !sendDone {
		// The turn was abandoned (timeout, ceiling, cancel). Interrupt it and
		// wait for its end before anyone else may use the session.
		m.abortTurn(s, handle, sendErr, cancelSend)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.curTurn = ""
	s.running = false
	s.status = s.adapter.Status(s.handle)
	if turnErr != nil {
		s.status = harness.StatusError
	}
	s.turns++
	s.lastActivity = now
	s.mu.Unlock()

	res := TurnResult{Text: text, TurnID: turnID, Generation: gen}
	if turnErr != nil {
		m.afterTurn(s, req, res, turnErr)
		return res, turnErr
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
	m.afterTurn(s, req, res, nil)
	release()

	if tripped {
		// Background so the reply goes out promptly; the reset takes the locks
		// again and is dropped when the process was replaced in between (an
		// older generation).
		go func() {
			if rerr := m.resetIfGeneration(context.Background(), id, reason, gen); rerr != nil {
				log.Printf("session %s: post-turn reset (%s) failed: %v", id, reason, rerr)
			}
		}()
	}
	return res, nil
}

// afterTurn is the hook for the end of a turn, still under ownership.
func (m *Manager) afterTurn(s *Session, req TurnRequest, res TurnResult, err error) {}

// abortTurn ends an abandoned turn before anyone else may use the session:
// it interrupts the turn, cancels its send, waits (bounded) for the send to
// return, and then replaces the process. The replacement is unconditional: an
// interrupt ends a Claude process anyway, and a process that is still busy
// with the old turn would hand its late answer to the next one. The old
// pump's events carry no turn id once the generation advanced. Caller holds
// the session's locks.
func (m *Manager) abortTurn(s *Session, handle harness.Handle, sendErr <-chan error, cancelSend context.CancelFunc) {
	if err := s.adapter.Interrupt(handle); err != nil {
		log.Printf("session %s: interrupt of an abandoned turn failed: %v", s.ID, err)
	}
	cancelSend()
	ended := false
	select {
	case <-sendErr:
		ended = true
	case <-time.After(abortWait):
	}
	if err := m.replaceProcessLocked(context.Background(), s, "abandoned turn"); err != nil {
		log.Printf("session %s: replace after an abandoned turn failed: %v", s.ID, err)
	}
	if !ended {
		// The teardown above ends a send that ignored its context.
		select {
		case <-sendErr:
		case <-time.After(abortWait):
			log.Printf("session %s: abandoned send did not return after teardown", s.ID)
		}
	}
}

// collect drains the bus for ONE turn. It returns the user-visible result,
// the turn error, and whether the harness send has returned.
func (m *Manager) collect(ctx context.Context, s *Session, turnID string, events <-chan eventbus.Event, sendErr <-chan error, opt TurnOptions) (string, error, bool) {
	id := s.ID
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

	handle := func(e eventbus.Event) {
		// Only this turn's events: the pump stamps the turn id when it reads
		// the event from the harness.
		if e.SessionID != id || e.TurnID != turnID {
			return
		}
		if opt.OnEvent != nil {
			opt.OnEvent(e)
		}
		// Progress resets the quiet timer: work that is visibly happening buys
		// more time; silence does not.
		if quiet != nil && isProgress(e.Kind) {
			if !quiet.Stop() {
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
	abandon := func(ceiling bool) (string, error, bool) {
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
		return lastOutput, te, false
	}

	for {
		select {
		case e := <-events:
			handle(e)
		case err := <-sendErr:
			if err != nil {
				return "", err, true
			}
			// Send returning can race the bus delivery of the turn's events;
			// drain briefly until the result event lands so the reply is not
			// built from a partial turn. Still under ownership.
			grace := time.After(resultGrace)
			for !gotResult {
				select {
				case e := <-events:
					handle(e)
				case <-grace:
					return result, nil, true
				case <-ctx.Done():
					return "", ctx.Err(), true
				}
			}
			return result, nil, true
		case <-quietC:
			return abandon(false)
		case <-ceilingC:
			return abandon(true)
		case <-ctx.Done():
			return "", ctx.Err(), false
		}
	}
}

// Send routes one user turn to the session's harness and returns when the
// harness finished it. It is RunTurn without collection: the turn still owns
// the session for the whole send. After a completed turn the context-reset
// triggers are evaluated at that natural boundary (see RunTurn).
func (m *Manager) Send(ctx context.Context, id, text string) (*SendResult, error) {
	if _, err := m.RunTurn(ctx, TurnRequest{SessionID: id, Text: text, NoCollect: true}); err != nil {
		return nil, err
	}
	return &SendResult{}, nil
}

// SendAndCollect sends one user turn to a session and returns the turn's
// USER-VISIBLE result text: RunTurn with the given options. A blanked result
// text maps back onto the turn's last streamed output (the harness blanks a
// result that merely duplicates it). The returned text is "" when the turn
// produced nothing; callers that must say SOMETHING substitute their own
// placeholder.
func (m *Manager) SendAndCollect(ctx context.Context, id, text string, opt TurnOptions) (string, error) {
	res, err := m.RunTurn(ctx, TurnRequest{SessionID: id, Text: text, Options: opt})
	if err != nil {
		var te *TurnTimeoutError
		if errors.As(err, &te) {
			return res.Text, err
		}
		return "", err
	}
	return res.Text, nil
}
