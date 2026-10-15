package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTRAE008APIUpstreamHeadersAcceptOnlyAccessSnapshot(t *testing.T) {
	c := apiCredential{
		Version:   credentialVersion,
		Token:     "synthetic-access",
		UserID:    "fixture",
		DeviceID:  "synthetic-device",
		MachineID: "synthetic-machine",
	}
	client := &traeClient{}
	headers := client.headers(c)
	if headers.Get("X-Cloudide-Token") != c.Token || headers.Get("x-device-id") != c.DeviceID {
		t.Fatal("snapshot was not used for upstream headers")
	}
}

func TestTRAE008APIHealthReadsCredentialFile(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(syntheticCredential(t, "synthetic-access")); err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig()
	cfg.APIToken = "synthetic-gateway"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8723/healthz", nil)
	req.Header.Set("Authorization", "Bearer synthetic-gateway")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	var body struct {
		LoggedIn bool `json:"logged_in"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || !body.LoggedIn {
		t.Fatalf("health status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTRAE008APIDoesNotServeAuthenticationRoutes(t *testing.T) {
	cfg := fixtureConfig()
	cfg.APIToken = "synthetic-gateway"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	for _, path := range []string{"/api/auth/login", "/api/auth/login/status", "/api/auth/login/cancel", "/authorize"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8723"+path, nil)
		req.Header.Set("Authorization", "Bearer synthetic-gateway")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("API exposed %s: HTTP %d", path, w.Code)
		}
	}
}

func TestTRAE008APIModelsUsesFileCredentialWithoutRefreshing(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(syntheticCredential(t, "synthetic-access")); err != nil {
		t.Fatal(err)
	}
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/ide/v1/batch_get_detail_param" || r.Header.Get("X-Cloudide-Token") != "synthetic-access" {
			t.Errorf("unexpected upstream request %s token=%q", r.URL.Path, r.Header.Get("X-Cloudide-Token"))
		}
		fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"model-a"}]}]}`)
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	cfg.APIToken = "synthetic-gateway"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8723/v1/models", nil)
	req.Header.Set("Authorization", "Bearer synthetic-gateway")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != 200 || requests != 1 {
		t.Fatalf("models HTTP %d upstream requests=%d body=%s", w.Code, requests, w.Body.String())
	}
}

func TestTRAE008ModelFallbackAndMaxRoute(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(syntheticCredential(t, "synthetic-access")); err != nil {
		t.Fatal(err)
	}
	detailCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"off","config_switch":false}]}]}`)
		case "/api/ide/v1/get_detail_param":
			detailCalls++
			fmt.Fprint(w, `{"config_info_list":[{"config_name":"m","model_detail_list":[{"model_name":"m__dev","max_tokens":256},{"model_name":"m__max","max_tokens":1024}],"context_window_tokens":{"dev":2000,"max":4000}}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8723/v1/models", nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != 200 || detailCalls == 0 || !strings.Contains(w.Body.String(), `"id":"m-max"`) {
		t.Fatalf("fallback HTTP %d calls=%d body=%s", w.Code, detailCalls, w.Body.String())
	}
	c, err := readAPICredential()
	if err != nil {
		t.Fatal(err)
	}
	got := s.upstream.cache[snapshotIdentity(c)].routes["m-max"]
	if got.name != "m__max" || got.window != 4000 {
		t.Fatalf("Max route=%+v", got)
	}
}

