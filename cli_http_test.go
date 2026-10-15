package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTRAE007ServeErrorSurvivesShutdownSignal(t *testing.T) {
	serveFailure := errors.New("synthetic Serve failure")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		serveErr := make(chan error, 1)
		serveErr <- serveFailure
		if err := waitForHTTP(ctx, serveErr, func() error { return nil }); !errors.Is(err, serveFailure) {
			t.Fatalf("iteration %d: Serve failure lost: %v", i, err)
		}
	}
}

func TestTRAE007ServeAndShutdownFailuresBothReported(t *testing.T) {
	serveFailure := errors.New("synthetic Serve failure")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	serveErr := make(chan error, 1)
	err := waitForHTTP(ctx, serveErr, func() error {
		serveErr <- serveFailure
		return context.DeadlineExceeded
	})
	if !errors.Is(err, serveFailure) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost Serve or shutdown failure: %v", err)
	}
}

func TestTRAE007WaitForHTTPShutdownOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		shutdownErr error
		wantErr     error
	}{
		{
			name: "clean shutdown",
		},
		{
			name:        "failed shutdown",
			shutdownErr: context.DeadlineExceeded,
			wantErr:     context.DeadlineExceeded,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancel()
			serveErr := make(chan error, 1)
			if err := waitForHTTP(ctx, serveErr, func() error {
				serveErr <- http.ErrServerClosed
				return tt.shutdownErr
			}); !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("waitForHTTP error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestTRAE007SingleListenerCallbackAndTokenBoundary(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.APIToken = "synthetic-gateway"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	service := s.httpServer()
	done := make(chan error, 1)
	go func() { done <- service.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = service.Shutdown(ctx)
		<-done
	}()
	base := "http://" + listener.Addr().String()
	client := newHTTPClient(2 * time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	call := func(method, path string, authorized bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer synthetic-gateway")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, path := range []string{"/healthz", "/api/auth/login", "/authorize/extra"} {
		resp := call(http.MethodGet, path, false)
		if resp.StatusCode != 401 {
			t.Errorf("unprotected %s: HTTP %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := call(http.MethodPost, "/api/auth/login", true)
	if resp.StatusCode != 200 {
		t.Fatalf("login start HTTP %d", resp.StatusCode)
	}
	var attempt map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&attempt); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	link, _ := url.Parse(attempt["url"])
	callback := link.Query().Get("auth_callback_url")
	if callback != base+"/authorize" {
		t.Fatalf("callback %q does not match listener %q", callback, base)
	}
	q := url.Values{
		"loginTraceID": {attempt["attempt_id"]},
		"userJwt":      {`{"Token":"synthetic","RefreshToken":"synthetic-refresh"}`},
		"userInfo":     {`{"UserID":"fixture"}`},
	}
	req, _ := http.NewRequest(http.MethodGet, callback+"?"+q.Encode(), nil)
	req.Header.Set("Origin", "https://www.trae.cn")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("cross-site official callback HTTP %d", resp.StatusCode)
	}
	resp.Body.Close()
	cred, _ := s.auth.snapshot()
	if cred.Token != "synthetic" {
		t.Fatal("callback did not install synthetic credential")
	}
	resp = call(http.MethodPost, "/authorize", false)
	if resp.StatusCode != 401 {
		t.Fatalf("POST callback bypass: HTTP %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func startSingleTestServer(t *testing.T, s *server) {
	t.Helper()
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	service := &http.Server{
		Handler: s.routes(),
	}
	go func() { _ = service.Serve(listener) }()
	t.Cleanup(func() { _ = service.Close() })
}

func TestTRAE007CallbackIsOnlyExactLocalGET(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.APIToken = "synthetic-gateway"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	callback := s.auth.callbackURL
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		host       string
		remoteAddr string
		want       int
	}{
		{
			name:       "remote peer even with forwarded loopback",
			method:     "GET",
			path:       callback,
			remoteAddr: "192.0.2.1:12345",
			want:       403,
		},
		{
			name:       "wrong Host",
			method:     "GET",
			path:       callback,
			host:       "attacker.invalid:8765",
			remoteAddr: "127.0.0.1:12345",
			want:       403,
		},
		{
			name:       "wrong local port",
			method:     "GET",
			path:       callback,
			host:       "127.0.0.1:8765",
			remoteAddr: "127.0.0.1:12345",
			want:       403,
		},
		{
			name:       "prefix",
			method:     "GET",
			path:       callback + "/extra",
			remoteAddr: "127.0.0.1:12345",
			want:       401,
		},
		{
			name:       "encoded path",
			method:     "GET",
			path:       strings.Replace(callback, "/authorize", "/%61uthorize", 1),
			remoteAddr: "127.0.0.1:12345",
			want:       401,
		},
		{
			name:       "wrong method",
			method:     "POST",
			path:       callback,
			remoteAddr: "127.0.0.1:12345",
			want:       401,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.host != "" {
				req.Host = tc.host
			}
			req.Header.Set("X-Forwarded-For", "127.0.0.1")
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestTRAE007CallbackAddressMatchesSingleListener(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "0.0.0.0:0", "[::]:0"} {
		t.Run(addr, func(t *testing.T) {
			cfg := fixtureConfig()
			cfg.Addr = addr
			cfg.APIToken = "synthetic-gateway"
			s, err := newServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.shutdown()
			listener, err := s.listenAPI()
			if err != nil {
				if strings.Contains(err.Error(), "address family not supported") || strings.Contains(err.Error(), "can't assign requested address") {
					t.Skip(err)
				}
				t.Fatal(err)
			}
			defer listener.Close()
			u, err := url.Parse(s.auth.callbackURL)
			if err != nil || u.Path != "/authorize" || !net.ParseIP(u.Hostname()).IsLoopback() {
				t.Fatalf("non-loopback callback: %q: %v", s.auth.callbackURL, err)
			}
			_, callbackPort, err := net.SplitHostPort(u.Host)
			if err != nil {
				t.Fatal(err)
			}
			_, listenPort, _ := net.SplitHostPort(listener.Addr().String())
			if callbackPort != listenPort {
				t.Fatalf("callback port %s != listener port %s", callbackPort, listenPort)
			}
			conn, err := net.DialTimeout("tcp", u.Host, time.Second)
			if err != nil {
				t.Fatalf("callback address unreachable on listener: %v", err)
			}
			conn.Close()
			service := s.httpServer()
			go func() { _ = service.Serve(listener) }()
			defer service.Close()
			link, err := s.auth.beginLogin()
			if err != nil {
				t.Fatal(err)
			}
			official, _ := url.Parse(link)
			q := url.Values{
				"loginTraceID": {official.Query().Get("login_trace_id")},
				"userJwt":      {`{"Token":"synthetic"}`},
				"userInfo":     {`{"UserID":"fixture"}`},
			}
			req, _ := http.NewRequest("GET", s.auth.callbackURL+"?"+q.Encode(), nil)
			req.Header.Set("Origin", "https://www.trae.cn")
			client := newHTTPClient(2 * time.Second)
			client.Transport.(*http.Transport).Proxy = nil
			defer client.CloseIdleConnections()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			cred, _ := s.auth.snapshot()
			if resp.StatusCode != 200 || cred.Token != "synthetic" {
				t.Fatalf("callback on %s: HTTP %d installed=%t", addr, resp.StatusCode, cred.Token != "")
			}
		})
	}
}

func TestTRAE007OldCallbackPortMayBeOccupied(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:8765")
	if err != nil {
		t.Skip("old callback port already occupied")
	}
	defer occupied.Close()
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatalf("service still needs fixed port: %v", err)
	}
	defer listener.Close()
	if strings.Contains(s.auth.callbackURL, ":8765/") {
		t.Fatal("callback used occupied old port")
	}
}

func TestTRAE007Port8765IsOrdinaryAPIListener(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:8765"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			t.Skip("port 8765 is occupied")
		}
		t.Fatal(err)
	}
	defer listener.Close()
	if s.auth.callbackURL != "http://127.0.0.1:8765/authorize" {
		t.Fatalf("callback URL %q", s.auth.callbackURL)
	}
	service := &http.Server{
		Handler: s.routes(),
	}
	go func() { _ = service.Serve(listener) }()
	defer service.Close()
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://127.0.0.1:8765/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz on 8765: HTTP %d", resp.StatusCode)
	}
}

func TestTRAE007OccupiedAPIPortDoesNotPrepareCallback(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg := fixtureConfig()
	cfg.Addr = occupied.Addr().String()
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err == nil {
		listener.Close()
		t.Fatal("started on occupied API port")
	}
	if s.auth.callbackURL != "" {
		t.Fatalf("partial callback initialized: %q", s.auth.callbackURL)
	}
}

func TestTRAE007AddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		addr    string
		token   string
		wantErr string
	}{
		{
			name: "legacy port is ordinary API port",
			addr: "127.0.0.1:8765",
		},
		{
			name: "IPv4 loopback",
			addr: "127.0.0.1:8723",
		},
		{
			name: "IPv6 loopback",
			addr: "[::1]:8723",
		},
		{
			name:  "wildcard IPv4 with token",
			addr:  "0.0.0.0:8723",
			token: "synthetic-token",
		},
		{
			name:  "wildcard IPv6 with token",
			addr:  "[::]:8723",
			token: "synthetic-token",
		},
		{
			name:    "wildcard requires token",
			addr:    "0.0.0.0:8723",
			wantErr: "TRAE_API_TOKEN is required",
		},
		{
			name:    "single nonloopback IP rejected",
			addr:    "192.0.2.1:8723",
			token:   "synthetic-token",
			wantErr: "use 0.0.0.0 or [::] with TRAE_API_TOKEN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRAE_API_ADDR", tc.addr)
			t.Setenv("TRAE_API_TOKEN", tc.token)
			cfg, err := loadConfig()
			if tc.wantErr == "" {
				if err != nil || cfg.Addr != tc.addr {
					t.Fatalf("loadConfig(%q): cfg=%q err=%v", tc.addr, cfg.Addr, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("loadConfig(%q): want %q, got %v", tc.addr, tc.wantErr, err)
			}
		})
	}
}

func TestTRAE007StandardFlagMatrix(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stop   bool
		stdout string
		stderr string
	}{
		{
			name:   "root help short",
			args:   []string{"-h"},
			code:   0,
			stop:   true,
			stdout: "Usage:",
			stderr: "",
		},
		{
			name:   "root help long",
			args:   []string{"--help"},
			code:   0,
			stop:   true,
			stdout: "-version",
			stderr: "",
		},
		{
			name:   "root version",
			args:   []string{"-version"},
			code:   0,
			stop:   true,
			stdout: "trae-api " + version,
			stderr: "",
		},
		{
			name:   "root version bool",
			args:   []string{"--version=true"},
			code:   0,
			stop:   true,
			stdout: "trae-api " + version,
			stderr: "",
		},
		{
			name:   "root version false",
			args:   []string{"-version=false"},
			code:   0,
			stop:   false,
			stdout: "",
			stderr: "",
		},
		{
			name:   "login help short",
			args:   []string{"login", "-h"},
			code:   0,
			stop:   true,
			stdout: "Usage: trae-api login",
			stderr: "",
		},
		{
			name:   "login help",
			args:   []string{"login", "-help"},
			code:   0,
			stop:   true,
			stdout: "Usage: trae-api login",
			stderr: "",
		},
		{
			name:   "login help long",
			args:   []string{"login", "--help"},
			code:   0,
			stop:   true,
			stdout: "Usage: trae-api login",
			stderr: "",
		},
		{
			name:   "unknown root flag",
			args:   []string{"-unknown"},
			code:   2,
			stop:   true,
			stdout: "",
			stderr: "flag provided but not defined",
		},
		{
			name:   "unknown login flag",
			args:   []string{"login", "-unknown"},
			code:   2,
			stop:   true,
			stdout: "",
			stderr: "flag provided but not defined",
		},
		{
			name:   "unknown command",
			args:   []string{"typo"},
			code:   2,
			stop:   true,
			stdout: "",
			stderr: "unknown command",
		},
		{
			name:   "extra login arg",
			args:   []string{"login", "extra"},
			code:   2,
			stop:   true,
			stdout: "",
			stderr: "login accepts no additional arguments",
		},
		{
			name:   "serve",
			args:   nil,
			code:   0,
			stop:   false,
			stdout: "",
			stderr: "",
		},
		{
			name:   "login",
			args:   []string{"login"},
			code:   0,
			stop:   false,
			stdout: "",
			stderr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			action, code := parseCommand(tt.args, out, errOut)
			stop := action == commandExit
			if code != tt.code || stop != tt.stop || !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errOut.String(), tt.stderr) {
				t.Fatalf("code=%d stop=%v stdout=%q stderr=%q", code, stop, out.String(), errOut.String())
			}
			if tt.stderr == "" && errOut.Len() != 0 {
				t.Fatalf("unexpected stderr %q", errOut.String())
			}
			if tt.stdout == "" && out.Len() != 0 {
				t.Fatalf("unexpected stdout %q", out.String())
			}
		})
	}
}

