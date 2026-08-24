package main

import (
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// TestReapDeletesPersistedMapping verifies that when a stable session expires
// the idle reaper deletes its persisted mapping too (so a restart doesn't
// resurrect an already-evicted session).
func TestReapDeletesPersistedMapping(t *testing.T) {
	dir := t.TempDir()
	store := newSessionStore(dir, time.Millisecond*100)
	if err := store.storeRecord(&sessionRecord{
		ExternalID:   "client-1",
		AcpSessionID: "acp-1",
		Cwd:          "/x",
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s := newServer(config{
		StateDir:            dir,
		SessionIdleTimeout:  100 * time.Millisecond,
		SessionScanInterval: 0,
	})
	s.store = store // keep the same store the record was written through

	p := &process{done: make(chan struct{}), client: &client{sessions: make(map[acp.SessionId]*session)}}
	sess := &session{
		process: p, id: "acp-1", lastUsed: time.Now().Add(-time.Hour),
		updates: make(chan update, 8),
	}
	s.sessions.mu.Lock()
	s.sessions.sessions["client-1"] = sess
	s.sessions.mu.Unlock()

	s.reapIdleSessions(time.Now())

	if rec, _ := s.store.loadRecord("client-1"); rec != nil {
		t.Fatalf("expired stable session mapping survived reap: %+v", rec)
	}
	if _, ok := s.sessions.sessions["client-1"]; ok {
		t.Fatal("expired stable session survived reap")
	}
}

// TestReapDoesNotTouchStoreForImplicit confirms the implicit reaper path
// passes no delete hook (implicitly: no store file for fingerprint-keyed
// sessions exists, and the call must not panic).
func TestReapDoesNotTouchStoreForImplicit(t *testing.T) {
	dir := t.TempDir()
	s := newServer(config{
		StateDir:            dir,
		SessionIdleTimeout:  time.Hour,
		ImplicitIdleTimeout: time.Hour,
		SessionScanInterval: 0,
	})
	p := &process{done: make(chan struct{}), client: &client{sessions: make(map[acp.SessionId]*session)}}
	sess := &session{
		process: p, id: "acp-i", lastUsed: time.Now().Add(-2 * time.Hour),
		updates: make(chan update, 8),
	}
	s.implicit.mu.Lock()
	s.implicit.sessions["abc123"] = sess
	s.implicit.mu.Unlock()

	s.reapIdleSessions(time.Now()) // must not panic and must not flag a store miss

	if _, ok := s.implicit.sessions["abc123"]; ok {
		t.Fatal("expired implicit session survived reap")
	}
}

// TestUpdateLastUsedPersistsStableSession confirms the per-turn touch writes
// back the refreshed last_used_at, and is a no-op for anonymous sessions.
func TestUpdateLastUsedPersistsStableSession(t *testing.T) {
	dir := t.TempDir()
	s := newServer(config{StateDir: dir, SessionIdleTimeout: time.Hour})
	old := time.Now().Add(-time.Hour)
	if err := s.store.storeRecord(&sessionRecord{
		ExternalID:   "client-1",
		AcpSessionID: "acp-1",
		Cwd:          "/x",
		CreatedAt:    old,
		LastUsedAt:   old,
	}); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now()
	updateLastUsed(s, "client-1", fresh, "GLM-5")
	rec, _ := s.store.loadRecord("client-1")
	if rec == nil || !rec.LastUsedAt.Equal(fresh) {
		t.Fatalf("last_used_at not persisted: %+v", rec)
	}
	if rec.Model != "GLM-5" {
		t.Fatalf("model not persisted: %+v", rec)
	}

	// anonymous touch is a no-op (no file created)
	before, _ := s.store.loadRecord("absent")
	updateLastUsed(s, "absent", time.Now(), "")
	after, _ := s.store.loadRecord("absent")
	if before != nil || after != nil {
		t.Fatal("updateLastUsed created a record for an anonymous session")
	}
}

// TestDisabledStoreReapIsNoOp confirms reaping does not panic and updateLastUsed
// is inert when persistence is disabled.
func TestDisabledStoreReapIsNoOp(t *testing.T) {
	s := newServer(config{StateDir: "", SessionIdleTimeout: time.Hour})
	p := &process{done: make(chan struct{}), client: &client{sessions: make(map[acp.SessionId]*session)}}
	sess := &session{process: p, id: "acp-1", lastUsed: time.Now().Add(-2 * time.Hour), updates: make(chan update, 8)}
	s.sessions.mu.Lock()
	s.sessions.sessions["client-1"] = sess
	s.sessions.mu.Unlock()
	s.reapIdleSessions(time.Now())
	updateLastUsed(s, "anything", time.Now(), "")
	if _, ok := s.sessions.sessions["client-1"]; ok {
		t.Fatal("reap did not run with disabled store")
	}
}
