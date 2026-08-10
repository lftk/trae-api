package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

type sessionCreation struct {
	done    chan struct{}
	session *session
	err     error
}

// sessionManager owns the mapping from caller-visible session IDs to the
// session/process pair. It deliberately does not own ACP transport or process
// pooling; those lifecycles belong to processPool and process.
type sessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*session
	pending     map[string]*sessionCreation
	idleTimeout time.Duration
}

func newSessionManager(idleTimeout time.Duration) *sessionManager {
	return &sessionManager{
		sessions:    make(map[string]*session),
		pending:     make(map[string]*sessionCreation),
		idleTimeout: idleTimeout,
	}
}

func (m *sessionManager) getOrCreate(
	ctx context.Context,
	id string,
	maxSessions int,
	create func(context.Context) (*session, error),
	onDeath func(string, *session),
) (*session, error) {
	m.mu.Lock()
	if session := m.sessions[id]; session != nil {
		select {
		case <-session.process.done:
			delete(m.sessions, id)
		default:
			session.mu.Lock()
			session.touchLocked()
			session.mu.Unlock()
			session.leases++
			m.mu.Unlock()
			return session, nil
		}
	}
	if creation := m.pending[id]; creation != nil {
		m.mu.Unlock()
		select {
		case <-creation.done:
			m.mu.Lock()
			if creation.err == nil && m.sessions[id] == creation.session && !processIsDone(creation.session.process) {
				creation.session.leases++
			} else if creation.err == nil {
				creation.err = errors.New("trae ACP process exited while creating session")
			}
			session, err := creation.session, creation.err
			m.mu.Unlock()
			return session, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if maxSessions > 0 && len(m.sessions)+len(m.pending) >= maxSessions {
		m.mu.Unlock()
		return nil, errSessionLimit
	}
	creation := &sessionCreation{done: make(chan struct{})}
	m.pending[id] = creation
	m.mu.Unlock()

	session, err := create(ctx)
	if err == nil && session == nil {
		err = errors.New("session factory returned nil session")
	}
	if err == nil && processIsDone(session.process) {
		err = errors.New("trae ACP process exited while creating session")
	}

	m.mu.Lock()
	delete(m.pending, id)
	creation.session = session
	creation.err = err
	if err == nil {
		session.leases = 1
		m.sessions[id] = session
	}
	close(creation.done)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	session.process.addOnDone(func() { onDeath(id, session) })
	return session, nil
}

func (m *sessionManager) release(id string, session *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] != session || session.leases == 0 {
		return
	}
	session.leases--
}

// takeContinuation leases the implicit session whose recorded transcript
// (lastFullFP) matches prefixFP, re-keying it to newUserFP. It returns nil
// when no live session continues the transcript. When several sessions match
// (identical conversations from different clients), the most recently used
// one wins; the others are left for their next request to replace.
func (m *sessionManager) takeContinuation(prefixFP, newUserFP uint64) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *session
	var bestUsed time.Time
	for key, candidate := range m.sessions {
		candidate.mu.Lock()
		match := candidate.lastFullFP == prefixFP
		used := candidate.lastUsed
		candidate.mu.Unlock()
		if !match {
			continue
		}
		if processIsDone(candidate.process) {
			delete(m.sessions, key)
			continue
		}
		if best == nil || used.After(bestUsed) {
			best, bestUsed = candidate, used
		}
	}
	if best == nil {
		return nil, nil
	}
	best.mu.Lock()
	delete(m.sessions, formatFingerprint(best.lastUserFP))
	best.lastUserFP = newUserFP
	best.mu.Unlock()
	m.sessions[formatFingerprint(newUserFP)] = best
	best.leases++
	best.mu.Lock()
	best.touchLocked()
	best.mu.Unlock()
	return best, nil
}

// evictByUserFP removes and returns the session registered under the exact
// request fingerprint, when it is not in flight. A repeat of the identical
// request must not reuse the session: its transcript already contains that
// prompt, so replaying it would duplicate the turn. The caller closes the
// returned session and creates a fresh one.
func (m *sessionManager) evictByUserFP(fp uint64) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := formatFingerprint(fp)
	session := m.sessions[key]
	if session == nil || session.leases != 0 {
		return nil
	}
	delete(m.sessions, key)
	return session
}

// releaseBySession releases a lease by session identity, tolerating the
// manager key changing between acquisition (continuation re-key) and release.
func (m *sessionManager) releaseBySession(session *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session.leases == 0 {
		return
	}
	session.leases--
}
