package main

import (
	"context"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type loadCall struct {
	id  string
	cwd string
}

type restoreHarness struct {
	mu        sync.Mutex
	created   int
	loadCalls []loadCall
	loadErr   error
	enable    bool
}

func (h *restoreHarness) factory() func(context.Context, config) (*process, error) {
	return func(_ context.Context, cfg config) (*process, error) {
		h.mu.Lock()
		h.created++
		h.mu.Unlock()
		p := &process{
			done:          make(chan struct{}),
			client:        &client{sessions: make(map[acp.SessionId]*session)},
			workdir:       cfg.Workdir,
			workdirTemp:   cfg.WorkdirTemp,
			loadSupported: h.enable,
		}
		var seq int
		p.newSessionFunc = func(context.Context) (*session, error) {
			seq++
			s := &session{process: p, id: acp.SessionId("acp-new"), lastUsed: time.Now(), updates: make(chan update, 8)}
			p.client.addSession(s)
			return s, nil
		}
		if h.enable {
			p.loadSessionFunc = func(_ context.Context, id, cwd string) (*session, error) {
				h.mu.Lock()
				h.loadCalls = append(h.loadCalls, loadCall{id: id, cwd: cwd})
				err := h.loadErr
				h.mu.Unlock()
				if err != nil {
					return nil, err
				}
				s := &session{process: p, id: acp.SessionId(id), lastUsed: time.Now(), updates: make(chan update, 8)}
				p.client.addSession(s)
				return s, nil
			}
		}
		return p, nil
	}
}

func (h *restoreHarness) loads() []loadCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]loadCall{}, h.loadCalls...)
}

func seedMapping(t *testing.T, dir, externalID, acpID, cwd string) {
	t.Helper()
	if err := newSessionStore(dir, time.Hour).storeRecord(&sessionRecord{
		ExternalID:   externalID,
		AcpSessionID: acpID,
		Cwd:          cwd,
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServerResumesPersistedSessionViaLoad(t *testing.T) {
	dir := t.TempDir()
	seedMapping(t, dir, "client-1", "acp-persisted", "/orig/dir")
	h := &restoreHarness{enable: true}
	s := newServer(config{StateDir: dir, SessionIdleTimeout: time.Hour})
	s.newProcess = h.factory()

	lease, err := s.acquireSession(context.Background(), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.session.sessionID(); got != "acp-persisted" {
		t.Fatalf("session id = %q, want acp-persisted (resumed via load)", got)
	}
	loads := h.loads()
	if len(loads) != 1 || loads[0].id != "acp-persisted" || loads[0].cwd != "/orig/dir" {
		t.Fatalf("expected one load(acp-persisted,/orig/dir), got %+v", loads)
	}
	// mapping must be preserved unchanged (no overwrite from the new path)
	rec, _ := s.store.loadRecord("client-1")
	if rec == nil || rec.AcpSessionID != "acp-persisted" {
		t.Fatalf("mapping after resume = %+v, want acp-persisted", rec)
	}
	lease.release()
}

func TestServerDegradesToFreshWhenLoadFails(t *testing.T) {
	dir := t.TempDir()
	seedMapping(t, dir, "client-1", "acp-gone", "/orig/dir")
	h := &restoreHarness{enable: true, loadErr: errSimple}
	s := newServer(config{StateDir: dir, SessionIdleTimeout: time.Hour})
	s.newProcess = h.factory()

	lease, err := s.acquireSession(context.Background(), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.session.sessionID(); got != "acp-new" {
		t.Fatalf("session id = %q, want acp-new (degraded fresh)", got)
	}
	// the failed-load branch deletes the stale mapping, then the fresh path
	// writes a replacement mapping for the new live session
	rec, _ := s.store.loadRecord("client-1")
	if rec == nil {
		t.Fatal("fresh mapping not written after degraded load")
	}
	if rec.AcpSessionID != "acp-new" {
		t.Fatalf("replacement mapping acp id = %q, want acp-new", rec.AcpSessionID)
	}
	lease.release()
}

func TestServerDegradesWhenLoadUnsupported(t *testing.T) {
	dir := t.TempDir()
	seedMapping(t, dir, "client-1", "acp-legacy", "/orig/dir")
	h := &restoreHarness{enable: false} // old trae-cli without LoadSession
	s := newServer(config{StateDir: dir, SessionIdleTimeout: time.Hour})
	s.newProcess = h.factory()

	lease, err := s.acquireSession(context.Background(), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.session.sessionID(); got != "acp-new" {
		t.Fatalf("session id = %q, want acp-new (no load support)", got)
	}
	if loads := h.loads(); len(loads) != 0 {
		t.Fatalf("load invoked without support: %+v", loads)
	}
	// the unrestorable mapping is replaced with the new live session's mapping
	rec, _ := s.store.loadRecord("client-1")
	if rec == nil || rec.AcpSessionID != "acp-new" {
		t.Fatalf("mapping after degrade = %+v, want acp-new", rec)
	}
	lease.release()
}

func TestServerSessionLoadedKeepsCurrentModelForEmptyRequest(t *testing.T) {
	dir := t.TempDir()
	seedMapping(t, dir, "client-1", "acp-persisted", "/orig/dir")
	h := &restoreHarness{enable: true}
	s := newServer(config{StateDir: dir, SessionIdleTimeout: time.Hour})
	s.newProcess = h.factory()

	lease, err := s.acquireSession(context.Background(), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	lease.session.mu.Lock()
	defer lease.session.mu.Unlock()
	// simulate the model restored by session/load's currentValue
	lease.session.currentModel = "GLM-5"
	lease.session.models = []string{"GLM-5", "Doubao-Seed"}
	if got, err := lease.session.setModel(context.Background(), ""); err != nil || got != "GLM-5" {
		t.Fatalf("setModel(\"\") on restored session = (%q,%v), want (GLM-5,nil)", got, err)
	}
}

var errSimple = errorsF("load failed for test")

type errorsF string

func (e errorsF) Error() string { return string(e) }
