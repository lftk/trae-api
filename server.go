package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type server struct {
	cfg        config
	mu         sync.Mutex
	sessions   map[string]*traeSession
	pending    map[string]*sessionCreation
	newSession func(context.Context, config) (*traeSession, error)
	nextID     uint64
	stopScan   chan struct{}
	scanDone   chan struct{}
	stopOnce   sync.Once
}

type sessionCreation struct {
	done    chan struct{}
	session *traeSession
	err     error
}
type modelListResponse struct {
	Object string         `json:"object"`
	Data   []openai.Model `json:"data"`
}

func newServer(cfg config) *server {
	s := &server{
		cfg:        cfg,
		sessions:   make(map[string]*traeSession),
		pending:    make(map[string]*sessionCreation),
		newSession: newSession,
		stopScan:   make(chan struct{}),
		scanDone:   make(chan struct{}),
	}
	if cfg.SessionIdleTimeout > 0 && cfg.SessionScanInterval > 0 {
		go s.scanIdleSessions()
	} else {
		close(s.scanDone)
	}
	return s
}

func (s *server) session(ctx context.Context, id string) (*traeSession, string, error) {
	s.mu.Lock()
	if id != "" {
		if session := s.sessions[id]; session != nil {
			select {
			case <-session.done:
				delete(s.sessions, id)
			default:
				session.mu.Lock()
				session.touchLocked()
				session.mu.Unlock()
				s.mu.Unlock()
				return session, id, nil
			}
		}
	} else {
		id = s.newSessionIDLocked()
	}
	if creation := s.pending[id]; creation != nil {
		s.mu.Unlock()
		select {
		case <-creation.done:
			return creation.session, id, creation.err
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	creation := &sessionCreation{done: make(chan struct{})}
	s.pending[id] = creation
	createSession := s.newSession
	s.mu.Unlock()

	session, err := createSession(ctx, s.cfg)

	s.mu.Lock()
	delete(s.pending, id)
	creation.session = session
	creation.err = err
	if err != nil {
		close(creation.done)
		s.mu.Unlock()
		return nil, "", err
	}
	s.sessions[id] = session
	close(creation.done)
	s.mu.Unlock()
	session.setOnDone(func() { s.handleSessionDeath(id) })
	return session, id, nil
}

func (s *server) handleSessionDeath(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return
	}
	select {
	case <-session.done:
		delete(s.sessions, id)
	default:
	}
}

func (s *server) newSessionIDLocked() string {
	for {
		s.nextID++
		id := fmt.Sprintf("sess_%d", s.nextID)
		if s.sessions[id] == nil && s.pending[id] == nil {
			return id
		}
	}
}

func (s *server) models(w http.ResponseWriter, r *http.Request) {
	models := []string{s.cfg.DefaultModel}
	workdir, err := resolveWorkdir(s.cfg.Workdir)
	if err != nil {
		slog.Warn("resolve workdir for models; using default model", "error", err)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.cfg.TraeBin, "models")
		cmd.Dir = workdir
		if output, err := cmd.Output(); err == nil {
			if parsed, parseErr := parseModels(output, s.cfg.DefaultModel); parseErr == nil {
				models = parsed
			} else {
				slog.Warn("parse trae models; using default model", "error", parseErr)
			}
		} else {
			slog.Warn("list trae models; using default model", "error", err)
		}
	}
	data := make([]openai.Model, 0, len(models))
	for _, model := range models {
		data = append(data, openai.Model{ID: model, Object: "model", OwnedBy: "trae"})
	}
	writeJSONOrLog(w, http.StatusOK, modelListResponse{Object: "list", Data: data})
}

func parseModels(output []byte, fallback string) ([]string, error) {
	var decoded []openai.Model
	if json.Unmarshal(output, &decoded) == nil && len(decoded) > 0 {
		models := make([]string, 0, len(decoded))
		for _, item := range decoded {
			if item.ID != "" {
				models = append(models, item.ID)
			}
		}
		if len(models) > 0 {
			return models, nil
		}
	}
	var models []string
	for _, line := range strings.Split(string(output), "\n") {
		if model := strings.TrimSpace(line); model != "" {
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return []string{fallback}, nil
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
	s.mu.Lock()
	var toClose []*traeSession
	for id, session := range s.sessions {
		session.mu.Lock()
		idle := now.Sub(session.lastUsed) > s.cfg.SessionIdleTimeout
		session.mu.Unlock()
		if idle {
			toClose = append(toClose, session)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, session := range toClose {
		_ = session.Close()
	}
}

func (s *server) shutdown() {
	s.stopOnce.Do(func() { close(s.stopScan) })
	scanTimeout := s.cfg.ShutdownTimeout
	if scanTimeout <= 0 {
		scanTimeout = 5 * time.Second
	}
	select {
	case <-s.scanDone:
	case <-time.After(scanTimeout):
	}
	s.mu.Lock()
	sessions := make([]*traeSession, 0, len(s.sessions))
	for id, session := range s.sessions {
		sessions = append(sessions, session)
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	for _, session := range sessions {
		_ = session.Close()
	}
}
