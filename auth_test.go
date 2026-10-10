package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginInvalidStateAndFailedRefresh(t *testing.T) {
	a, _ := newAuthManager(fixtureConfig())
	a.callbackURL = "http://127.0.0.1:8765/authorize"
	rr := httptest.NewRecorder()
	a.callback(rr, httptest.NewRequest("GET", "/authorize?loginTraceID=unknown&userJwt=%7B%22Token%22%3A%22bad%22%7D", nil))
	if rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	u, _ := a.beginLogin()
	parsed, _ := url.Parse(u)
	state := parsed.Query().Get("login_trace_id")
	a.mu.Lock()
	attempt := a.pending[state]
	attempt.expires = time.Now().Add(-time.Second)
	a.pending[state] = attempt
	a.mu.Unlock()
	rr = httptest.NewRecorder()
	a.callback(rr, httptest.NewRequest("GET", "/authorize?loginTraceID="+state, nil))
	if rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"Result":{}}`) }))
	defer fail.Close()
	a.cfg.AuthBaseURL = fail.URL
	a.cred = credential{Token: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Second)}
	if _, _, e := a.get(context.Background()); e != errLoginRequired {
		t.Fatal(e)
	}
	if _, _, e := a.get(context.Background()); e != errLoginRequired {
		t.Fatal(e)
	}
}
func TestLogoutDuringRefreshCannotRestoreCredential(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeJSON(w, 200, map[string]any{"Result": map[string]any{"Token": "fresh", "TokenExpireAt": time.Now().Add(time.Hour).Unix()}})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, _ := newAuthManager(cfg)
	a.cred = credential{Token: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Second)}
	done := make(chan error, 1)
	go func() { _, _, e := a.get(context.Background()); done <- e }()
	<-started
	if e := a.logout(); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != errLoginRequired {
		t.Fatal(e)
	}
	if c, _ := a.snapshot(); c.Token != "" {
		t.Fatal("refresh resurrected a logged-out account")
	}
}
func TestAuthenticationRejectionRefreshesOnceBeforeModelOutput(t *testing.T) {
	var attempts, refreshes atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cloudide/api/v3/trae/oauth/ExchangeToken" {
			refreshes.Add(1)
			writeJSON(w, 200, map[string]any{"Result": map[string]any{"Token": "fresh", "TokenExpireAt": time.Now().Add(time.Hour).Unix()}})
			return
		}
		attempts.Add(1)
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT fresh" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, textFrames("ok"))
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	s.auth.mu.Lock()
	s.auth.cred.RefreshToken = "refresh"
	s.auth.mu.Unlock()
	_, c, e := s.execute(context.Background(), chatRequest{Model: "m", Messages: []message{{Role: "user", Content: "hi"}}}, nil)
	if e != nil || c.Message.Content != "ok" || attempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("%+v %v attempts=%d refreshes=%d", c, e, attempts.Load(), refreshes.Load())
	}
}
