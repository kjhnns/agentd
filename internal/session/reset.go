// Session lifecycle: the CONTEXT RESET (agentd-owned, harness-agnostic) plus
// the GC/sweeper that reclaims idle and recovers dead sessions.
//
// Steady state is ONE long-lived warm session per workspace. agentd does NOT
// compact the harness's context window; it owns a controlled RESET instead.
// When a threshold trips (pressure, turns, wallclock, idle, or an explicit
// call), agentd:
//
//  1. runs a bounded CHECKPOINT-FLUSH turn on the CURRENT handle, instructing
//     the agent to write anything worth preserving into context.md + memory,
//  2. waits for that turn so the git autocommit captures the artifacts,
//  3. TEARS DOWN the process,
//  4. STARTS A FRESH process for the SAME session id, re-hydrated from the now
//     updated workspace artifacts (ComposeSystemPrompt).
//
// The session identity (id/title/workspace) is continuous from agentd's side;
// only the harness process (its context window) is new. The artifacts are what
// make the reset LOSSLESS: the fresh process reads the fact back from
// context.md/memory, not from the old process's context.
package session

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/kjhnns/agentd/internal/harness"
)

// checkpointFlushPrompt is the bounded instruction injected before teardown. It
// is deliberately tight: preserve durable state, do not start new work.
const checkpointFlushPrompt = `SYSTEM CHECKPOINT: your context is being reset shortly. Before that happens, ` +
	`persist anything worth keeping so a fresh session can continue losslessly. Concisely:
- Update context.md with open threads, decisions made (and why), pending confirmations, and blockers.
- Write any NEW durable learnings (fixed quirks, stable facts, decisions) into the relevant memory/pages/*.md, re-synthesizing the page rather than appending; keep memory/INDEX.md current.
- Do NOT start any new task or tool work beyond these writes. When done, reply with just: CHECKPOINT SAVED.`

// Reset performs a controlled context reset on a session: checkpoint-flush ->
// teardown -> fresh re-hydrated process (same id). It serializes against turns
// via turnMu so it never runs mid-turn. reason is recorded to the run-log.
func (m *Manager) Reset(ctx context.Context, id, reason string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return m.resetLocked(ctx, s, reason)
}

// resetLocked runs the reset state machine. Caller holds s.turnMu.
func (m *Manager) resetLocked(ctx context.Context, s *Session, reason string) error {
	s.mu.Lock()
	oldHandle := s.handle
	s.mu.Unlock()

	prePressure := s.adapter.Pressure(oldHandle)

	// (1)+(2) bounded checkpoint-flush turn, then capture artifacts in git.
	flushErr := m.checkpointFlush(ctx, s, oldHandle)
	flushOK := flushErr == nil
	if flushErr != nil {
		log.Printf("session %s: checkpoint-flush incomplete (%v); proceeding to teardown+fresh", s.ID, flushErr)
	}
	checkpointCommit := ""
	if m.GitAutoCommit && s.ws != nil {
		if sha, cerr := s.ws.AutoCommit("checkpoint (reset: " + reason + ")"); cerr != nil {
			log.Printf("session %s: checkpoint autocommit failed: %v", s.ID, cerr)
		} else {
			checkpointCommit = sha
		}
	}

	// (3) tear down the old process.
	_ = s.adapter.Teardown(oldHandle)

	// (4) fresh process re-hydrated from the (now updated) artifacts.
	cwd := s.cwd
	systemPrompt := ""
	if s.ws != nil {
		cwd = s.ws.Cwd()
		sp, perr := s.ws.ComposeSystemPrompt()
		if perr != nil {
			return fmt.Errorf("session %s: recompose injection on reset: %w", s.ID, perr)
		}
		systemPrompt = sp
	}
	// The checkpoint-flush above preserved SUMMARISED state (context.md +
	// memory). It cannot preserve WORDING: a summary is the agent's paraphrase,
	// and a back-reference ("re 2", "like I asked you") points at literal text.
	// Append the verbatim tail of this thread from the run-log so the fresh
	// process still has the exact words. See recap.go.
	systemPrompt = m.composeInjection(systemPrompt, s.Title)
	h, err := m.adapter.Start(ctx, harness.SessionConfig{
		SessionID:       s.ID,
		Cwd:             cwd,
		Model:           s.model,
		SystemPrompt:    systemPrompt,
		SkipPermissions: m.SkipPermissions,
		PermissionMode:  m.PermissionMode,
	})
	if err != nil {
		s.mu.Lock()
		s.status = harness.StatusDead
		s.mu.Unlock()
		return fmt.Errorf("session %s: fresh start after reset failed: %w", s.ID, err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.handle = h
	s.turns = 0
	s.handleStart = now
	s.lastActivity = now
	s.status = harness.StatusIdle
	s.mu.Unlock()

	go m.pump(s, h)

	if m.log != nil {
		_ = m.log.Append("session_reset", map[string]any{
			"session":           s.ID,
			"reason":            reason,
			"pre_pressure":      prePressure.Fraction,
			"pressure_source":   string(prePressure.Source),
			"flush_ok":          flushOK,
			"checkpoint_commit": checkpointCommit,
			"ts":                now,
		})
	}
	log.Printf("session %s: context reset done (reason=%s, pre_pressure=%.2f/%s, flush_ok=%v, commit=%s)",
		s.ID, reason, prePressure.Fraction, prePressure.Source, flushOK, checkpointCommit)
	return nil
}

// checkpointFlush runs the bounded flush turn. It is robust: on error/timeout it
// returns the error but the caller proceeds to teardown+fresh regardless, so a
// wedged flush can never strand the session.
func (m *Manager) checkpointFlush(ctx context.Context, s *Session, h harness.Handle) error {
	fctx, cancel := context.WithTimeout(ctx, m.Policy.checkpointTimeout())
	defer cancel()
	s.mu.Lock()
	s.status = harness.StatusThinking
	s.mu.Unlock()
	return s.adapter.Send(fctx, h, harness.Input{Text: checkpointFlushPrompt})
}

// StartGC launches the sweeper goroutine: on each tick it enforces the idle
// timeout (checkpoint-flush + reclaim the process) and recovers dead/stale
// sessions. It returns immediately; the goroutine stops when ctx is cancelled.
func (m *Manager) StartGC(ctx context.Context) {
	iv := m.Policy.gcInterval()
	go func() {
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.gcSweep(ctx)
			}
		}
	}()
}

