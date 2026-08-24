package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type server struct {
	cfg         config
	sessions    *sessionManager
	implicit    *sessionManager
	store       *sessionStore
	processPool *processPool
	newProcess  func(context.Context, config) (*process, error)
	stopScan    chan struct{}
	scanDone    chan struct{}
	stopOnce    sync.Once
}

type sessionLease struct {
	session  *session
	id       string
	implicit bool
	release  func()
}

type modelListResponse struct {
	Object string         `json:"object"`
	Data   []openai.Model `json:"data"`
}

func newServer(cfg config) *server {
	s := &server{
		cfg:      cfg,
		sessions: newSessionManager(cfg.SessionIdleTimeout),
		implicit: newSessionManager(cfg.ImplicitIdleTimeout),
		store:    newSessionStore(cfg.StateDir, cfg.SessionIdleTimeout),
		stopScan: make(chan struct{}),
		scanDone: make(chan struct{}),
	}
	s.store.pruneExpired(time.Now())
	s.newProcess = startProcess
	// The pool resolves the factory through the field so tests can swap in a
	// fake process factory after construction.
	s.processPool = newProcessPool(cfg, func(ctx context.Context, cfg config) (*process, error) {
		return s.newProcess(ctx, cfg)
	})
	if cfg.SessionIdleTimeout > 0 && cfg.SessionScanInterval > 0 {
		go s.scanIdleSessions()
	} else {
		close(s.scanDone)
	}
	return s
}

func (s *server) acquireSession(ctx context.Context, id string) (*sessionLease, error) {
	if id == "" {
		// Implicit sessions are enabled: chat() routes anonymous requests
		// through acquireImplicitSession. This branch only serves the
		// disabled configuration and keeps the historical behavior of
		// closing the temporary session as soon as the request ends.
		session, err := s.createSession(ctx, "")
		if err != nil {
			return nil, err
		}
		var once sync.Once
		return &sessionLease{
			session: session,
			release: func() {
				once.Do(func() {
					if err := session.Close(); err != nil {
						slog.Warn("close temporary ACP session", "sessionid", session.sessionID(), "error", err)
					}
				})
			},
		}, nil
	}
	session, err := s.sessions.getOrCreate(ctx, id, s.cfg.MaxSessions, func(ctx context.Context) (*session, error) {
		return s.createSession(ctx, id)
	}, s.handleSessionDeath)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return &sessionLease{session: session, id: id, release: func() {
		once.Do(func() { s.sessions.release(id, session) })
	}}, nil
}

// acquireImplicitSession maps an anonymous request to a live implicit session
// when the request continues that session's transcript (its message prefix
// hashes to the session's recorded transcript fingerprint). Otherwise it
// starts a new implicit session. It reports whether the caller may prompt
// with only the trailing user message instead of the full history.
func (s *server) acquireImplicitSession(ctx context.Context, messages []openai.ChatCompletionMessage) (*sessionLease, bool, error) {
	fp := fingerprintMessages(messages)
	last := messages[len(messages)-1]
	canContinue := len(messages) >= 2 && last.Role == openai.ChatMessageRoleUser
	if canContinue {
		prefixFP := fingerprintMessages(messages[:len(messages)-1])
		if session, err := s.implicit.takeContinuation(prefixFP, fp); err != nil {
			return nil, false, err
		} else if session != nil {
			return implicitLease(session, s.implicit), true, nil
		}
	}
	session, err := s.createImplicitSession(ctx, formatFingerprint(fp))
	if err != nil {
		return nil, false, err
	}
	return implicitLease(session, s.implicit), false, nil
}

// createImplicitSession always starts a fresh session and never shares an
// existing entry under the same fingerprint key. Replaying the exact previous
// request (retry or regenerate without edits) replaces the idle session whose
// transcript already contains that prompt; an entry still leased by an
// in-flight request is replaced as well and closes once that request releases
// it (see releaseBySession). Two clients with identical conversations can
// therefore never prompt into the same transcript.
func (s *server) createImplicitSession(ctx context.Context, key string) (*session, error) {
	if s.cfg.MaxSessions > 0 && s.implicit.count() >= s.cfg.MaxSessions {
		return nil, errSessionLimit
	}
	sess, err := s.createSession(ctx, "")
	if err != nil {
		return nil, err
	}
	var replaced *session
	s.implicit.mu.Lock()
	if old := s.implicit.sessions[key]; old != nil {
		delete(s.implicit.sessions, key)
		replaced = old
	}
	s.implicit.sessions[key] = sess
	sess.leases = 1
	s.implicit.mu.Unlock()
	if replaced != nil && replaced.leases == 0 {
		slog.Info("implicit session replaced for replayed request", "acpsessionid", replaced.sessionID())
		if err := replaced.Close(); err != nil {
			slog.Warn("close replaced implicit session", "sessionid", replaced.sessionID(), "error", err)
		}
	}
	sess.process.addOnDone(func() { s.handleImplicitDeath(key, sess) })
	return sess, nil
}

func implicitLease(session *session, manager *sessionManager) *sessionLease {
	var once sync.Once
	session.mu.Lock()
	id := formatFingerprint(session.lastUserFP)
	session.mu.Unlock()
	return &sessionLease{
		session:  session,
		id:       id,
		implicit: true,
		release: func() {
			once.Do(func() { manager.releaseBySession(session) })
		},
	}
}

