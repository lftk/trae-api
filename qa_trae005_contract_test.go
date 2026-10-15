package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQA005ModelDiscoveryWireNullsAndFallback(t *testing.T) {
	batchCalls := 0
	detailCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("model discovery used %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			batchCalls++
			functions, ok := body["functions"].([]any)
			if !ok || len(functions) != 4 || body["mode_type"] != float64(0) || body["access_type"] != float64(0) || body["show_custom_model"] != true {
				t.Errorf("batch wire fields changed: %v", body)
			}
			current, ok := body["current_config_info"].(map[string]any)
			if !ok || current["config_name"] != "" || current["is_custom_model"] != false {
				t.Errorf("batch current_config_info changed: %v", body["current_config_info"])
			}
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"disabled","config_switch":false}]}]}`)
		case "/api/ide/v1/get_detail_param":
			detailCalls++
			for _, key := range []string{"config_names", "current_config_info", "mode_type", "agent_type"} {
				value, exists := body[key]
				if !exists || value != nil {
					t.Errorf("detail %s must be present and null; body=%v", key, body)
				}
			}
			if body["need_prompt"] != false || body["poly_prompt"] != true {
				t.Errorf("detail flags changed: %v", body)
			}
			if body["function"] == "chat_v3" {
				fmt.Fprint(w, `{"config_info_list":[{"config_name":"z","model_detail_list":[{"model_name":"z__dev","max_tokens":512},{"model_name":"z__max","max_tokens":1024}],"context_window_tokens":{"dev":2048,"max":4096}},{"config_name":"a"},{"config_name":"embed","usage":"embedding"},{"config_name":"custom_model_1"}]}`)
			} else {
				fmt.Fprint(w, `{"config_info_list":[]}`)
			}
		default:
			t.Errorf("unexpected model path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	server := authenticatedServer(t, cfg)
	defer server.shutdown()
	cred, generation := server.auth.snapshot()
	names, err := server.upstream.modelNames(context.Background(), cred, generation)
	if err != nil || strings.Join(names, ",") != "a,z,z-max" || batchCalls != 1 || detailCalls != 4 {
		t.Fatalf("discovery changed: names=%v err=%v batch=%d detail=%d", names, err, batchCalls, detailCalls)
	}
	route := server.upstream.routes["z-max"]
	if route.function != "chat_v3" || route.base != "z" || route.name != "z__max" || route.most != 1024 || route.window != 4096 {
		t.Fatalf("max metadata changed: %+v", route)
	}
}
