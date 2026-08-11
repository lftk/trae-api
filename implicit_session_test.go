package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

func userMessage(text string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: text}
}

func assistantMessage(text string) openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: text}
}

// acquireAndRecord simulates one chat() request: acquire the implicit session
// and, on success, record the transcript fingerprints like http.go does.
func acquireAndRecord(s *server, messages []openai.ChatCompletionMessage, answer string) (*sessionLease, bool, error) {
	lease, continued, err := s.acquireImplicitSession(context.Background(), messages)
	if err == nil {
		recordImplicitFingerprints(lease, messages, answer)
	}
	return lease, continued, err
}

func TestImplicitSessionContinuationReusesSessionAndProcess(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	first := []openai.ChatCompletionMessage{userMessage("hello")}
	firstLease, continued, err := acquireAndRecord(s, first, "hi there")
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("first request of a conversation was reported as continuation")
	}
	firstLease.release()

	second := []openai.ChatCompletionMessage{
		userMessage("hello"), assistantMessage("hi there"), userMessage("what is 1+1?"),
	}
	secondLease, continued, err := acquireAndRecord(s, second, "2")
	if err != nil {
		t.Fatal(err)
	}
	if !continued {
		t.Fatal("request extending the recorded transcript was not continued")
	}
	if secondLease.session != firstLease.session {
		t.Fatalf("continuation reused a different session: %p vs %p", firstLease.session, secondLease.session)
	}
	if process, created := current(); process == nil || created != 1 {
		t.Fatalf("continuation created a new trae process: created=%d", created)
	}
	secondLease.release()

	// A third turn must continue the re-keyed session as well.
	third := []openai.ChatCompletionMessage{
		userMessage("hello"), assistantMessage("hi there"), userMessage("what is 1+1?"), assistantMessage("2"), userMessage("and 2+2?"),
	}
	thirdLease, continued, err := acquireAndRecord(s, third, "4")
	if err != nil {
		t.Fatal(err)
	}
	if !continued || thirdLease.session != firstLease.session {
		t.Fatal("second continuation did not reuse the implicit session")
	}

	firstLease.release()
	if len(s.implicit.sessions) != 1 {
		t.Fatalf("implicit sessions retained: %d, want 1", len(s.implicit.sessions))
	}
}

func TestImplicitSessionReplayedRequestStartsFreshSession(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	messages := []openai.ChatCompletionMessage{userMessage("retry me")}
	firstLease, continued, err := acquireAndRecord(s, messages, "answer")
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("first request was reported as continuation")
	}
	firstSession := firstLease.session
	firstLease.release()

	// The exact same request (retry/regenerate without edits) must not reuse
	// the session: its transcript already contains this prompt.
	secondLease, continued, err := acquireAndRecord(s, messages, "answer")
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("replayed request was reported as continuation")
	}
	if secondLease.session == firstSession {
		t.Fatal("replayed request reused the session whose transcript already contains the prompt")
	}
	if process, created := current(); process == nil || created != 2 {
		t.Fatalf("replayed request did not create a fresh process: created=%d", created)
	}
	select {
	case <-firstSession.process.done:
	default:
		t.Fatal("replaced implicit session process was not closed")
	}
	secondLease.release()
}

func TestImplicitSessionEditedHistoryStartsNewConversation(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	first := []openai.ChatCompletionMessage{userMessage("explain go")}
	firstLease, _, err := acquireAndRecord(s, first, "go is a language")
	if err != nil {
		t.Fatal(err)
	}
	firstLease.release()

	// Same number of turns but edited content: the transcript diverges, so
	// the full history must be replayed on a fresh session.
	edited := []openai.ChatCompletionMessage{userMessage("explain rust")}
	secondLease, continued, err := acquireAndRecord(s, edited, "rust is a language")
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("edited history was mistaken for a continuation")
	}
	if secondLease.session == firstLease.session {
		t.Fatal("edited history reused the old session")
	}
	if process, created := current(); process == nil || created != 2 {
		t.Fatalf("edited history did not create a fresh process: created=%d", created)
	}
	secondLease.release()
}

func TestImplicitSessionTrailingAssistantMessageDoesNotContinue(t *testing.T) {
	factory, _ := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	first := []openai.ChatCompletionMessage{userMessage("hello")}
	firstLease, _, err := acquireAndRecord(s, first, "hi")
	if err != nil {
		t.Fatal(err)
	}
	firstLease.release()

	// A transcript that ends with an assistant message is not a valid
	// continuation target (nothing new to prompt); it must not reuse.
	odd := []openai.ChatCompletionMessage{
		userMessage("hello"), assistantMessage("hi"), assistantMessage("extra"),
	}
	secondLease, continued, err := acquireAndRecord(s, odd, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("trailing-assistant transcript was treated as continuation")
	}
	secondLease.release()
}

func TestImplicitSessionReapedWhenIdle(t *testing.T) {
	factory, _ := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Minute})
	s.newProcess = factory

	lease, _, err := acquireAndRecord(s, []openai.ChatCompletionMessage{userMessage("hi")}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	session := lease.session
	lease.release()

	session.lastUsed = time.Now().Add(-2 * time.Minute)
	s.reapIdleSessions(time.Now())
	if len(s.implicit.sessions) != 0 {
		t.Fatalf("idle implicit session remained: %d", len(s.implicit.sessions))
	}
	select {
	case <-session.process.done:
	default:
		t.Fatal("idle implicit session process was not closed")
	}
}

