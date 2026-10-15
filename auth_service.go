package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// authService owns the writer lock, browser flow, refresh, and HTTP listener.
// addr is separate from config until the coordinator supplies its Auth address.
type authService struct {
	addr        string
	manager     *authManager
	store       *credentialStore
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	listener    net.Listener
	http        *http.Server
	once        sync.Once
	shutdownErr error
}

func newAuthService(c config, addr string) (*authService, error) {
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return nil, fmt.Errorf("Auth address must be a loopback host:port")
	}
	store, err := openCredentialStore()
	if err != nil {
		return nil, err
	}
	manager, err := newAuthManager(c)
	if err != nil {
		store.Close()
		return nil, err
	}
	saved, err := store.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		store.Close()
		return nil, err
	}
	if err == nil {
		manager.cred = saved
		if saved.RefreshPending {
			manager.refreshErr = errAmbiguousExchange
		}
	}
	manager.store = store
	serviceCtx, cancel := context.WithCancel(context.Background())
	return &authService{
		addr:    addr,
		manager: manager,
		store:   store,
		ctx:     serviceCtx,
		cancel:  cancel,
	}, nil
}

func (a *authService) listen() (net.Listener, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listener != nil {
		return nil, errors.New("Auth listener already active")
	}
	l, err := net.Listen("tcp", a.addr)
	if err != nil {
		return nil, err
	}
	tcp, ok := l.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		l.Close()
		return nil, errors.New("Auth listener must bind loopback TCP")
	}
	callback := "http://" + net.JoinHostPort(tcp.IP.String(), strconv.Itoa(tcp.Port)) + "/authorize"
	a.manager.mu.Lock()
	a.manager.callbackURL = callback
	a.manager.mu.Unlock()
	a.listener = l
	return l, nil
}

