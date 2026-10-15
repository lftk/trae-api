package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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
