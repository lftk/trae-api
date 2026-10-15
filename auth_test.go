package main

import (
	"bytes"
	"context"
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
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"Result":{}}`) }))
	defer fail.Close()
	a.cfg.AuthBaseURL = fail.URL
	a.cred = credential{
		Token:        "expired",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(-time.Second),
	}
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
	_, c, e := s.execute(context.Background(), chatRequest{
		Model: "m",
		Messages: []message{{
			Role:    "user",
			Content: "hi",
		}},
	}, nil)
	if e != nil || c.Message.Content != "ok" || attempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("%+v %v attempts=%d refreshes=%d", c, e, attempts.Load(), refreshes.Load())
	}
}

func loginRequest(t *testing.T, s *server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8723"+path, strings.NewReader(jsonText(body)))
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}
func TestTRAE003LoginLifecycle(t *testing.T) {
	s, err := newServer(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	start := loginRequest(t, s, "/api/auth/login", map[string]any{})
	if start.Code != 200 {
		t.Fatalf("login entry missing: %d %s", start.Code, start.Body.String())
	}
	obj := decoded(t, start)
	id, ok := obj["attempt_id"].(string)
	if !ok || id == "" {
		t.Fatal("missing attempt")
	}
	u, _ := url.Parse(obj["url"].(string))
	if u.Host != "www.trae.cn" {
		t.Fatal("not official authorization")
	}
	wait := loginRequest(t, s, "/api/auth/login/status", map[string]string{"attempt_id": id})
	if decoded(t, wait)["status"] != "pending" {
		t.Fatal(wait.Body.String())
	}
	q := url.Values{"loginTraceID": {id}, "userJwt": {`{"Token":"synthetic-callback-token"}`}, "userInfo": {`{"UserID":"fixture"}`}}
	cb := httptest.NewRecorder()
	s.auth.callback(cb, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
	if cb.Code != 200 {
		t.Fatal(cb.Body.String())
	}
	wait = loginRequest(t, s, "/api/auth/login/status", map[string]string{"attempt_id": id})
	if decoded(t, wait)["status"] != "completed" || strings.Contains(wait.Body.String(), "synthetic-callback-token") {
		t.Fatal("completion not correlated or leaked")
	}
	again := loginRequest(t, s, "/api/auth/login", map[string]any{})
	if decoded(t, again)["status"] != "already_logged_in" {
		t.Fatal("duplicate login opened flow")
	}
	restarted, _ := newServer(fixtureConfig())
	defer restarted.shutdown()
	c, _ := restarted.auth.snapshot()
	if c.Token != "" {
		t.Fatal("login survives restart")
	}
}

func TestTRAE003LoginAttemptBoundaries(t *testing.T) {
	for _, kind := range []string{"cancelled", "superseded", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newServer(fixtureConfig())
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			first := loginRequest(t, s, "/api/auth/login", map[string]any{})
			id := decoded(t, first)["attempt_id"].(string)
			switch kind {
			case "cancelled":
				w := loginRequest(t, s, "/api/auth/login/cancel", map[string]string{"attempt_id": id})
				if w.Code != 200 {
					t.Fatalf("cancel missing: %d", w.Code)
				}
			case "superseded":
				loginRequest(t, s, "/api/auth/login", map[string]any{})
			case "expired":
				s.auth.mu.Lock()
				r := s.auth.loginResults[id]
				r.Expires = time.Now().Add(-time.Second)
				s.auth.loginResults[id] = r
				s.auth.mu.Unlock()
			}
			state := loginRequest(t, s, "/api/auth/login/status", map[string]string{"attempt_id": id})
			if decoded(t, state)["status"] != kind {
				t.Fatalf("status=%s want=%s", state.Body.String(), kind)
			}
			cb := httptest.NewRecorder()
			s.auth.callback(cb, httptest.NewRequest("GET", "/authorize?loginTraceID="+id+"&userJwt=%7B%22Token%22%3A%22fixture%22%7D&userInfo=%7B%22UserID%22%3A%22fixture%22%7D", nil))
			if cb.Code == 200 {
				t.Fatal("inactive attempt logged in")
			}
		})
	}
}
func TestTRAE003ExpiredAccountCanLogin(t *testing.T) {
	s := authenticatedServer(t, fixtureConfig())
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	s.auth.cred.ExpiresAt = time.Now().Add(-time.Second)
	w := loginRequest(t, s, "/api/auth/login", map[string]any{})
	if decoded(t, w)["status"] != "pending" {
		t.Fatal("expired account treated as logged in")
	}
}

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

func TestTRAE003LoginRejectsRemoteAddress(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "203.0.113.1:8723"
	cfg.APIToken = "synthetic-gateway-token"
	err := runLogin(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error {
		t.Fatal("remote service opened browser")
		return nil
	}, time.Millisecond, 15*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "this computer") {
		t.Fatalf("remote target not rejected before network: %v", err)
	}
}
