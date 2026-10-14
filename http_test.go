package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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
	s.auth.cred = credential{Token: "test-token", UserID: "account", ClientID: cfg.ClientID}
	s.auth.mu.Unlock()
	// Explicit-model transport tests isolate their concern from discovery.
	s.upstream.routes = map[string]modelRoute{"m": {function: "solo_work_lite", base: "m"}}
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
