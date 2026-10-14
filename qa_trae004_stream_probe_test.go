package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQA004DefectUnknownExplicitModelStreamingStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ide/v1/batch_get_detail_param" {
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"known"}]}]}`)
			return
		}
		t.Errorf("unknown model reached chat endpoint %s", r.URL.Path)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	defer s.shutdown()
	w := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "missing", "stream": true, "messages": []message{{Role: "user", Content: "hi"}}}, "")
	var response map[string]any
	if w.Code != 400 || strings.Contains(w.Body.String(), "data:") || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["error"] == nil {
		t.Fatalf("invalid explicit model returned HTTP %d; sse=%t; body=%q", w.Code, strings.Contains(w.Body.String(), "data:"), w.Body.String())
	}
}
