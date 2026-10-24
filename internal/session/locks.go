package session

import "context"

// LOCK ORDER. Every operation that runs the harness or touches the workspace
// of a main or side session (RunTurn, Reset, the post-turn reset, idle
// reclaim, dead-process recovery) takes the per-workspace INTERACTIVE lock
// first, then the session's turnMu, and releases both before it starts any
// other such operation. Main and side sessions share one workspace and the
// autocommit stages everything (git add -A), so two of them must never run
// at the same time. LaneOther sessions (scheduler- and API-created) take only
// their turnMu, as before. The GC only ever TryLocks both.

// workspaceKey names the workspace a session works in: the workspace name, or
// the bare cwd when no workspace store is configured.
func (s *Session) workspaceKey() string {
	if s.Workspace != "" {
		return "ws:" + s.Workspace
	}
	return "cwd:" + s.cwd
}

// interactiveLock returns the interactive lock of a workspace.
func (m *Manager) interactiveLock(key string) *turnLock {
	m.ilockMu.Lock()
	defer m.ilockMu.Unlock()
	if m.ilocks == nil {
		m.ilocks = map[string]*turnLock{}
	}
	l, ok := m.ilocks[key]
	if !ok {
		l = &turnLock{}
		m.ilocks[key] = l
	}
	return l
}

// lockSession takes the locks of a session in the fixed order and returns
// the release function.
func (m *Manager) lockSession(ctx context.Context, s *Session) (func(), error) {
	var il *turnLock
	if s.Lane.interactive() {
		il = m.interactiveLock(s.workspaceKey())
		if err := il.Lock(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.turnMu.Lock(ctx); err != nil {
		if il != nil {
			il.Unlock()
		}
		return nil, err
	}
	return func() {
		s.turnMu.Unlock()
		if il != nil {
			il.Unlock()
		}
	}, nil
}

// tryLockSession is lockSession without waiting (the GC never waits).
func (m *Manager) tryLockSession(s *Session) (func(), bool) {
	var il *turnLock
	if s.Lane.interactive() {
		il = m.interactiveLock(s.workspaceKey())
		if !il.TryLock() {
			return nil, false
		}
	}
	if !s.turnMu.TryLock() {
		if il != nil {
			il.Unlock()
		}
		return nil, false
	}
	return func() {
		s.turnMu.Unlock()
		if il != nil {
			il.Unlock()
		}
	}, true
}
