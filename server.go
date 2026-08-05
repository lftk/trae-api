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
	cfg            config
	mu             sync.Mutex
	sessions       map[string]*session
	pending        map[string]*sessionCreation
	process        *process
	processPending *processCreation
	newProcess     func(context.Context, config) (*process, error)
	stopScan       chan struct{}
	scanDone       chan struct{}
	stopOnce       sync.Once
}

type processCreation struct {
	done    chan struct{}
	process *process
	err     error
}

type sessionCreation struct {
	done    chan struct{}
	session *session
	err     error
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
		cfg:        cfg,
		sessions:   make(map[string]*session),
		pending:    make(map[string]*sessionCreation),
		newProcess: startProcess,
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

func (s *server) acquireSession(ctx context.Context, id string) (*sessionLease, error) {
	if id == "" {
		process, err := s.ensureProcess(ctx)
		if err != nil {
			return nil, err
		}
		session, err := process.newSession(ctx)
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
	s.mu.Lock()
	if session := s.sessions[id]; session != nil {
		select {
		case <-session.process.done:
			delete(s.sessions, id)
		default:
			session.mu.Lock()
			session.touchLocked()
			session.mu.Unlock()
			s.mu.Unlock()
			return &sessionLease{session: session, id: id, release: func() {}}, nil
		}
	}
	if creation := s.pending[id]; creation != nil {
		s.mu.Unlock()
		select {
		case <-creation.done:
			if creation.err != nil {
				return nil, creation.err
			}
			return &sessionLease{session: creation.session, id: id, release: func() {}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	creation := &sessionCreation{done: make(chan struct{})}
	s.pending[id] = creation
	s.mu.Unlock()

	process, err := s.ensureProcess(ctx)
	var session *session
	if err == nil {
		session, err = process.newSession(ctx)
	}

	s.mu.Lock()
	delete(s.pending, id)
	creation.session = session
	creation.err = err
	if err != nil {
		close(creation.done)
		s.mu.Unlock()
		return nil, err
	}
	s.sessions[id] = session
	close(creation.done)
	s.mu.Unlock()
	return &sessionLease{session: session, id: id, release: func() {}}, nil
}

func (s *server) ensureProcess(ctx context.Context) (*process, error) {
	s.mu.Lock()
	if s.process != nil {
		select {
		case <-s.process.done:
			s.process = nil
		default:
			p := s.process
			s.mu.Unlock()
			return p, nil
		}
	}
	if creation := s.processPending; creation != nil {
		s.mu.Unlock()
		select {
		case <-creation.done:
			return creation.process, creation.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	creation := &processCreation{done: make(chan struct{})}
	s.processPending = creation
	createProcess := s.newProcess
	s.mu.Unlock()

	p, err := createProcess(ctx, s.cfg)
	s.mu.Lock()
	s.processPending = nil
	creation.process, creation.err = p, err
	if err == nil {
		s.process = p
	}
	close(creation.done)
	s.mu.Unlock()
	if err == nil {
		p.setOnDone(func() { s.handleProcessDeath(p) })
	}
	return p, err
}

func (s *server) handleProcessDeath(process *process) {
	s.mu.Lock()
	if s.process == process {
		s.process = nil
		for id := range s.sessions {
			delete(s.sessions, id)
		}
		slog.Error("shared trae ACP process failed; sessions invalidated")
	}
	s.mu.Unlock()
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
	s.mu.Lock()
	var toClose []*session
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
	sessions := make([]*session, 0, len(s.sessions))
	for id, session := range s.sessions {
		sessions = append(sessions, session)
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	for _, session := range sessions {
		_ = session.Close()
	}
	s.mu.Lock()
	process := s.process
	s.process = nil
	s.mu.Unlock()
	if process != nil {
		_ = process.Close()
	}
}
