package main

import (
	"context"
	"errors"
	"sync"
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
	mu       sync.Mutex
	sessions map[string]*session
	pending  map[string]*sessionCreation
}

func newSessionManager() *sessionManager {
	return &sessionManager{
		sessions: make(map[string]*session),
		pending:  make(map[string]*sessionCreation),
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
