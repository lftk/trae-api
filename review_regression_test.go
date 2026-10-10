package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTRAE003OldRejectionCannotInvalidateNewToken(t *testing.T) {
	var a *authManager
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cloudide/api/v3/trae/oauth/ExchangeToken" {
			writeJSON(w, 200, map[string]any{"Result": map[string]any{"Token": "retry-token", "TokenExpireAt": time.Now().Add(time.Hour).Unix()}})
			return
		}
		if r.Header.Get("Authorization") == "Cloud-IDE-JWT retry-token" {
			a.mu.Lock()
			a.cred.Token = "newer-token"
			a.mu.Unlock()
		}
		w.WriteHeader(401)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	cfg.AuthBaseURL = up.URL
	s := authenticatedServer(t, cfg)
	a = s.auth
	a.cred.RefreshToken = "synthetic-refresh"
	_, _, err := s.execute(context.Background(), chatRequest{Model: "m", Messages: []message{{Role: "user", Content: "hi"}}}, nil)
	if err != errLoginRequired {
		t.Fatalf("expected rejected request: %v", err)
	}
	c, _, err := a.get(context.Background())
	if err != nil || c.Token != "newer-token" {
		t.Fatalf("old rejection poisoned newer token: %v", err)
	}
}
func TestTRAE003AllRoutesRejectRebinding(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/models", "/healthz", "/v1/responses"} {
		r := httptest.NewRequest("GET", "http://attacker.invalid"+path, nil)
		w := httptest.NewRecorder()
		authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("%s allowed rebinding Host: %d", path, w.Code)
		}
	}
}
