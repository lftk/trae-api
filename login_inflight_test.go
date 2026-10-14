package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestTRAE003ExpiredCallbackCannotCommit(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"Result":{"Token":"synthetic-fresh","UserID":"fixture"}}`)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	first := loginRequest(t, s, "/api/auth/login", map[string]any{})
	id := decoded(t, first)["attempt_id"].(string)
	q := url.Values{"loginTraceID": {id}, "refreshToken": {"fixture"}}
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.auth.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
		done <- w.Code
	}()
	<-started
	s.auth.mu.Lock()
	result := s.auth.loginResults[id]
	result.Expires = time.Now().Add(-time.Second)
	s.auth.loginResults[id] = result
	s.auth.mu.Unlock()
	close(release)
	if code := <-done; code == 200 {
		t.Fatal("expired in-flight authorization committed")
	}
	c, _ := s.auth.snapshot()
	if c.Token != "" {
		t.Fatal("expired callback logged in")
	}
}
