package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func testProcessFactory() (func(context.Context, config) (*process, error), func() (*process, int)) {
	var mu sync.Mutex
	created := 0
	var currentProcess *process
	factory := func(_ context.Context, cfg config) (*process, error) {
		mu.Lock()
		defer mu.Unlock()
		created++
		p := &process{done: make(chan struct{}), client: &client{sessions: make(map[acp.SessionId]*session)}, workdirTemp: cfg.WorkdirTemp}
		var sequence int
		p.newSessionFunc = func(context.Context) (*session, error) {
			sequence++
			s := &session{process: p, session: acp.SessionId(fmt.Sprintf("acp-%d", sequence)), lastUsed: time.Now(), updates: make(chan update, 8)}
			p.client.addSession(s)
			return s, nil
		}
		currentProcess = p
		return p, nil
	}
	count := func() (*process, int) { mu.Lock(); defer mu.Unlock(); return currentProcess, created }
	return factory, count
}

func TestServerCreatesDedicatedProcessPerStableSession(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{})
	s.newProcess = factory

	oneLease, err := s.acquireSession(context.Background(), "client-1")
	if err != nil {
		t.Fatal(err)
	}
	one := oneLease.session
	oneAgainLease, err := s.acquireSession(context.Background(), oneLease.id)
	if err != nil {
		t.Fatal(err)
	}
	oneAgain := oneAgainLease.session
	if one != oneAgain || one.sessionID() != "acp-1" {
		t.Fatalf("stable session was not reused: %p/%p %s", one, oneAgain, one.sessionID())
	}
	twoLease, err := s.acquireSession(context.Background(), "client-2")
	if err != nil {
		t.Fatal(err)
	}
	if one.process == twoLease.session.process {
		t.Fatal("different stable sessions reused the same process")
	}
	if got, created := current(); got == nil || got.client == nil {
		t.Fatal("dedicated process was not created")
	} else if created != 2 {
		t.Fatalf("created %d trae processes, want 2", created)
	}
}

func TestServerCreatesFreshACPSessionWithoutExternalID(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true})
	s.newProcess = factory

	oneLease, err := s.acquireSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	one := oneLease.session
	p, _ := current()
	if len(s.sessions.sessions) != 0 || len(s.sessions.pending) != 0 || len(p.client.sessions) != 1 {
		t.Fatalf("anonymous session was retained: sessions=%d pending=%d", len(s.sessions.sessions), len(s.sessions.pending))
	}
	twoLease, err := s.acquireSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	two := twoLease.session
	for _, temporary := range []*session{one, two} {
		if got := temporary.preparePrompt("[user]\nHello"); !strings.HasPrefix(got, "[system]\n"+temporaryWorkspaceNotice) {
			t.Fatalf("anonymous session %q did not receive workspace notice: %q", temporary.sessionID(), got)
		}
	}
	oneLease.release()
	twoLease.release()
	if len(p.client.sessions) != 0 {
		t.Fatalf("temporary ACP sessions remained: %d", len(p.client.sessions))
	}
	select {
	case <-one.process.done:
	default:
		t.Fatal("first temporary session process was not closed")
	}
	select {
	case <-two.process.done:
	default:
		t.Fatal("second temporary session process was not closed")
	}
	if one.process == two.process {
		t.Fatal("anonymous requests reused the same process")
	}
	if got, created := current(); got == nil {
		t.Fatal("process was not created")
	} else if created != 2 {
		t.Fatalf("created %d trae processes, want 2", created)
	}
}

func TestTemporarySessionReleaseClosesSupportedACPSession(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{})
	s.newProcess = factory
	lease, err := s.acquireSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := current()
	p.closeSupported = true
	var closed acp.SessionId
	p.closeSessionFunc = func(context.Context, acp.SessionId) error {
		closed = lease.session.session
		return nil
	}
	lease.release()
	if closed != lease.session.session {
		t.Fatalf("closed ACP session %q, want %q", closed, lease.session.session)
	}
	if len(s.sessions.sessions) != 0 || len(s.sessions.pending) != 0 {
		t.Fatalf("temporary session was retained: sessions=%d pending=%d", len(s.sessions.sessions), len(s.sessions.pending))
	}
}

func TestServerRecreatesSessionAfterProcessFailure(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{})
	s.newProcess = factory
	if _, err := s.acquireSession(context.Background(), "client"); err != nil {
		t.Fatal(err)
	}
	p, created := current()
	if created != 1 {
		t.Fatalf("created %d processes, want 1", created)
	}
	s.handleSessionDeath("client", p.client.sessions["acp-1"])
	if _, err := s.acquireSession(context.Background(), "client"); err != nil {
		t.Fatal(err)
	}
	if _, created = current(); created != 2 {
		t.Fatalf("created %d processes after failure, want 2", created)
	}
}

func TestReapIdleSessionClosesSupportedACPSession(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{SessionIdleTimeout: time.Minute})
	s.newProcess = factory
	lease, err := s.acquireSession(context.Background(), "idle-client")
	if err != nil {
		t.Fatal(err)
	}
	session, id := lease.session, lease.id
	p, _ := current()
	p.closeSupported = true
	var closed acp.SessionId
	p.closeSessionFunc = func(context.Context, acp.SessionId) error { closed = session.session; return nil }
	session.lastUsed = time.Now().Add(-2 * time.Minute)
	s.reapIdleSessions(time.Now())
	if closed != session.session {
		t.Fatalf("closed ACP session %q, want %q", closed, session.session)
	}
	if _, ok := s.sessions.sessions[id]; ok {
		t.Fatal("idle session remained in server map")
	}
}

func TestTraeClientRoutesUpdatesByACPSessionID(t *testing.T) {
	c := &client{sessions: make(map[acp.SessionId]*session)}
	a := &session{session: "a", updates: make(chan update, 1)}
	b := &session{session: "b", updates: make(chan update, 1)}
	c.addSession(a)
	c.addSession(b)

	message := func(id acp.SessionId, text string) acp.SessionNotification {
		return acp.SessionNotification{SessionId: id, Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text)}}}
	}
	if err := c.SessionUpdate(context.Background(), message("a", "only-a")); err != nil {
		t.Fatal(err)
	}
	if err := c.SessionUpdate(context.Background(), message("b", "only-b")); err != nil {
		t.Fatal(err)
	}
	if got := (<-a.updates).Text; got != "only-a" {
		t.Fatalf("session a received %q", got)
	}
	if got := (<-b.updates).Text; got != "only-b" {
		t.Fatalf("session b received %q", got)
	}
}

func TestProcessPoolUsesWarmProcessAndClosesIt(t *testing.T) {
	factory, _ := testProcessFactory()
	p := newProcessPool(config{WarmProcesses: 1}, factory)
	process, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if process == nil {
		t.Fatal("warm process was nil")
	}
	p.close()
	_ = process.Close()
	select {
	case <-process.done:
	default:
		t.Fatal("assigned process was not closed")
	}
}

func TestProcessPoolCreatesProcessesOnDemand(t *testing.T) {
	factory, _ := testProcessFactory()
	p := newProcessPool(config{}, factory)
	process, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	second, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second == process {
		t.Fatal("on-demand acquire reused an assigned process")
	}
	p.close()
}
