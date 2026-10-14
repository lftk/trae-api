package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type server struct {
	cfg      config
	auth     *authManager
	upstream *traeClient
	slots    chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
}
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }
func badRequest(s string) error   { return &apiError{400, s} }
func newServer(cfg config) (*server, error) {
	auth, err := newAuthManager(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{cfg: cfg, auth: auth, slots: make(chan struct{}, cfg.MaxConcurrent), ctx: ctx, cancel: cancel}
	s.upstream = &traeClient{cfg: cfg, http: newHTTPClient(cfg.RequestTimeout), auth: auth}
	return s, nil
}
func acquireGate(ctx context.Context, gate chan struct{}, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return &apiError{503, "request queue timed out"}
	}
}
func (s *server) execute(ctx context.Context, r chatRequest, emit func(delta) error) (chatRequest, completion, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return r, completion{}, err
	}
	if err := validateRequest(&r); err != nil {
		return r, completion{}, badRequest(err.Error())
	}
	if err := validateToolHistory(r.Messages); err != nil {
		return r, completion{}, badRequest(err.Error())
	}
	if err := acquireGate(ctx, s.slots, s.cfg.QueueTimeout); err != nil {
		return r, completion{}, err
	}
	defer func() { <-s.slots }()
	c, g, err := s.auth.get(ctx)
	if err != nil {
		return r, completion{}, err
	}
	if r.Model == "" || r.Model == "auto" {
		models, e := s.upstream.modelNames(ctx, c, g)
		if e != nil {
			return r, completion{}, e
		}
		r.Model = models[0]
	}
	// Discover and validate explicit models before emitting a streaming start.
	// Otherwise an unknown model commits HTTP 200 and an assistant chunk.
	if _, err := s.upstream.resolveRoute(ctx, c, g, r.Model); err != nil {
		return r, completion{}, err
	}
	latest, latestGeneration, err := s.auth.get(ctx)
	if err != nil {
		return r, completion{}, err
	}
	if latestGeneration != g {
		return r, completion{}, errLoginRequired
	}
	c = latest
	if emit != nil {
		if err = emit(delta{Start: true, Model: r.Model, Tools: r.Tools}); err != nil {
			return r, completion{}, err
		}
	}
	started := time.Now()
	result, err := s.upstream.prompt(ctx, c, g, r, newID("request_"), emit)
	slog.Debug("Trae request finished", "model", r.Model, "elapsed", time.Since(started), "success", err == nil)
	if err == nil && !s.auth.validGeneration(g) {
		err = errLoginRequired
	}
	return r, result, err
}
func (s *server) shutdown() {
	s.stopOnce.Do(func() { s.cancel(); s.upstream.http.CloseIdleConnections(); s.auth.client.CloseIdleConnections() })
}
func statusForError(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	if errors.Is(err, errLoginRequired) {
		return 401
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504
	}
	return 502
}
