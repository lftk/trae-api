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
)

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
