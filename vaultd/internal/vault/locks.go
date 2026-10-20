package vault

import "sync"

// pathLocks is a refcounted lock per canonical store path (spec section 7).
// Its own mutex is a leaf: it is never held while taking another lock.
type pathLocks struct {
	mu sync.Mutex
	m  map[string]*pathLock
}

type pathLock struct {
	mu   sync.Mutex
	refs int
}

func newPathLocks() *pathLocks { return &pathLocks{m: map[string]*pathLock{}} }

// lock takes the lock of path and returns its unlock.
func (p *pathLocks) lock(path string) func() {
	p.mu.Lock()
	l := p.m[path]
	if l == nil {
		l = &pathLock{}
		p.m[path] = l
	}
	l.refs++
	p.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(p.m, path)
		}
		p.mu.Unlock()
	}
}