// gcSweep enforces idle_timeout and reclaims dead sessions once. It never blocks
// on a running turn: it TryLocks each session's turnMu and skips a busy one
// (its idleness will be re-checked next tick).
func (m *Manager) gcSweep(ctx context.Context) {
	m.mu.RLock()
	snapshot := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		snapshot = append(snapshot, s)
	}
	m.mu.RUnlock()

	for _, s := range snapshot {
		s.mu.Lock()
		handle := s.handle
		last := s.lastActivity
		s.mu.Unlock()

		// Dead-process recovery: a fresh start from artifacts in place (a dead
		// process cannot be checkpoint-flushed; the last committed artifacts are
		// the recovery point). fresh-from-artifacts is always the safe fallback.
		if s.adapter.Status(handle) == harness.StatusDead {
			if s.turnMu.TryLock() {
				m.recoverDead(ctx, s)
				s.turnMu.Unlock()
			}
			continue
		}

		if m.Policy.IdleTimeout > 0 && time.Since(last) >= m.Policy.IdleTimeout {
			if s.turnMu.TryLock() {
				m.reclaimIdle(ctx, s)
				s.turnMu.Unlock()
			}
		}
	}
}

// recoverDead restarts a dead session's process fresh from the workspace
// artifacts, preserving the session id. Caller holds s.turnMu.
func (m *Manager) recoverDead(ctx context.Context, s *Session) {
	s.mu.Lock()
	old := s.handle
	s.mu.Unlock()
	_ = s.adapter.Teardown(old)

	cwd := s.cwd
	systemPrompt := ""
	if s.ws != nil {
		cwd = s.ws.Cwd()
		if sp, err := s.ws.ComposeSystemPrompt(); err == nil {
			systemPrompt = sp
		}
	}
	// A dead process was never checkpoint-flushed, so context.md is stale as of
	// the last reset. The run-log recap is the ONLY record of what was said
	// since; it matters more here than anywhere else. See recap.go.
	systemPrompt = m.composeInjection(systemPrompt, s.Title)
	h, err := m.adapter.Start(ctx, harness.SessionConfig{
		SessionID:       s.ID,
		Cwd:             cwd,
		Model:           s.model,
		SystemPrompt:    systemPrompt,
		SkipPermissions: m.SkipPermissions,
		PermissionMode:  m.PermissionMode,
	})
	if err != nil {
		log.Printf("session %s: dead-recovery restart failed: %v", s.ID, err)
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.handle = h
	s.turns = 0
	s.handleStart = now
	s.lastActivity = now
	s.status = harness.StatusIdle
	s.mu.Unlock()
	go m.pump(s, h)
	if m.log != nil {
		_ = m.log.Append("session_recover", map[string]any{"session": s.ID, "reason": "dead", "ts": now})
	}
	log.Printf("session %s: recovered dead process (fresh from artifacts)", s.ID)
}

// reclaimIdle checkpoint-flushes an idle session, captures the artifacts, tears
// down the process to free resources, and removes the session. A later inbound
// re-hydrates a fresh one from the committed artifacts. Caller holds s.turnMu.
func (m *Manager) reclaimIdle(ctx context.Context, s *Session) {
	s.mu.Lock()
	handle := s.handle
	s.mu.Unlock()

	flushErr := m.checkpointFlush(ctx, s, handle)
	if flushErr != nil {
		log.Printf("session %s: idle checkpoint-flush incomplete (%v); reclaiming anyway", s.ID, flushErr)
	}
	commit := ""
	if m.GitAutoCommit && s.ws != nil {
		if sha, cerr := s.ws.AutoCommit("checkpoint (idle reclaim)"); cerr == nil {
			commit = sha
		}
	}
	_ = s.adapter.Teardown(handle)

	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.mu.Unlock()

	s.mu.Lock()
	s.status = harness.StatusDead
	s.mu.Unlock()

	if m.log != nil {
		_ = m.log.Append("session_reclaim", map[string]any{
			"session":           s.ID,
			"reason":            "idle",
			"flush_ok":          flushErr == nil,
			"checkpoint_commit": commit,
			"ts":                time.Now().UTC(),
		})
	}
	log.Printf("session %s: idle-reclaimed (flush_ok=%v, commit=%s)", s.ID, flushErr == nil, commit)
}
