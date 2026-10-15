package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	requests := 0
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprint(w, `{"Result":{}}`)
	}))
	defer fail.Close()
	a.cfg.AuthBaseURL = fail.URL
	a.cred = credential{
		Token:        "expired",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	if _, _, e := a.get(context.Background()); e == nil || errors.Is(e, errLoginRequired) {
		t.Fatalf("incomplete HTTP 200 is not definitive rejection: %v", e)
	}
	if _, _, e := a.get(context.Background()); !errors.Is(e, errLoginRequired) || requests != 1 {
		t.Fatalf("ambiguous refresh was replayed: err=%v requests=%d", e, requests)
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
	a.cred = credential{
		Token:        "old",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	done := make(chan error, 1)
	go func() {
		_, _, e := a.get(context.Background())
		done <- e
	}()
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
func loginRequest(t *testing.T, s *server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8723"+path, strings.NewReader(jsonText(body)))
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}
func TestTRAE001TransientRefresh(t *testing.T) {
	var attempts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `{"Result":{"Token":"fresh","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{
		Token:        "still-valid",
		RefreshToken: "fixture-refresh",
		UserID:       "account",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	for i := 0; i < 2; i++ {
		c, _, err := a.get(context.Background())
		if err != nil || c.Token != "still-valid" {
			t.Fatalf("valid token lost on transient failure: %q %v", c.Token, err)
		}
	}
	a.mu.Lock()
	a.refreshedAt = time.Now().Add(-2 * time.Minute)
	a.mu.Unlock()
	c, _, err := a.get(context.Background())
	if err != nil || c.Token != "fresh" {
		t.Fatalf("retry failed: %q %v", c.Token, err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("exchanges=%d", attempts.Load())
	}
}

func TestTRAE001LegacyExpiry(t *testing.T) {
	expiry := time.Unix(1900000000, 0)
	t.Run("callback query refreshExpireAt", func(t *testing.T) {
		a, err := newAuthManager(fixtureConfig())
		if err != nil {
			t.Fatal(err)
		}
		a.callbackURL = "http://127.0.0.1:8765/authorize"
		login, err := a.beginLogin()
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(login)
		q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"fixture","tokenExpireAt":1900000000}`}, "userInfo": {`{"UserID":"account"}`}, "refreshExpireAt": {"1900000000"}}
		w := httptest.NewRecorder()
		a.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
		c, _ := a.snapshot()
		if w.Code != 200 || !c.ExpiresAt.Equal(expiry) || !c.RefreshExpiresAt.Equal(expiry) {
			t.Fatalf("status=%d expiry=%v refresh=%v", w.Code, c.ExpiresAt, c.RefreshExpiresAt)
		}
	})
	t.Run("exchange lower-case expiry", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"Result":{"Token":"fresh","tokenExpireAt":1900000000,"refreshExpireAt":1900000000}}`)
		}))
		defer up.Close()
		cfg := fixtureConfig()
		cfg.AuthBaseURL = up.URL
		a, err := newAuthManager(cfg)
		if err != nil {
			t.Fatal(err)
		}
		c, err := a.exchange(context.Background(), credential{
			RefreshToken: "fixture",
		})
		if err != nil || !c.ExpiresAt.Equal(expiry) || !c.RefreshExpiresAt.Equal(expiry) {
			t.Fatalf("expiry=%v refresh=%v err=%v", c.ExpiresAt, c.RefreshExpiresAt, err)
		}
	})
	t.Run("callback lower-case refresh expiry", func(t *testing.T) {
		a, _ := newAuthManager(fixtureConfig())
		a.callbackURL = "http://127.0.0.1:8765/authorize"
		login, _ := a.beginLogin()
		u, _ := url.Parse(login)
		q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"fixture","refreshExpireAt":1900000000}`}, "userInfo": {`{"UserID":"account"}`}}
		w := httptest.NewRecorder()
		a.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
		c, _ := a.snapshot()
		if w.Code != 200 || !c.RefreshExpiresAt.Equal(expiry) {
			t.Fatalf("status=%d refresh=%v", w.Code, c.RefreshExpiresAt)
		}
	})
}
