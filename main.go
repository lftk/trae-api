package main

import (
	"context"
	"errors"

	"fmt"

	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"

	"sync"
	"syscall"
	"time"
)

// version is overridden at build time via
// -ldflags "-X main.version=vX.Y.Z" (e.g. by the Homebrew formula).
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newCommand().Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCurrent(ctx context.Context, cfg config) error {
	setupLogger(false)
	s, err := newServer(cfg)
	if err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		return fmt.Errorf("start API listener: %w", err)
	}
	defer listener.Close()

	server := s.httpServer()

	slog.Info(
		"trae-api listening",
		"addr", listener.Addr().String(),
		"upstream", cfg.BaseURL,
		"login", "run trae-api login",
	)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(listener)
	}()

	if err := waitForHTTP(ctx, serverErr, func() error {
		return s.shutdownHTTP(server, cfg.ShutdownTimeout)
	}); err != nil {
		return fmt.Errorf("http server stopped: %w", err)
	}
	return nil
}

func waitForHTTP(ctx context.Context, serverErr <-chan error, shutdown func() error) error {
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining...")
		shutdownErr := shutdown()
		serveErr := <-serverErr
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown failed: %w", errors.Join(shutdownErr, serveErr))
		}
		return serveErr
	}
	return nil
}

func setupLogger(debug bool) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	if debug {
		level.Set(slog.LevelDebug)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}

// Only Addr and APIToken are user configuration. Other fields are internal
// protocol constants/test seams, not environment-configurable options.
type config struct {
	Addr     string
	AuthAddr string
	APIToken string

	BaseURL         string
	AuthBaseURL     string
	AuthURL         string
	ClientID        string
	IDEVersion      string
	IDEVersionCode  string
	MaxConcurrent   int
	QueueTimeout    time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
}

func defaultConfig() config {
	return config{
		Addr:     "127.0.0.1:8723",
		AuthAddr: "127.0.0.1:8765",

		BaseURL:         "https://trae-api-cn.mchost.guru",
		AuthBaseURL:     "https://api.trae.cn",
		AuthURL:         "https://www.trae.cn/authorization",
		ClientID:        "ono9krqynydwx5",
		IDEVersion:      "3.3.65",
		IDEVersionCode:  "20260401",
		MaxConcurrent:   2,
		QueueTimeout:    time.Minute,
		RequestTimeout:  5 * time.Minute,
		ShutdownTimeout: 30 * time.Second,
	}
}
func isLoopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	return err == nil && (h == "localhost" || net.ParseIP(h).IsLoopback())
}

func validateAPIConfig(c config) error {
	host, port, err := net.SplitHostPort(c.Addr)
	n, parseErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || n < 0 || n > 65535 {
		return errors.New("API address must be host:port with port 0..65535")
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return errors.New("API address must use localhost or a literal IP")
	}
	if !isLoopbackAddr(c.Addr) && c.APIToken == "" {
		return errors.New("TRAE_API_TOKEN is required for non-loopback listening")
	}
	return nil
}

type server struct {
	cfg      config
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
func badRequest(s string) error {
	return &apiError{
		400,
		s,
	}
}
func newServer(cfg config) (*server, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{
		cfg:    cfg,
		slots:  make(chan struct{}, cfg.MaxConcurrent),
		ctx:    ctx,
		cancel: cancel,
	}
	s.upstream = &traeClient{
		cfg:  cfg,
		http: newHTTPClient(cfg.RequestTimeout),
	}
	return s, nil
}

// listenAPI binds the API only; Auth owns its own loopback callback listener.
func (s *server) listenAPI() (net.Listener, error) {
	if err := validateAPIConfig(s.cfg); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

func (s *server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.routes(),
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    128 << 10,
	}
}

func (s *server) shutdownHTTP(server *http.Server, timeout time.Duration) error {
	// Cancel streams and callbacks before waiting for active requests to drain.
	s.shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
		return err
	}
	return nil
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
		return &apiError{
			503,
			"request queue timed out",
		}
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
	snapshot, err := readAPICredential()
	if err != nil {
		return r, completion{}, err
	}
	g := snapshotIdentity(snapshot)
	c := snapshot
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

	if emit != nil {
		if err = emit(delta{
			Start: true,
			Model: r.Model,
		}); err != nil {
			return r, completion{}, err
		}
	}
	started := time.Now()
	result, err := s.upstream.prompt(ctx, c, g, r, newID("request_"), emit)
	slog.Debug("Trae request finished", "model", r.Model, "elapsed", time.Since(started), "success", err == nil)
	return r, result, err
}
func (s *server) shutdown() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.upstream.http.CloseIdleConnections()

	})
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
