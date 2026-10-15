package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type qa004WaitingContext struct {
	context.Context
	entered chan<- struct{}
}

func (c qa004WaitingContext) Done() <-chan struct{} {
	c.entered <- struct{}{}
	return c.Context.Done()
}

// All credentials and upstream responses here are synthetic; no login or model call is made.
func TestQA004ConcurrentRefreshSharesOneExchange(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var exchanges atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceExchangePath {
			t.Errorf("bound refresh used legacy path: %s", r.URL.Path)
		}
		if exchanges.Add(1) == 1 {
			close(started)
		}
		<-release
		writeJSON(w, 200, map[string]any{"Result": map[string]any{
			"Token": "fresh-A", "UserID": "user-A", "TokenExpireAt": time.Now().Add(time.Hour).Unix(),
		}})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.client.CloseIdleConnections()
	key, err := newDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{
		Token:        "expired-A",
		UserID:       "user-A",
		RefreshToken: "refresh-A",
		DeviceID:     "device-A",
		MachineID:    "machine-A",
		DeviceKey:    key,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	const callers = 12
	var wg sync.WaitGroup
	wg.Add(1)
	results := make(chan error, callers)
	check := func(ctx context.Context) {
		defer wg.Done()
		c, _, err := a.get(ctx)
		if err == nil && (c.Token != "fresh-A" || c.UserID != "user-A" || c.DeviceID != "device-A" || c.MachineID != "machine-A" || c.DeviceKey != key) {
			err = fmt.Errorf("refresh changed token, account or device identity")
		}
		results <- err
	}
	go check(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("refresh never started")
	}
	// Done() is reached by each follower only in get's refreshing wait branch.
	// Do not release the exchange until all followers actually reached it.
	entered := make(chan struct{}, callers-1)
	wg.Add(callers - 1)
	for i := 1; i < callers; i++ {
		go check(qa004WaitingContext{
			Context: context.Background(),
			entered: entered,
		})
	}
	for i := 1; i < callers; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("follower did not enter shared-refresh wait")
		}
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("concurrent refresh made %d exchanges; want 1", got)
	}
}

func TestQA004RefreshTokenRotationThenOmissionRetainsLatest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"RefreshToken"`
			DeviceInfo   struct {
				DeviceID  string `json:"DeviceID"`
				MachineID string `json:"MachineID"`
			} `json:"DeviceInfo"`
		}
		if r.URL.Path != deviceExchangePath || r.Header.Get("Authorization") == "" {
			t.Errorf("exchange lost binding: path=%s auth_present=%t", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.DeviceInfo.DeviceID != "device-A" || body.DeviceInfo.MachineID != "machine-A" {
			t.Errorf("exchange changed device binding: %+v", body.DeviceInfo)
		}
		mu.Lock()
		seen = append(seen, body.RefreshToken)
		attempt := len(seen)
		mu.Unlock()
		result := map[string]any{
			"Token":         fmt.Sprintf("fresh-%d", attempt),
			"UserID":        "user-A",
			"TokenExpireAt": time.Now().Add(time.Hour).Unix(),
		}
		if attempt == 1 {
			result["RefreshToken"] = "refresh-rotated"
		}
		writeJSON(w, 200, map[string]any{"Result": result})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.client.CloseIdleConnections()
	key, err := newDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{
		Token:        "expired",
		UserID:       "user-A",
		RefreshToken: "refresh-original",
		DeviceID:     "device-A",
		MachineID:    "machine-A",
		DeviceKey:    key,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	for attempt := 1; attempt <= 2; attempt++ {
		c, _, err := a.get(context.Background())
		if err != nil || c.RefreshToken != "refresh-rotated" || c.UserID != "user-A" || c.DeviceID != "device-A" || c.MachineID != "machine-A" || c.DeviceKey != key {
			t.Fatalf("exchange %d: token retention/device binding failed: %v (refresh=%q device=%q)", attempt, err, c.RefreshToken, c.DeviceID)
		}
		if attempt == 1 {
			a.mu.Lock()
			a.cred.ExpiresAt = time.Now().Add(-time.Second)
			a.mu.Unlock()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "refresh-original" || seen[1] != "refresh-rotated" {
		t.Fatalf("refresh rotation was not used on next exchange: %v", seen)
	}
}

func TestQA004StringAuthCodeRetriesOnceButBusinessCodeDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        string
		wantLists   int
		wantRefresh int
		wantDetails int
	}{
		{
			"rejected_after_refresh",
			"1001",
			2,
			1,
			0,
		},
		{
			"business_error_discovery_fallback",
			"1002",
			1,
			0,
			4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lists, refreshes, details int
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/ide/v1/batch_get_detail_param":
					lists++
					fmt.Fprintf(w, `{"code":%q,"message":"synthetic rejection"}`, tc.code)
				case "/trae/api/v3/oauth/ExchangeToken", "/cloudide/api/v3/trae/oauth/ExchangeToken":
					refreshes++
					fmt.Fprint(w, `{"Result":{"Token":"fresh-A","UserID":"user-A"}}`)
				default:
					details++
					if r.URL.Path != "/api/ide/v1/get_detail_param" {
						t.Errorf("unexpected request: %s", r.URL.Path)
					}
					fmt.Fprint(w, `{"config_info_list":[{"config_name":"fallback-model"}]}`)
				}
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL, cfg.AuthBaseURL = up.URL, up.URL
			s := authenticatedServer(t, cfg)
			defer s.shutdown()
			s.auth.mu.Lock()
			s.auth.cred.RefreshToken = "synthetic-refresh-A"
			s.auth.cred.UserID = "user-A"
			s.auth.mu.Unlock()
			c, g := s.auth.snapshot()
			names, err := s.upstream.modelNames(context.Background(), c, g)
			if lists != tc.wantLists || refreshes != tc.wantRefresh || details != tc.wantDetails {
				t.Fatalf("code=%s err=%v batch=%d refresh=%d detail=%d", tc.code, err, lists, refreshes, details)
			}
			if tc.code == "1001" {
				if err == nil || !strings.Contains(err.Error(), "login required") {
					t.Fatalf("repeated auth rejection not mapped to login-required: %v", err)
				}
			} else if err != nil || strings.Join(names, ",") != "fallback-model" {
				t.Fatalf("non-auth batch error did not use discovery fallback: names=%v err=%v", names, err)
			}
		})
	}
}
