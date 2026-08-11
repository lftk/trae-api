package main

import (
	"context"
	"errors"
	"log/slog"
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

func (m *sessionManager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
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
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if creation.err == nil && m.sessions[id] == creation.session && !processIsDone(creation.session.process) {
			creation.session.leases++
		} else if creation.err == nil {
			creation.err = errors.New("trae ACP process exited while creating session")
		}
		return creation.session, creation.err
	}
	if maxSessions > 0 && len(m.sessions)+len(m.pending) >= maxSessions {
		m.mu.Unlock()
		return nil, errSessionLimit
	}
	creation := &sessionCreation{done: make(chan struct{})}
	m.pending[id] = creation
	m.mu.Unlock()

	session, err := create(ctx)
	if err == nil {
		if session == nil {
			err = errors.New("session factory returned nil session")
		} else if processIsDone(session.process) {
			err = errors.New("trae ACP process exited while creating session")
		}
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
// when no live session continues the transcript. Candidates that are already
// leased are skipped: a session is only a valid continuation target after its
// own request finished, so a leased match means a different client with an
// identical conversation (fingerprint collision) — sharing its transcript
// would cross-talk. When several idle sessions match, the most recently used
// one wins; the others are left for their next request to replace.
func (m *sessionManager) takeContinuation(prefixFP, newUserFP uint64) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *session
	var bestUsed time.Time
	for key, candidate := range m.sessions {
		if candidate.leases != 0 {
			continue
		}
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
	best.leases++
	best.touchLocked()
	best.mu.Unlock()
	m.sessions[formatFingerprint(newUserFP)] = best
	return best, nil
}

// releaseBySession releases a lease by session identity, tolerating the
// manager key changing between acquisition (continuation re-key) and release.
// A session that was replaced while its request was still in flight is no
// longer in the map; once its last lease drains it is closed here.
func (m *sessionManager) releaseBySession(session *session) {
	m.mu.Lock()
	if session.leases == 0 {
		m.mu.Unlock()
		return
	}
	session.leases--
	orphaned := session.leases == 0
	if orphaned {
		for _, s := range m.sessions {
			if s == session {
				orphaned = false
				break
			}
		}
	}
	m.mu.Unlock()
	if orphaned {
		slog.Info("close replaced implicit session after lease drained", "acpsessionid", session.sessionID())
		_ = session.Close()
	}
}