func TestTRAE007ShutdownCancelsInflightCallback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	upstreamDone := make(chan struct{})
	authUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		close(upstreamDone)
	}))
	defer authUpstream.Close()
	defer close(release)
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.AuthBaseURL = authUpstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	s.auth.client.Transport.(*http.Transport).Proxy = nil
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	service := s.httpServer()
	serveErr := make(chan error, 1)
	go func() { serveErr <- service.Serve(listener) }()
	defer service.Close()
	link, err := s.auth.beginLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := url.Values{
		"loginTraceID": {u.Query().Get("login_trace_id")},
		"refreshToken": {"synthetic-refresh"},
		"userInfo":     {`{"UserID":"fixture"}`},
	}
	client := newHTTPClient(3 * time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	requestDone := make(chan struct{})
	go func() {
		resp, _ := client.Get(s.auth.callbackURL + "?" + q.Encode())
		if resp != nil {
			resp.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("callback exchange did not start")
	}
	if err := s.shutdownHTTP(service, time.Second); err != nil {
		t.Fatalf("callback did not drain: %v", err)
	}
	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("callback request did not complete")
	}
	if err := <-serveErr; err != http.ErrServerClosed {
		t.Fatalf("Serve returned %v", err)
	}
	cred, _ := s.auth.snapshot()
	if cred.Token != "" {
		t.Fatal("shutdown installed callback credential")
	}
}

