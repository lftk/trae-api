package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func apiFixture(t *testing.T, token string, expires time.Time) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path, err := credentialPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeAPIFixture(t, token, expires)
}
func writeAPIFixture(t *testing.T, token string, expires time.Time) {
	t.Helper()
	path, err := credentialPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"version": 1, "token": token, "expires_at": expires, "user_id": "user-" + token, "client_id": "client", "device_id": "device", "machine_id": "machine", "refresh_token": "PRIVATE-REFRESH", "device_private_key": "PRIVATE-KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func apiRequest(s *server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://localhost:8723"+path, nil))
	return w
}
func TestAPIComponentRotatesSnapshotWithoutAuthRoutes(t *testing.T) {
	apiFixture(t, "A", time.Now().Add(time.Hour))
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "ExchangeToken") {
			t.Error("API attempted refresh")
		}
		if r.URL.Path != "/api/ide/v1/batch_get_detail_param" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Cloud-IDE-JWT ")
		json.NewEncoder(w).Encode(map[string]any{"function_configs": []any{map[string]any{"function": "chat_v3", "config_info_list": []any{map[string]any{"config_name": "model-" + token, "model_detail_list": []any{map[string]any{"model_name": "native-" + token + "__dev"}}}}}}})
	}))
	defer upstream.Close()
	cfg := defaultConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	if got := apiRequest(s, "/healthz"); got.Code != 200 || !strings.Contains(got.Body.String(), `"logged_in":true`) {
		t.Fatalf("health: %d %s", got.Code, got.Body.String())
	}
	for _, path := range []string{"/api/auth/login", "/api/auth/login/status", "/api/auth/login/cancel", "/authorize"} {
		w := apiRequest(s, path)
		if w.Code != 404 {
			t.Errorf("%s exposed: %d", path, w.Code)
		}
	}
	for _, token := range []string{"A", "B"} {
		writeAPIFixture(t, token, time.Now().Add(time.Hour))
		w := apiRequest(s, "/v1/models")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "model-"+token) || strings.Contains(w.Body.String(), "model-"+map[string]string{"A": "B", "B": "A"}[token]) {
			t.Fatalf("snapshot %s: %d %s", token, w.Code, w.Body.String())
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls %d, want 2", got)
	}
}

func TestAPIComponentCredentialUnavailableNeverCallsUpstream(t *testing.T) {
	apiFixture(t, "A", time.Now().Add(time.Hour))
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer upstream.Close()
	cfg := defaultConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	path, _ := credentialPath()
	for _, fixture := range []struct {
		name   string
		change func()
	}{
		{
			name:   "expired",
			change: func() { writeAPIFixture(t, "A", time.Now().Add(-time.Hour)) },
		},
		{
			name:   "invalid",
			change: func() { os.WriteFile(path, []byte("not-json"), 0600) },
		},
		{
			name:   "missing",
			change: func() { os.Remove(path) },
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			fixture.change()
			if w := apiRequest(s, "/healthz"); w.Code != 200 || !strings.Contains(w.Body.String(), `"logged_in":false`) {
				t.Errorf("health %d %s", w.Code, w.Body.String())
			}
			if w := apiRequest(s, "/v1/models"); w.Code == 200 || w.Body.Len() == 0 {
				t.Errorf("models %d %s", w.Code, w.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Errorf("upstream called %d times", calls.Load())
	}
}

func testChat(s *server) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8723/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	s.routes().ServeHTTP(w, r)
	return w
}

func TestAPIComponentInflightRequestPinsInitialSnapshotAndRoute(t *testing.T) {
	apiFixture(t, "A", time.Now().Add(time.Hour))
	entered, release := make(chan struct{}), make(chan struct{})
	var prompts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Cloud-IDE-JWT ")
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			if token == "A" {
				close(entered)
				<-release
			}
			fmt.Fprintf(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"m","model_detail_list":[{"model_name":"native-%s__dev"}]}]}]}`, token)
		case "/api/agent/v3/llm_utils_chat":
			prompts.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if token == "A" && !strings.Contains(fmt.Sprint(body), "native-A__dev") {
				t.Errorf("A cross-routed: %v", body)
			}
			if token == "B" && !strings.Contains(fmt.Sprint(body), "native-B__dev") {
				t.Errorf("B cross-routed: %v", body)
			}
			fmt.Fprint(w, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	cfg := defaultConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- testChat(s) }()
	<-entered
	writeAPIFixture(t, "B", time.Now().Add(time.Hour))
	b := testChat(s)
	close(release)
	a := <-result
	if a.Code != 200 || b.Code != 200 || prompts.Load() != 2 {
		t.Fatalf("A %d %s; B %d %s; prompts %d", a.Code, a.Body.String(), b.Code, b.Body.String(), prompts.Load())
	}
}

func TestAPIComponentRejectedPromptNeverReplays(t *testing.T) {
	apiFixture(t, "A", time.Now().Add(time.Hour))
	var prompts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"m","model_detail_list":[{"model_name":"native__dev"}]}]}]}`)
		case "/api/agent/v3/llm_utils_chat":
			prompts.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected refresh path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	cfg := defaultConfig()
	cfg.BaseURL = upstream.URL
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	w := testChat(s)
	if w.Code != 401 || prompts.Load() != 1 {
		t.Fatalf("status=%d prompt calls=%d body=%s", w.Code, prompts.Load(), w.Body.String())
	}
}

func TestAPIComponentExplicitNonloopbackRequiresGatewayToken(t *testing.T) {
	cfg := defaultConfig()
	cfg.Addr = "192.0.2.1:8723"
	if err := validateAPIConfig(cfg); err == nil {
		t.Fatal("accepted nonloopback without gateway token")
	}
	cfg.APIToken = "synthetic-gateway"
	if err := validateAPIConfig(cfg); err != nil {
		t.Fatalf("rejected explicit interface with token: %v", err)
	}
}
