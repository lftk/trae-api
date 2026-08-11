package main

import (
	"context"
	"errors"
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
			s := &session{process: p, id: acp.SessionId(fmt.Sprintf("acp-%d", sequence)), lastUsed: time.Now(), updates: make(chan update, 8)}
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
		closed = lease.session.id
		return nil
	}
	lease.release()
	if closed != lease.session.id {
		t.Fatalf("closed ACP session %q, want %q", closed, lease.session.id)
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
	lease.release()
	p, _ := current()
	p.closeSupported = true
	var closed acp.SessionId
	p.closeSessionFunc = func(context.Context, acp.SessionId) error { closed = session.id; return nil }
	session.lastUsed = time.Now().Add(-2 * time.Minute)
	s.reapIdleSessions(time.Now())
	if closed != session.id {
		t.Fatalf("closed ACP session %q, want %q", closed, session.id)
	}
	if _, ok := s.sessions.sessions[id]; ok {
		t.Fatal("idle session remained in server map")
	}
}

func TestReapIdleSessionDoesNotCloseLeasedSession(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{SessionIdleTimeout: time.Minute})
	s.newProcess = factory
	lease, err := s.acquireSession(context.Background(), "active-client")
	if err != nil {
		t.Fatal(err)
	}
	session := lease.session
	session.lastUsed = time.Now().Add(-2 * time.Minute)
	s.reapIdleSessions(time.Now())
	if _, ok := s.sessions.sessions["active-client"]; !ok {
		t.Fatal("leased session was reaped")
	}
	if process, _ := current(); processIsDone(process) {
		t.Fatal("leased session process was closed")
	}
	lease.release()
	s.reapIdleSessions(time.Now())
}

func TestTraeClientRoutesUpdatesByACPSessionID(t *testing.T) {
	c := &client{sessions: make(map[acp.SessionId]*session)}
	a := &session{id: "a", updates: make(chan update, 1)}
	b := &session{id: "b", updates: make(chan update, 1)}
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

func TestTraeClientDropsUpdatesWithoutActivePrompt(t *testing.T) {
	c := &client{sessions: make(map[acp.SessionId]*session)}
	s := &session{id: "inactive"}
	c.addSession(s)
	n := acp.SessionNotification{SessionId: s.id, Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("late")},
	}}
	if err := c.SessionUpdate(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-s.updates:
		t.Fatalf("received inactive update %q", got.Text)
	default:
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

func TestProcessPoolConcurrentAcquireReturnsDistinctProcesses(t *testing.T) {
	factory, _ := testProcessFactory()
	p := newProcessPool(config{WarmProcesses: 1}, factory)
	defer p.close()

	const count = 8
	results := make(chan *process, count)
	errors := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			process, err := p.acquire(context.Background())
			if err != nil {
				errors <- err
				return
			}
			results <- process
		}()
	}
	wg.Wait()
	close(results)
	close(errors)

	seen := make(map[*process]bool, count)
	for process := range results {
		if seen[process] {
			t.Fatalf("process %p was acquired more than once", process)
		}
		seen[process] = true
		_ = process.Close()
	}
	for err := range errors {
		t.Fatal(err)
	}
	if len(seen) != count {
		t.Fatalf("acquired %d processes, want %d", len(seen), count)
	}
}

func TestProcessPoolRefillsWarmProcessAfterAcquire(t *testing.T) {
	factory, current := testProcessFactory()
	p := newProcessPool(config{WarmProcesses: 1}, factory)
	defer p.close()

	first, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, created := current(); created >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, created := current(); created < 2 {
		t.Fatalf("warm process was not refilled, created %d processes", created)
	}

	second, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second == first {
		t.Fatal("refilled acquire reused the assigned process")
	}
}

func TestProcessPoolReplacesExitedWarmProcess(t *testing.T) {
	factory, current := testProcessFactory()
	p := newProcessPool(config{WarmProcesses: 1}, factory)
	defer p.close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if process, created := current(); process != nil && created == 1 {
			_ = process.Close()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if process, created := current(); process == nil || created != 1 {
		t.Fatalf("initial warm process was not created: process=%p created=%d", process, created)
	}

	for time.Now().Before(deadline) {
		if _, created := current(); created >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, created := current(); created < 2 {
		t.Fatalf("exited warm process was not replaced, created %d processes", created)
	}
}

func TestProcessPoolAcquireContextCancelsWhileCreationIsPending(t *testing.T) {
	baseFactory, _ := testProcessFactory()
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	factory := func(ctx context.Context, cfg config) (*process, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return baseFactory(ctx, cfg)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p := newProcessPool(config{}, factory)
	defer p.close()

	firstResult := make(chan error, 1)
	go func() {
		_, err := p.acquire(context.Background())
		firstResult <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending acquire error = %v, want context deadline", err)
	}

	close(release)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
}

func TestProcessPoolMaxProcessesBlocksInsteadOfCreating(t *testing.T) {
	factory, current := testProcessFactory()
	p := newProcessPool(config{MaxProcesses: 1}, factory)
	defer p.close()

	firstProcess, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	secondResult := make(chan struct {
		process *process
		err     error
	}, 1)
	go func() {
		got, err := p.acquire(context.Background())
		secondResult <- struct {
			process *process
			err     error
		}{process: got, err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for p.waiting.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.waiting.Load() == 0 {
		t.Fatal("second acquire did not start waiting")
	}
	if _, created := current(); created != 1 {
		t.Fatalf("created %d processes, want 1", created)
	}

	_ = firstProcess.Close()
	result := <-secondResult
	if result.err != nil {
		t.Fatal(result.err)
	}
	if _, created := current(); created != 2 {
		t.Fatalf("created %d processes after waiting acquire, want 2", created)
	}
	_ = result.process.Close()
}

func TestProcessPoolWarmZeroCreatesOnlyAfterDemand(t *testing.T) {
	factory, current := testProcessFactory()
	p := newProcessPool(config{WarmProcesses: 0, MaxProcesses: 1}, factory)
	defer p.close()

	if _, created := current(); created != 0 {
		t.Fatalf("created %d processes before demand, want 0", created)
	}
	process, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if _, created := current(); created != 1 {
		t.Fatalf("created %d processes after demand, want 1", created)
	}
}