// createSession acquires a process and either resumes a persisted ACP session
// (calling trae-cli's session/load) or creates a fresh one. For non-empty
// externalID it persists (or overwrites) the external-ID -> ACP session mapping
// so the session can be resumed after a restart. Stores are best-effort: a
// failure to read or resume a mapping degrades to a fresh session.
func (s *server) createSession(ctx context.Context, externalID string) (*session, error) {
	p, err := s.processPool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if externalID != "" && p.loadSupported {
		if rec, _ := s.store.loadRecord(externalID); rec != nil {
			if session, err := p.loadSession(ctx, rec.AcpSessionID, rec.Cwd); err == nil {
				return session, nil
			} else {
				slog.Warn("resume ACP session failed; recreating", "external_id", externalID, "acpsessionid", rec.AcpSessionID, "error", err)
				s.store.deleteRecord(externalID)
			}
		}
	}
	session, err := p.newSession(ctx)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	if externalID != "" {
		now := time.Now()
		if err := s.store.storeRecord(&sessionRecord{
			ExternalID:   externalID,
			AcpSessionID: session.sessionID(),
			Cwd:          p.workdir,
			Model:        session.currentModel,
			CreatedAt:    now,
			LastUsedAt:   now,
		}); err != nil {
			slog.Warn("persist session mapping", "external_id", externalID, "error", err)
		}
	}
	return session, nil
}

func (s *server) handleSessionDeath(id string, session *session) {
	s.sessions.mu.Lock()
	if s.sessions.sessions[id] == session {
		delete(s.sessions.sessions, id)
		slog.Error("trae ACP process failed; session invalidated", "sessionid", id, "acpsessionid", session.sessionID())
	}
	s.sessions.mu.Unlock()
}

func (s *server) handleImplicitDeath(id string, session *session) {
	s.implicit.mu.Lock()
	if s.implicit.sessions[id] == session {
		delete(s.implicit.sessions, id)
		slog.Error("trae ACP process failed; implicit session invalidated", "acpsessionid", session.sessionID())
	}
	s.implicit.mu.Unlock()
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	models, err := s.listModels(r.Context())
	if err != nil {
		slog.Warn("list trae models", "error", err)
		writeError(w, http.StatusBadGateway, "list trae models: "+err.Error())
		return
	}
	data := make([]openai.Model, 0, len(models))
	for _, model := range models {
		data = append(data, openai.Model{ID: model, Object: "model", OwnedBy: "trae"})
	}
	writeJSONOrLog(w, http.StatusOK, modelListResponse{Object: "list", Data: data})
}

// listModels queries trae-cli for the available models, preferring the JSON
// output format with a line-based fallback.
func (s *server) listModels(ctx context.Context) ([]string, error) {
	workdir, err := resolveWorkdir(s.cfg.Workdir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.TraeBin, "models")
	cmd.Dir = workdir
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseModels(output)
}

func parseModels(output []byte) ([]string, error) {
	var decoded []openai.Model
	if err := json.Unmarshal(output, &decoded); err == nil {
		models := make([]string, 0, len(decoded))
		for _, item := range decoded {
			if item.ID != "" {
				models = append(models, item.ID)
			}
		}
		return models, nil
	}
	var models []string
	for _, line := range strings.Split(string(output), "\n") {
		if model := strings.TrimSpace(line); model != "" {
			models = append(models, model)
		}
	}
	return models, nil
}

func (s *server) scanIdleSessions() {
	defer close(s.scanDone)
	ticker := time.NewTicker(s.cfg.SessionScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.reapIdleSessions(time.Now())
		case <-s.stopScan:
			return
		}
	}
}

func (s *server) reapIdleSessions(now time.Time) {
	s.reapManager(now, s.sessions, s.store.deleteRecord)
	if s.cfg.ImplicitIdleTimeout > 0 {
		s.reapManager(now, s.implicit, nil)
	}
}

func (s *server) reapManager(now time.Time, manager *sessionManager, deleteByID func(string)) {
	manager.mu.Lock()
	var toClose []*session
	var idsForStore []string
	for id, session := range manager.sessions {
		if session.leases != 0 {
			continue
		}
		session.mu.Lock()
		idle := now.Sub(session.lastUsed) > manager.idleTimeout
		session.mu.Unlock()
		if idle {
			toClose = append(toClose, session)
			delete(manager.sessions, id)
			if deleteByID != nil {
				idsForStore = append(idsForStore, id)
			}
		}
	}
	manager.mu.Unlock()
	for _, id := range idsForStore {
		deleteByID(id)
	}
	for _, session := range toClose {
		_ = session.Close()
	}
}

func (s *server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout())
	defer cancel()
	s.shutdownWithContext(ctx)
}

func (s *server) shutdownTimeout() time.Duration {
	if s.cfg.ShutdownTimeout <= 0 {
		return defaultCloseTimeout
	}
	return s.cfg.ShutdownTimeout
}

func (s *server) shutdownWithContext(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.stopScan) })
	select {
	case <-s.scanDone:
	case <-ctx.Done():
	}
	s.closeSessions(ctx, s.sessions)
	s.closeSessions(ctx, s.implicit)
	s.processPool.closeWithContext(ctx)
}

func (s *server) closeSessions(ctx context.Context, manager *sessionManager) {
	manager.mu.Lock()
	sessions := make([]*session, 0, len(manager.sessions))
	for id, session := range manager.sessions {
		sessions = append(sessions, session)
		delete(manager.sessions, id)
	}
	manager.mu.Unlock()
	for _, session := range sessions {
		_ = session.closeWithContext(ctx)
	}
}