func (a *authService) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		c, _ := a.manager.snapshot()
		writeJSON(w, 200, map[string]any{"status": "ok", "logged_in": c.Token != "" && time.Now().Before(c.ExpiresAt)})
	})
	mux.HandleFunc("POST /api/auth/login", a.startLogin)
	mux.HandleFunc("POST /api/auth/login/status", a.loginStatus)
	mux.HandleFunc("POST /api/auth/login/cancel", a.cancelLogin)
	mux.HandleFunc("GET /authorize", a.manager.callback)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.manager.mu.Lock()
		callback := a.manager.callbackURL
		a.manager.mu.Unlock()
		u, _ := url.Parse(callback)
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(peer)
		if u == nil || u.Host == "" || r.Host != u.Host || !localManagementHost(r.Host) || err != nil || ip == nil || !ip.IsLoopback() || r.URL.EscapedPath() == "/authorize" && r.Method != http.MethodGet {
			writeError(w, 403, "non-local Auth request denied")
			return
		}
		if r.URL.EscapedPath() != "/authorize" && !sameOrigin(r) {
			writeError(w, 403, "cross-origin management request denied")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *authService) startLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.manager.mu.Lock()
	c := a.manager.cred
	logged := c.Token != "" && a.manager.refreshErr == nil && !a.manager.durabilityUncertain && time.Now().Before(c.ExpiresAt)
	a.manager.mu.Unlock()
	if logged {
		writeJSON(w, 200, map[string]string{"status": "already_logged_in"})
		return
	}
	link, err := a.manager.beginLogin()
	if err != nil {
		writeError(w, 503, "authorization callback listener is unavailable")
		return
	}
	u, _ := url.Parse(link)
	writeJSON(w, 200, map[string]string{"status": "pending", "url": link, "attempt_id": u.Query().Get("login_trace_id")})
}
func (a *authService) attemptRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, _, err := decodeBody(w, r)
	if err != nil {
		writeError(w, 400, "invalid login attempt request")
		return "", false
	}
	id := firstString(objectValue(string(body)), "attempt_id")
	if id == "" {
		writeError(w, 400, "missing attempt_id")
		return "", false
	}
	return id, true
}
func (a *authService) loginStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, ok := a.attemptRequest(w, r)
	if !ok {
		return
	}
	a.manager.mu.Lock()
	result, found := a.manager.loginResults[id]
	if found && result.Status == "pending" && !time.Now().Before(result.Expires) {
		result.Status = "expired"
		a.manager.loginResults[id] = result
		delete(a.manager.pending, id)
		if a.manager.latestLogin == id {
			a.manager.latestLogin = ""
		}
	}
	a.manager.mu.Unlock()
	if !found {
		writeError(w, 404, "login attempt not found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": result.Status})
}
func (a *authService) cancelLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, ok := a.attemptRequest(w, r)
	if !ok {
		return
	}
	a.manager.mu.Lock()
	result, found := a.manager.loginResults[id]
	if found && result.Status == "pending" {
		result.Status = "cancelled"
		a.manager.loginResults[id] = result
		delete(a.manager.pending, id)
		if a.manager.latestLogin == id {
			a.manager.latestLogin = ""
		}
	}
	a.manager.mu.Unlock()
	if !found {
		writeError(w, 404, "login attempt not found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": result.Status})
}
func (a *authService) httpServer() *http.Server {
	return &http.Server{
		Handler:           a.routes(),
		BaseContext:       func(net.Listener) context.Context { return a.ctx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    128 << 10,
	}
}

// Run blocks until context cancellation or listener failure, then releases the writer lock.
func (a *authService) Run(ctx context.Context) (runErr error) {
	defer func() { runErr = errors.Join(runErr, a.shutdown()) }()
	l, err := a.listen()
	if err != nil {
		return err
	}
	srv := a.httpServer()
	a.mu.Lock()
	a.http = srv
	a.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	refreshCtx, cancelRefresh := context.WithCancel(ctx)
	refreshDone := make(chan struct{})
	go func() { defer close(refreshDone); a.refreshLoop(refreshCtx) }()
	defer func() {
		cancelRefresh()
		<-refreshDone
	}()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return errors.New("Auth listener stopped unexpectedly")
		}
		return err
	case <-ctx.Done():
		a.cancel()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		shutdownErr := srv.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, srv.Close())
		}
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	}
}
func (a *authService) shutdown() error {
	a.once.Do(func() {
		a.cancel()
		a.mu.Lock()
		l, srv := a.listener, a.http
		a.mu.Unlock()
		if srv != nil {
			a.shutdownErr = errors.Join(a.shutdownErr, srv.Close())
		} else if l != nil {
			a.shutdownErr = errors.Join(a.shutdownErr, l.Close())
		}
		a.manager.mu.Lock()
		a.manager.callbackURL = ""
		a.manager.mu.Unlock()
		if err := a.store.Close(); err != nil {
			a.shutdownErr = errors.Join(a.shutdownErr, fmt.Errorf("close Auth credential store: %w", err))
		}
	})
	return a.shutdownErr
}
func (a *authService) refreshLoop(ctx context.Context) {
	a.refreshIfNeeded(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshIfNeeded(ctx)
		}
	}
}
func (a *authService) refreshIfNeeded(ctx context.Context) {
	a.manager.mu.Lock()
	c := a.manager.cred
	if c.Token == "" || time.Until(c.ExpiresAt) > 2*time.Minute || a.manager.refreshErr != nil {
		a.manager.mu.Unlock()
		return
	}
	a.manager.mu.Unlock()
	_, _, err := a.manager.get(ctx)
	if err != nil && ctx.Err() == nil {
		a.manager.mu.Lock()
		uncertain := a.manager.durabilityUncertain
		a.manager.mu.Unlock()
		if uncertain {
			slog.Warn("Auth refresh durability unconfirmed", "error", err)
		} else {
			slog.Warn("Auth refresh failed", "error", err)
		}
	}
}
