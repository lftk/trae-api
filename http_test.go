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

func authenticatedServer(t *testing.T, cfg config) *server {
	t.Helper()
	s, e := newServer(cfg)
	if e != nil {
		t.Fatal(e)
	}
	s.auth.mu.Lock()
	s.auth.cred = credential{
		Token:    "test-token",
		UserID:   "account",
		ClientID: cfg.ClientID,
	}
	s.auth.mu.Unlock()
	// Explicit-model transport tests isolate their concern from discovery.
	s.upstream.routes = map[string]modelRoute{"m": {
		function: "solo_work_lite",
		base:     "m",
	}}
	s.upstream.modelsAt = time.Now()
	t.Cleanup(s.shutdown)
	return s
}
func callAPI(t *testing.T, s *server, path string, body any, session string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://127.0.0.1:8723"+path, strings.NewReader(jsonText(body)))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("X-Session-ID", session)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}
func decoded(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &m); e != nil {
		t.Fatalf("invalid JSON: %s", w.Body.String())
	}
	return m
}
func events(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			var m map[string]any
			if e := json.Unmarshal([]byte(line[6:]), &m); e != nil {
				t.Fatal(e)
			}
			out = append(out, m)
		}
	}
	return out
}

func TestTRAE001ManagementHost(t *testing.T) {
	s := authenticatedServer(t, fixtureConfig())
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	for _, host := range []string{"attacker.invalid:8723", "localhost.attacker.invalid:8723", "127.0.0.1.attacker.invalid:8723", "0.0.0.0:8723", "[::]:8723"} {
		for _, route := range []struct {
			method string
			path   string
		}{{
			"GET",
			"/web/login",
		}, {
			"GET",
			"/api/auth/status",
		}, {
			"POST",
			"/api/auth/login",
		}, {
			"POST",
			"/api/auth/logout",
		}} {
			t.Run(host+route.path, func(t *testing.T) {
				r := httptest.NewRequest(route.method, "http://"+host+route.path, nil)
				r.Header.Set("Origin", "http://"+host)
				r.Header.Set("X-Forwarded-Host", "localhost:8723")
				w := httptest.NewRecorder()
				authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })).ServeHTTP(w, r)
				if w.Code != 403 {
					t.Fatalf("malicious host allowed: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	c, _ := s.auth.snapshot()
	if c.Token == "" {
		t.Fatal("denied logout changed account")
	}
	for _, host := range []string{"localhost:8723", "127.0.0.1:8723", "[::1]:8723"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/auth/status", nil)
		w := httptest.NewRecorder()
		authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("local host rejected %s: %d", host, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://localhost:8723/api/auth/status", nil)
	r.Header.Set("Origin", "https://attacker.invalid")
	w := httptest.NewRecorder()
	authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })).ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), `"user_id"`) {
		t.Fatal("cross-origin management read allowed")
	}
}

func TestTRAE003StatelessChat(t *testing.T) {
	var bodies []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		fmt.Fprint(w, textFrames("answer"))
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	for _, text := range []string{"first-only", "second-only"} {
		w := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "m", "messages": []message{{
			Role:    "user",
			Content: text,
		}}}, "same")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if len(bodies) != 2 || strings.Contains(jsonText(bodies[1]), "first-only") || strings.Contains(jsonText(bodies[1]), "answer") {
		t.Fatalf("server retained history: %s", jsonText(bodies))
	}
	if bodies[0]["session_id"] == bodies[1]["session_id"] {
		t.Fatal("upstream session reused")
	}
}
func TestTRAE003ResponsesUnsupported(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, textFrames("answer"))
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	w := callAPI(t, s, "/v1/responses", map[string]any{"model": "m", "input": "hello"}, "")
	if w.Code != 501 || !strings.Contains(w.Body.String(), "unsupported") || calls.Load() != 0 {
		t.Fatalf("status=%d upstream=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
}
func TestTRAE003NoCredentialRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	t.Setenv("TRAE_API_STATE_DIR", filepath.Join(home, "trae-api"))
	dir := filepath.Join(home, "trae-api")
	os.MkdirAll(dir, 0700)
	file := filepath.Join(dir, "credential.json")
	original := []byte(`{"token":"synthetic-old-token","user_id":"fixture"}`)
	os.WriteFile(file, original, 0600)
	cfg := fixtureConfig()
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.shutdown()
	c, _ := s.auth.snapshot()
	if c.Token != "" {
		t.Fatal("old credentials restored")
	}
	after, _ := os.ReadFile(file)
	if string(after) != string(original) {
		t.Fatal("old file changed")
	}
}