func TestImplicitSessionDisabledWithZeroTimeout(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: 0})
	s.newProcess = factory

	// With implicit sessions disabled, anonymous requests keep the historical
	// behavior: a temporary session closed when the request ends.
	lease, err := s.acquireSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	if len(s.implicit.sessions) != 0 {
		t.Fatal("implicit manager retained sessions while disabled")
	}
	if process, created := current(); process == nil || created != 1 {
		t.Fatalf("unexpected process state: created=%d", created)
	}
	select {
	case <-lease.session.process.done:
	default:
		t.Fatal("temporary session was not closed with implicit sessions disabled")
	}
}

func TestImplicitSessionDifferentFingerprintsDoNotCollide(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	one := []openai.ChatCompletionMessage{userMessage("one")}
	two := []openai.ChatCompletionMessage{userMessage("two")}
	oneLease, _, err := acquireAndRecord(s, one, "1")
	if err != nil {
		t.Fatal(err)
	}
	twoLease, continued, err := acquireAndRecord(s, two, "2")
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("different first messages collided on continuation")
	}
	if oneLease.session == twoLease.session {
		t.Fatal("different conversations shared an implicit session")
	}
	if process, created := current(); process == nil || created != 2 {
		t.Fatalf("created %d processes, want 2", created)
	}
	oneLease.release()
	twoLease.release()
}

func TestImplicitSessionContinuationIgnoresOtherConversations(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	one := []openai.ChatCompletionMessage{userMessage("alpha")}
	oneLease, _, err := acquireAndRecord(s, one, "a")
	if err != nil {
		t.Fatal(err)
	}
	oneLease.release()

	two := []openai.ChatCompletionMessage{userMessage("beta")}
	twoLease, _, err := acquireAndRecord(s, two, "b")
	if err != nil {
		t.Fatal(err)
	}

	// Continuing conversation "one" must not pick up conversation "two".
	oneNext := []openai.ChatCompletionMessage{
		userMessage("alpha"), assistantMessage("a"), userMessage("gamma"),
	}
	oneNextLease, continued, err := acquireAndRecord(s, oneNext, "g")
	if err != nil {
		t.Fatal(err)
	}
	if !continued {
		t.Fatal("valid continuation was not recognized among other conversations")
	}
	if oneNextLease.session != oneLease.session {
		t.Fatal("continuation picked the wrong conversation's session")
	}
	if process, created := current(); process == nil || created != 2 {
		t.Fatalf("continuation created a new process: created=%d", created)
	}
	oneLease.release()
	twoLease.release()
	oneNextLease.release()
}

func TestImplicitSessionContinuationSkipsInFlightSession(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	first := []openai.ChatCompletionMessage{userMessage("hello")}
	firstLease, _, err := acquireAndRecord(s, first, "hi")
	if err != nil {
		t.Fatal(err)
	}
	firstLease.release()

	// The continuation is acquired but its request is still in flight
	// (lease held, fingerprints not recorded yet).
	second := []openai.ChatCompletionMessage{
		userMessage("hello"), assistantMessage("hi"), userMessage("1+1?"),
	}
	secondLease, continued, err := s.acquireImplicitSession(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if !continued {
		t.Fatal("valid continuation was not recognized")
	}

	// An identical request arriving mid-flight must not share the leased
	// session's transcript (fingerprint collision between clients).
	thirdLease, continued, err := s.acquireImplicitSession(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("in-flight session was used as continuation target")
	}
	if thirdLease.session == secondLease.session {
		t.Fatal("in-flight session transcript was shared")
	}
	if process, created := current(); process == nil || created != 2 {
		t.Fatalf("created %d processes, want 2", created)
	}

	// The replaced in-flight session closes once its request releases it.
	secondLease.release()
	select {
	case <-secondLease.session.process.done:
	default:
		t.Fatal("replaced in-flight session process was not closed after lease drained")
	}
	thirdLease.release()
}

func TestImplicitSessionInFlightDuplicateDoesNotShare(t *testing.T) {
	factory, current := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	messages := []openai.ChatCompletionMessage{userMessage("hello")}
	firstLease, _, err := acquireAndRecord(s, messages, "hi")
	if err != nil {
		t.Fatal(err)
	}

	// A second client sends the identical first message while the first
	// request is still in flight: it must get its own session, not share
	// the leased transcript.
	dupLease, continued, err := s.acquireImplicitSession(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if continued {
		t.Fatal("duplicate first message was reported as continuation")
	}
	if dupLease.session == firstLease.session {
		t.Fatal("duplicate first message shared the in-flight session")
	}
	if process, created := current(); process == nil || created != 2 {
		t.Fatalf("created %d processes, want 2", created)
	}

	firstLease.release()
	select {
	case <-firstLease.session.process.done:
	default:
		t.Fatal("replaced in-flight session process was not closed after lease drained")
	}
	dupLease.release()
}

func TestHealthzReportsSessionCounts(t *testing.T) {
	factory, _ := testProcessFactory()
	s := newServer(config{WorkdirTemp: true, ImplicitIdleTimeout: time.Hour})
	s.newProcess = factory

	lease, _, err := acquireAndRecord(s, []openai.ChatCompletionMessage{userMessage("hi")}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("healthz status = %v", body["status"])
	}
	if got := body["implicit_sessions"]; got != float64(1) {
		t.Fatalf("implicit_sessions = %v, want 1", got)
	}
	if got := body["sessions"]; got != float64(0) {
		t.Fatalf("sessions = %v, want 0", got)
	}
	lease.release()
}
