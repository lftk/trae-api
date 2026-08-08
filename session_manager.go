package main

import "sync"

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
