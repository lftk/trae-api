package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is overridden at build time via
// -ldflags "-X main.version=vX.Y.Z" (e.g. by the Homebrew formula).
var version = "dev"

func main() {
	action, code := parseCommand(os.Args[1:], os.Stdout, os.Stderr)
	if action == commandExit {
		os.Exit(code)
	}
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	if action == commandLogin {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runLogin(ctx, cfg, os.Stdout, openOfficialBrowser, 500*time.Millisecond, 10*time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	setupLogger(false)
	s, err := newServer(cfg)
	if err != nil {
		slog.Error("initialize service", "error", err)
		os.Exit(1)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		slog.Error("start API listener", "error", err)
		os.Exit(1)
	}
	defer listener.Close()

	server := s.httpServer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		slog.Error("http server stopped", "error", err)
		s.shutdown()
		os.Exit(1)
	}
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

type commandAction uint8

const (
	commandServe commandAction = iota
	commandLogin
	commandExit
)

// parseCommand dispatches once, then lets each command's FlagSet parse flags.
// Exit codes are 0 for help/version, 2 for invalid arguments.
func parseCommand(args []string, stdout, stderr io.Writer) (commandAction, int) {
	action := commandServe
	name := "trae-api"
	if len(args) > 0 && args[0] == "login" {
		action = commandLogin
		name = "trae-api login"
		args = args[1:]
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var showVersion *bool
	if action == commandServe {
		showVersion = fs.Bool("version", false, "print version and exit")
	}
	usage := func(w io.Writer) {
		if action == commandLogin {
			writeLoginUsage(w)
		} else {
			writeUsage(fs, w)
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return commandExit, 0
		}
		fmt.Fprintln(stderr, err)
		usage(stderr)
		return commandExit, 2
	}
	if fs.NArg() != 0 {
		if action == commandLogin {
			fmt.Fprintln(stderr, "login accepts no additional arguments")
		} else {
			fmt.Fprintln(stderr, "unknown command; use trae-api or trae-api login")
		}
		return commandExit, 2
	}
	if action == commandServe && *showVersion {
		fmt.Fprintf(stdout, "trae-api %s\n", version)
		return commandExit, 0
	}
	return action, 0
}

func writeLoginUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: trae-api login\nLog in to the running service via official browser authorization (10 minute timeout).\nUse the same TRAE_API_ADDR and TRAE_API_TOKEN as the service. Ctrl+C cancels.")
}

func writeUsage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintf(w, `trae-api %s

OpenAI-compatible HTTP gateway for Trae CN (direct HTTP).

Usage:
  trae-api [flags]        Start the API service
  trae-api login          Log in to the running service

Configuration is read from TRAE_API_* environment variables; see the
project README for the full list. Highlights:

  TRAE_API_ADDR          listen address (default 127.0.0.1:8723)
  TRAE_API_TOKEN         API token, required for wildcard remote listening
  Use 0.0.0.0 or [::] with a token for remote access; single-interface non-loopback binds are unsupported.
  Authorization callback uses the API listener port on loopback.
  Credentials exist only in memory; restart requires login again.

Flags:
`, version)
	fs.SetOutput(w)
	fs.PrintDefaults()
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
		Addr: "127.0.0.1:8723",

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
func loadConfig() (config, error) {
	c := defaultConfig()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TRAE_API_") && key != "TRAE_API_ADDR" && key != "TRAE_API_TOKEN" {
			return c, fmt.Errorf("%s is unsupported; only TRAE_API_ADDR and TRAE_API_TOKEN are supported", key)
		}
	}
	if addr := os.Getenv("TRAE_API_ADDR"); addr != "" {
		c.Addr = addr
	}
	c.APIToken = os.Getenv("TRAE_API_TOKEN")
	host, port, err := net.SplitHostPort(c.Addr)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("TRAE_API_ADDR must be host:port with port 1..65535")
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return c, fmt.Errorf("TRAE_API_ADDR must use localhost or a literal IP")
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
		return c, fmt.Errorf("TRAE_API_ADDR cannot bind a single non-loopback interface with one browser callback; use 0.0.0.0 or [::] with TRAE_API_TOKEN for remote access")
	}
	if !isLoopbackAddr(c.Addr) && c.APIToken == "" {
		return c, fmt.Errorf("TRAE_API_TOKEN is required for non-loopback listening")
	}
	return c, nil
}
func isLoopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	return err == nil && (h == "localhost" || net.ParseIP(h).IsLoopback())
}

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
func badRequest(s string) error {
	return &apiError{
		400,
		s,
	}
}
func newServer(cfg config) (*server, error) {
	auth, err := newAuthManager(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{
		cfg:    cfg,
		auth:   auth,
		slots:  make(chan struct{}, cfg.MaxConcurrent),
		ctx:    ctx,
		cancel: cancel,
	}
	s.upstream = &traeClient{
		cfg:  cfg,
		http: newHTTPClient(cfg.RequestTimeout),
		auth: auth,
	}
	return s, nil
}

// listenAPI binds once; the browser callback must point to this very listener.
// A loopback callback cannot be served by an explicitly non-loopback bind.
func (s *server) listenAPI() (net.Listener, error) {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return nil, err
	}
	ipAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return nil, errors.New("API listener is not TCP")
	}
	ip := ipAddr.IP
	switch {
	case ip.IsLoopback():
	case ip.IsUnspecified() && ip.To4() != nil:
		ip = net.IPv4(127, 0, 0, 1)
	case ip.IsUnspecified():
		ip = net.IPv6loopback
	default:
		listener.Close()
		return nil, errors.New("single-listener browser callback requires loopback or wildcard API address; use 0.0.0.0 or [::] with TRAE_API_TOKEN for remote access")
	}
	callbackURL := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(ipAddr.Port)) + "/authorize"
	s.auth.mu.Lock()
	s.auth.callbackURL = callbackURL
	s.auth.mu.Unlock()
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
	if err == nil && !s.auth.validGeneration(g) {
		err = errLoginRequired
	}
	return r, result, err
}
func (s *server) shutdown() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.upstream.http.CloseIdleConnections()
		s.auth.client.CloseIdleConnections()
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
