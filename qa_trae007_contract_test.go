package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestQA007CallbackExceptionIsLimitedToExactRoute verifies the production
// router, not auth.callback in isolation. The browser does not carry the API
// bearer token, while every other API/management path must remain protected.
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
