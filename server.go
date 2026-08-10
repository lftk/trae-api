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
	processPool *processPool
	newProcess  func(context.Context, config) (*process, error)
	stopScan    chan struct{}
	scanDone    chan struct{}
	stopOnce    sync.Once
}

type sessionLease struct {
	session *session
	id      string
	release func()
}

type modelListResponse struct {
	Object string         `json:"object"`
	Data   []openai.Model `json:"data"`
}

func newServer(cfg config) *server {
	s := &server{
		cfg:      cfg,
		sessions: newSessionManager(),
		stopScan: make(chan struct{}),
		scanDone: make(chan struct{}),
	}
	s.newProcess = startProcess
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
		session, err := s.createSession(ctx)
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
	session, err := s.sessions.getOrCreate(ctx, id, s.cfg.MaxSessions, s.createSession, s.handleSessionDeath)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return &sessionLease{session: session, id: id, release: func() {
		once.Do(func() { s.sessions.release(id, session) })
	}}, nil
}

func (s *server) createSession(ctx context.Context) (*session, error) {
	p, err := s.processPool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	session, err := p.newSession(ctx)
	if err != nil {
		_ = p.Close()
		return nil, err
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

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	var models []string
	workdir, err := resolveWorkdir(s.cfg.Workdir)
	if err != nil {
		slog.Warn("resolve workdir for models", "error", err)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.cfg.TraeBin, "models")
		cmd.Dir = workdir
		if output, err := cmd.Output(); err == nil {
			if parsed, parseErr := parseModels(output); parseErr == nil {
				models = parsed
			} else {
				slog.Warn("parse trae models", "error", parseErr)
			}
		} else {
			slog.Warn("list trae models", "error", err)
		}
	}
	data := make([]openai.Model, 0, len(models))
	for _, model := range models {
		data = append(data, openai.Model{ID: model, Object: "model", OwnedBy: "trae"})
	}
	writeJSONOrLog(w, http.StatusOK, modelListResponse{Object: "list", Data: data})
}

func parseModels(output []byte) ([]string, error) {
	var decoded []openai.Model
	if json.Unmarshal(output, &decoded) == nil {
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
	if len(models) == 0 {
		return []string{}, nil
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
	s.sessions.mu.Lock()
	var toClose []*session
	for id, session := range s.sessions.sessions {
		if session.leases != 0 {
			continue
		}
		session.mu.Lock()
		idle := now.Sub(session.lastUsed) > s.cfg.SessionIdleTimeout
		session.mu.Unlock()
		if idle {
			toClose = append(toClose, session)
			delete(s.sessions.sessions, id)
		}
	}
	s.sessions.mu.Unlock()
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
		return 5 * time.Second
	}
	return s.cfg.ShutdownTimeout
}

func (s *server) shutdownWithContext(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.stopScan) })
	select {
	case <-s.scanDone:
	case <-ctx.Done():
	}
	s.sessions.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions.sessions))
	for id, session := range s.sessions.sessions {
		sessions = append(sessions, session)
		delete(s.sessions.sessions, id)
	}
	s.sessions.mu.Unlock()
	for _, session := range sessions {
		_ = session.closeWithContext(ctx)
	}
	s.processPool.closeWithContext(ctx)
}
