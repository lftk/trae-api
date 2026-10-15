package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestQA007CallbackExceptionIsLimitedToExactRoute verifies the production
// router, not auth.callback in isolation. The browser does not carry the API
// bearer token, while every other API/management path must remain protected.
func TestQA007CallbackExceptionIsLimitedToExactRoute(t *testing.T) {
	cfg := fixtureConfig()
	cfg.APIToken = "synthetic-gateway"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8723/authorize"
	routes := s.routes()

	call := func(method, path, host string, origin string, token bool) int {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1:8723"+path, nil)
		r.RemoteAddr = "127.0.0.1:34567"
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
		}
		if token {
			r.Header.Set("Authorization", "Bearer synthetic-gateway")
		}
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, r)
		return w.Code
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{
			method: "GET",
			path:   "/healthz",
		},
		{
			method: "GET",
			path:   "/v1/models",
		},
		{
			method: "POST",
			path:   "/api/auth/login",
		},
		{
			method: "GET",
			path:   "/authorize/extra",
		},
		{
			method: "POST",
			path:   "/authorize",
		},
	} {
		if got := call(tc.method, tc.path, "127.0.0.1:8723", "", false); got != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s %s: status=%d, want 401", tc.method, tc.path, got)
		}
	}
	link, err := s.auth.beginLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{
		"loginTraceID": {u.Query().Get("login_trace_id")},
		"userJwt":      {`{"Token":"synthetic-account"}`},
		"userInfo":     {`{"UserID":"synthetic-user"}`},
	}
	path := "/authorize?" + query.Encode()
	origin := u.Scheme + "://" + u.Host
	if got := call("GET", path, "attacker.invalid:8723", origin, false); got != http.StatusForbidden {
		t.Errorf("spoofed callback Host: status=%d, want 403", got)
	}
	if c, _ := s.auth.snapshot(); c.Token != "" {
		t.Fatal("spoofed callback installed credential")
	}
	if got := call("GET", path, "127.0.0.1:8723", origin, false); got != http.StatusOK {
		t.Fatalf("official browser callback without bearer: status=%d, want 200", got)
	}
	if c, _ := s.auth.snapshot(); c.Token != "synthetic-account" {
		t.Fatal("official callback did not install synthetic credential")
	}
	if got := call("GET", path, "127.0.0.1:8723", origin, false); got == http.StatusOK {
		t.Fatal("one-time callback was accepted twice")
	}
}

// TestQA007SharedServerDoesNotCutLongSSE uses the production shared Server
// configuration, rather than httptest's default server with unrelated timeouts.
func TestQA007SharedServerDoesNotCutLongSSE(t *testing.T) {
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
	service := s.httpServer()
	service.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink := newStreamSink(w, r)
		defer sink.close()
		if err := sink.send("", map[string]string{"text": "start"}); err != nil {
			return
		}
		timer := time.NewTimer(32 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		_ = sink.done()
	})
	done := make(chan error, 1)
	go func() { done <- service.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("shutdown shared Server: %v", err)
		}
		<-done
	}()
	client := &http.Client{
		Timeout: 40 * time.Second,
	}
	resp, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(content), "data: [DONE]") {
		t.Fatalf("shared Server cut idle SSE: read_error=%v terminal_frame=%t", err, strings.Contains(string(content), "data: [DONE]"))
	}
}

// TestQA007ChatRouteLongSSE exercises the real shared HTTP route, with a
// synthetic local IDE upstream that pauses beyond the old 30s write deadline.
func TestQA007ChatRouteLongSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, frame("output", map[string]any{"response": "first"}))
		w.(http.Flusher).Flush()
		timer := time.NewTimer(32 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		fmt.Fprint(w, frame("done", map[string]string{"finish_reason": "stop"}))
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.BaseURL = upstream.URL
	cfg.RequestTimeout = 45 * time.Second
	s := authenticatedServer(t, cfg)
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	service := s.httpServer()
	done := make(chan error, 1)
	go func() { done <- service.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("shutdown chat Server: %v", err)
		}
		<-done
	}()
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"synthetic"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: 45 * time.Second,
	}
	started := time.Now()
	resp, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	text := string(content)
	if err != nil || resp.StatusCode != http.StatusOK || time.Since(started) < 31*time.Second || !strings.Contains(text, `"content":"first"`) || !strings.Contains(text, `"finish_reason":"stop"`) || !strings.Contains(text, "data: [DONE]") || strings.Contains(text, `"error":`) {
		t.Fatalf("chat route cut or failed long SSE: status=%d read_error=%v elapsed=%v success_content=%t stop=%t done=%t error_chunk=%t", resp.StatusCode, err, time.Since(started), strings.Contains(text, `"content":"first"`), strings.Contains(text, `"finish_reason":"stop"`), strings.Contains(text, "data: [DONE]"), strings.Contains(text, `"error":`))
	}
}
