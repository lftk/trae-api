package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStringAuthCodeTriggersSingleRefresh(t *testing.T) {
	var lists, refreshes int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cloudide/api/v3/trae/oauth/ExchangeToken" {
			refreshes++
			fmt.Fprint(w, `{"Result":{"Token":"fresh"}}`)
			return
		}
		lists++
		if r.Header.Get("X-Cloudide-Token") != "fresh" {
			fmt.Fprint(w, `{"code":"1001","message":"expired"}`)
			return
		}
		fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"ok"}]}]}`)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	cfg.AuthBaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:        "old",
		RefreshToken: "refresh",
		UserID:       "u",
		ClientID:     cfg.ClientID,
	}
	c, g := s.auth.snapshot()
	names, err := s.upstream.modelNames(context.Background(), c, g)
	if err != nil || strings.Join(names, ",") != "ok" || lists != 2 || refreshes != 1 {
		t.Fatalf("models=%v err=%v list=%d refresh=%d", names, err, lists, refreshes)
	}
}

func TestModelDiscoveryCannotPublishAfterAccountSwitch(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Cloudide-Token") == "A" {
			close(started)
			<-release
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"A-only"}]}]}`)
			return
		}
		fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"B-only"}]}]}`)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "A",
		UserID: "A",
	}
	c, g := s.auth.snapshot()
	done := make(chan error, 1)
	go func() {
		_, err := s.upstream.modelNamesAttempt(context.Background(), c, g)
		done <- err
	}()
	<-started
	s.auth.mu.Lock()
	s.auth.cred = credential{
		Token:  "B",
		UserID: "B",
	}
	s.auth.generation++
	s.auth.mu.Unlock()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale model discovery accepted")
	}
	c, g = s.auth.snapshot()
	names, err := s.upstream.modelNamesAttempt(context.Background(), c, g)
	if err != nil || strings.Join(names, ",") != "B-only" {
		t.Fatalf("B list=%v err=%v", names, err)
	}
}

func TestFilteredBatchFallsBackToDetail(t *testing.T) {
	var details int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ide/v1/batch_get_detail_param" {
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"off","config_switch":false}]}]}`)
			return
		}
		details++
		fmt.Fprint(w, `{"config_info_list":[{"config_name":"available"}]}`)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "test",
		UserID: "test",
	}
	names, err := s.upstream.modelNamesAttempt(context.Background(), s.auth.cred, 0)
	if err != nil || strings.Join(names, ",") != "available" || details != 4 {
		t.Fatalf("models=%v err=%v details=%d", names, err, details)
	}
}

func TestDeviceExchangeNetworkFailureDoesNotTrySecondClient(t *testing.T) {
	var attempts int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(500)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, _ := newAuthManager(cfg)
	key, _ := newDeviceKey()
	_, err := a.exchange(context.Background(), credential{
		Token:        "old",
		RefreshToken: "r",
		UserID:       "u",
		DeviceID:     "1234567890123456789",
		MachineID:    "machine",
		DeviceKey:    key,
	})
	if err == nil || attempts != 1 {
		t.Fatalf("uncertain exchange retried: attempts=%d err=%v", attempts, err)
	}
}

func TestModelFallbackPartialAndNoAuthFallback(t *testing.T) {
	var batch, details int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			batch++
			w.WriteHeader(500)
		case "/api/ide/v1/get_detail_param":
			details++
			if strings.Contains(r.URL.RawQuery, "impossible") {
				t.Fatal("wrong query")
			}
			if details == 1 {
				w.WriteHeader(500)
				return
			}
			fmt.Fprint(w, `{"data":{"config_info_list":[{"config_name":"ok","usage":"chat_completion"},{"config_name":"custom_model_1"},{"config_name":"off","config_switch":false},{"config_name":"embed","usage":"embedding"}]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "x",
		UserID: "u",
	}
	c, g, _ := s.auth.get(context.Background())
	names, err := s.upstream.modelNames(context.Background(), c, g)
	if err != nil || strings.Join(names, ",") != "ok" || batch != 1 || details != 4 {
		t.Fatalf("models=%v err=%v batch=%d details=%d", names, err, batch, details)
	}
	names, err = s.upstream.modelNames(context.Background(), c, g)
	if err != nil || len(names) != 1 || batch != 1 {
		t.Fatalf("cache mismatch %v %v", names, err)
	}
}

func TestBoundRefreshNeverFallsBackToLegacy(t *testing.T) {
	var bound, legacy int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == deviceExchangePath {
			bound++
			w.WriteHeader(403)
			return
		}
		legacy++
		t.Errorf("keyed account attempted legacy exchange: %s", r.URL.Path)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, _ := newAuthManager(cfg)
	key, _ := newDeviceKey()
	a.cred = credential{
		Token:        "old",
		RefreshToken: "refresh",
		UserID:       "u",
		DeviceID:     "1234567890123456789",
		MachineID:    "machine",
		DeviceKey:    key,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	if _, _, err := a.get(context.Background()); err != errLoginRequired || bound != 2 || legacy != 0 {
		t.Fatalf("err=%v bound=%d legacy=%d", err, bound, legacy)
	}
}

func TestEmptyBatchFallsBackToDetail(t *testing.T) {
	var detail int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ide/v1/batch_get_detail_param" {
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[]}]}`)
			return
		}
		if r.URL.Path == "/api/ide/v1/get_detail_param" {
			detail++
			fmt.Fprint(w, `{"config_info_list":[{"config_name":"only-detail"}]}`)
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "test",
		UserID: "test",
	}
	names, err := s.upstream.modelNamesAttempt(context.Background(), credential{
		Token: "test",
	}, 0)
	if err != nil || strings.Join(names, ",") != "only-detail" || detail != 4 {
		t.Fatalf("names=%v detail=%d err=%v", names, detail, err)
	}
}

func TestMaxWindowFromAnotherFunctionEntry(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"m","model_detail_list":[{"model_name":"m__max","max_tokens":1024}]}]},{"function":"solo_agent","config_info_list":[{"config_name":"m","model_detail_list":[{"model_name":"m__dev","max_tokens":256}],"context_window_tokens":{"dev":2000,"max":4000}}]}]}`)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "test",
		UserID: "test",
	}
	names, err := s.upstream.modelNamesAttempt(context.Background(), credential{
		Token: "test",
	}, 0)
	if err != nil || strings.Join(names, ",") != "m,m-max" || s.upstream.routes["m-max"].window != 4000 || s.upstream.routes["m-max"].function != "chat_v3" {
		t.Fatalf("names=%v err=%v max=%+v", names, err, s.upstream.routes["m-max"])
	}
}

func TestPartialDeviceBindingCannotDowngrade(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("partial device attempted network exchange %s", r.URL.Path)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, _ := newAuthManager(cfg)
	for _, c := range []credential{
		{
			RefreshToken: "refresh",
			DeviceID:     "1234567890123456789",
		},
		{
			RefreshToken: "refresh",
			MachineID:    "machine",
		},
	} {
		_, err := a.exchange(context.Background(), c)
		if err != errLoginRequired {
			t.Fatalf("partial device binding used legacy exchange: %v", err)
		}
	}
}
