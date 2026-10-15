package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTRAE003CLIWaitAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "browser", true: "fallback"}[fallback], func(t *testing.T) {
			s, _ := newServer(fixtureConfig())
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			api := httptest.NewServer(s.routes())
			defer api.Close()
			u, _ := url.Parse(api.URL)
			cfg := fixtureConfig()
			cfg.Addr = u.Host
			out := &bytes.Buffer{}
			opened := 0
			err := runLogin(context.Background(), cfg, out, func(ctx context.Context, link string) error {
				opened++
				a, _ := url.Parse(link)
				id := a.Query().Get("login_trace_id")
				q := url.Values{"loginTraceID": {id}, "userJwt": {`{"Token":"synthetic-sensitive-fixture"}`}, "userInfo": {`{"UserID":"fixture"}`}}
				w := httptest.NewRecorder()
				s.auth.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
				if w.Code != 200 {
					t.Error("callback failed")
				}
				if fallback {
					return errors.New("synthetic-sensitive-fixture /authorize?userJwt=secret")
				}
				return nil
			}, time.Millisecond, time.Second)
			if err != nil || opened != 1 || !strings.Contains(out.String(), "Login successful") {
				t.Fatalf("err=%v opened=%d out=%s", err, opened, out)
			}
			if strings.Contains(out.String(), "synthetic-sensitive-fixture") || strings.Contains(out.String(), "userJwt") {
				t.Fatal("credentials or full callback leaked")
			}
			if fallback && !strings.Contains(out.String(), "https://www.trae.cn/authorization?") {
				t.Fatal("fallback URL missing")
			}
			out.Reset()
			err = runLogin(context.Background(), cfg, out, func(context.Context, string) error {
				t.Fatal("opened browser for logged in service")
				return nil
			}, time.Millisecond, time.Second)
			if err != nil || !strings.Contains(out.String(), "Already logged in") {
				t.Fatal("repeat failed")
			}
		})
	}
}
func TestTRAE003CLIErrorBoundaries(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout", "unauthorized", "failed"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixtureConfig()
			if kind == "unauthorized" {
				cfg.APIToken = "fixture-gateway"
			}
			s, _ := newServer(cfg)
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			api := httptest.NewServer(s.routes())
			defer api.Close()
			u, _ := url.Parse(api.URL)
			cfg.Addr = u.Host
			cfg.APIToken = ""
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var id string
			out := &bytes.Buffer{}
			err := runLogin(ctx, cfg, out, func(ctx context.Context, link string) error {
				u, _ := url.Parse(link)
				id = u.Query().Get("login_trace_id")
				if kind == "cancel" {
					cancel()
				}
				if kind == "failed" {
					s.auth.callback(httptest.NewRecorder(), httptest.NewRequest("GET", "/authorize?loginTraceID="+id, nil))
				}
				return nil
			}, time.Millisecond, 25*time.Millisecond)
			if err == nil {
				t.Fatal("expected error")
			}
			want := map[string]string{
				"cancel":       "cancelled",
				"timeout":      "timed out",
				"unauthorized": "authentication failed",
				"failed":       "authorization failed",
			}[kind]
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err=%v", err)
			}
			if id != "" && kind != "failed" {
				s.auth.mu.Lock()
				status := s.auth.loginResults[id].Status
				s.auth.mu.Unlock()
				if status != "cancelled" {
					t.Fatalf("attempt not cleaned up: %s", status)
				}
			}
		})
	}
}
func TestTRAE003ManagementRoutesProtected(t *testing.T) {
	s, _ := newServer(fixtureConfig())
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	for _, path := range []string{"/api/auth/login", "/api/auth/login/status", "/api/auth/login/cancel"} {
		r := httptest.NewRequest(http.MethodPost, "http://evil.invalid:8723"+path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "http://evil.invalid:8723")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s bypass: %d", path, w.Code)
		}
	}
}