func TestTRAE008ModelCacheBoundedAndExpiresPerSnapshotRoutes(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"%s"}]}]}`, r.Header.Get("X-Cloudide-Token"))
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	client := &traeClient{
		cfg:  cfg,
		http: newHTTPClient(5 * time.Second),
		cache: map[uint64]modelCache{
			999: {
				models: []string{"expired"},
				at:     time.Now().Add(-6 * time.Minute),
				routes: map[string]modelRoute{
					"expired": {
						name: "expired",
					},
				},
			},
		},
	}
	for i := 0; i < 10; i++ {
		credential := apiCredential{
			Token: fmt.Sprintf("model-%d", i),
		}
		key := snapshotIdentity(credential)
		names, err := client.modelNamesAttempt(context.Background(), credential, key)
		if err != nil || len(names) != 1 || names[0] != credential.Token {
			t.Fatalf("snapshot %d: names=%v err=%v", i, names, err)
		}
		if len(client.cache) > 8 {
			t.Fatalf("unbounded cache: %d entries", len(client.cache))
		}
		if _, ok := client.cache[999]; ok {
			t.Fatal("expired cache route retained")
		}
		if route := client.cache[key].routes[credential.Token]; route.base != credential.Token {
			t.Fatalf("incorrect per-snapshot route %q: %+v", credential.Token, route)
		}
	}
	if calls != 10 {
		t.Fatalf("discovery calls=%d, want 10", calls)
	}
	for _, token := range []string{"model-8", "model-9"} {
		credential := apiCredential{
			Token: token,
		}
		key := snapshotIdentity(credential)
		names, err := client.modelNamesAttempt(context.Background(), credential, key)
		if err != nil || len(names) != 1 || names[0] != token || client.cache[key].routes[token].base != token {
			t.Fatalf("cached snapshot %s: names=%v err=%v", token, names, err)
		}
	}
	if calls != 10 {
		t.Fatalf("fresh snapshots did not hit their own cache: calls=%d", calls)
	}
}

func TestTRAE008ModelString403DoesNotRetry(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(syntheticCredential(t, "synthetic-access")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/ide/v1/batch_get_detail_param" {
			t.Errorf("unexpected retry or refresh %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"code":"403"}`)
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8723/v1/models", nil))
	if w.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("model rejection HTTP %d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

func TestTRAE008ModelString401DoesNotRefresh(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(syntheticCredential(t, "synthetic-access")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/ide/v1/batch_get_detail_param" {
			t.Errorf("unexpected retry or refresh %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"code":"401"}`)
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8723/v1/models", nil))
	if w.Code != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("model rejection HTTP %d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

func TestTRAE008SSEAuthBusinessCodesReject(t *testing.T) {
	for _, code := range []string{`"401"`, `"403"`, `401`, `403`} {
		t.Run(code, func(t *testing.T) {
			frames := "event: output\ndata: {\"code\":" + code + ",\"response\":\"not a success\"}\n\nevent: done\ndata: {}\n\n"
			_, err := readIDEStream(strings.NewReader(frames), chatRequest{
				Model:  "m",
				Choice: "auto",
			}, nil)
			if !errors.Is(err, errCredentialRejected) {
				t.Fatalf("SSE auth code %s accepted as response: %v", code, err)
			}
		})
	}
}

func TestTRAE008StartedChatStreamAuthErrorHasNoSuccessEnd(t *testing.T) {
	for _, code := range []string{`"401"`, `403`} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/agent/v3/llm_utils_chat" {
					t.Errorf("unexpected upstream path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: output\ndata: {\"code\":"+code+",\"response\":\"rejected\"}\n\nevent: done\ndata: {}\n\n")
			}))
			defer upstream.Close()
			cfg := fixtureConfig()
			cfg.BaseURL = upstream.URL
			s := authenticatedServer(t, cfg)
			w := callAPI(t, s, "/v1/chat/completions", map[string]any{
				"model":    "m",
				"messages": []map[string]string{{"role": "user", "content": "synthetic"}},
				"stream":   true,
			}, "")
			body := w.Body.String()
			if w.Code != http.StatusOK || calls != 1 || strings.Count(body, `"type":"authentication_error"`) != 1 || strings.Contains(body, `"type":"server_error"`) || strings.Contains(body, "data: [DONE]") || strings.Contains(body, `"finish_reason":"stop"`) {
				t.Fatalf("started stream auth error: status=%d calls=%d body=%s", w.Code, calls, body)
			}
		})
	}
}
