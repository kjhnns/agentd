package session

import "context"

// lockSession takes the locks every operation that runs the harness or
// touches the workspace of a session needs, and returns the release
// function: the session's turnMu.
func (m *Manager) lockSession(ctx context.Context, s *Session) (func(), error) {
	if err := s.turnMu.Lock(ctx); err != nil {
		return nil, err
	}
	return s.turnMu.Unlock, nil
}

// tryLockSession is lockSession without waiting (the GC never waits).
func (m *Manager) tryLockSession(s *Session) (func(), bool) {
	if !s.turnMu.TryLock() {
		return nil, false
	}
	return s.turnMu.Unlock, true
}