func TestTRAE007ShutdownDeadlineIsFailure(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	service := s.httpServer()
	service.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // Simulate a handler that ignores cancellation.
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- service.Serve(listener) }()
	defer service.Close()
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	requestDone := make(chan struct{})
	go func() {
		resp, _ := client.Get("http://" + listener.Addr().String() + "/healthz")
		if resp != nil {
			resp.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	if err := s.shutdownHTTP(service, 10*time.Millisecond); err != context.DeadlineExceeded {
		t.Fatalf("shutdown should fail on timeout, got %v", err)
	}
	close(release)
	<-requestDone
	if err := <-serveErr; err != http.ErrServerClosed {
		t.Fatalf("Serve returned %v", err)
	}
}

func TestTRAE007VersionRejectsOperands(t *testing.T) {
	for _, args := range [][]string{
		{"-version", "extra"},
		{"-version", "login"},
		{"--version=true", "extra"},
	} {
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		action, code := parseCommand(args, stdout, stderr)
		if action != commandExit || code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown command") {
			t.Errorf("parseCommand(%q): action=%v code=%d stdout=%q stderr=%q", args, action, code, stdout, stderr)
		}
	}
}

func TestTRAE007CanceledCallbackCannotCommit(t *testing.T) {
	cfg := fixtureConfig()
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8723/authorize"
	link, err := s.auth.beginLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := url.Values{
		"loginTraceID": {u.Query().Get("login_trace_id")},
		"userJwt":      {`{"Token":"synthetic","RefreshToken":"synthetic-refresh"}`},
		"userInfo":     {`{"UserID":"fixture"}`},
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil).WithContext(ctx)
	cancel()
	s.auth.callback(httptest.NewRecorder(), req)
	cred, _ := s.auth.snapshot()
	if cred.Token != "" {
		t.Fatal("canceled callback committed credential")
	}
}
