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

func TestTRAE004String401RefreshOnce(t *testing.T) {
	for _, again := range []bool{false, true} {
		t.Run(fmt.Sprint(again), func(t *testing.T) {
			batches, details, refreshes := 0, 0, 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/cloudide/api/v3/trae/oauth/ExchangeToken":
					refreshes++
					fmt.Fprint(w, `{"Result":{"Token":"fresh"}}`)
				case "/api/ide/v1/batch_get_detail_param":
					batches++
					if r.Header.Get("X-Cloudide-Token") == "fresh" && !again {
						fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"available"}]}]}`)
						return
					}
					fmt.Fprint(w, `{"code":"401","message":"expired"}`)
				case "/api/ide/v1/get_detail_param":
					details++
					t.Error("auth rejection fell through to detail")
				default:
					t.Errorf("unexpected upstream path %s", r.URL.Path)
				}
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
			if batches != 2 || refreshes != 1 || details != 0 {
				t.Fatalf("batch=%d refresh=%d detail=%d err=%v", batches, refreshes, details, err)
			}
			if again {
				if err != errLoginRequired {
					t.Fatalf("second rejection err=%v", err)
				}
			} else if err != nil || strings.Join(names, ",") != "available" {
				t.Fatalf("models=%v err=%v", names, err)
			}
		})
	}
}

func TestTRAE004MaxBudgetStaysWithinWindow(t *testing.T) {
	for _, asked := range []int{0, 2000, 500} {
		t.Run(fmt.Sprint(asked), func(t *testing.T) {
			r := chatRequest{
				Model:     "m-max",
				MaxTokens: asked,
			}
			if asked == 2000 {
				r.MaxTokens = 10
				r.MaxCompletionTokens = asked
			}
			body := buildRoutedIDEBody(r, "id", modelRoute{
				function: "chat_v3",
				base:     "m",
				name:     "m__max",
				most:     2000,
				window:   1000,
			})
			limit, ok := body["max_tokens"].(int)
			if !ok {
				t.Fatalf("missing max_tokens: %v", body)
			}
			info := body["user_message_context"].(map[string]any)["model_info"].(map[string]any)
			prompt := info["prompt_max_tokens"].(int)
			if limit <= 0 || limit > 1000 || prompt < 0 || prompt+limit > 1000 {
				t.Fatalf("invalid budget: output=%d prompt=%d", limit, prompt)
			}
		})
	}
}

func TestTRAE004UnknownExplicitStreamingModelRejectedBeforeOutput(t *testing.T) {
	batches, prompts := 0, 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			batches++
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"known"}]}]}`)
		case "/api/agent/v3/llm_utils_chat":
			prompts++
			t.Error("unknown model sent to inference")
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "synthetic",
		UserID: "u",
	}
	rr := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "missing", "stream": true, "messages": []message{{
		Role:    "user",
		Content: "hello",
	}}}, "")
	var errorBody map[string]any
	if rr.Code != 400 || strings.Contains(rr.Body.String(), "data:") || json.Unmarshal(rr.Body.Bytes(), &errorBody) != nil || errorBody["error"] == nil || batches != 1 || prompts != 0 {
		t.Fatalf("status=%d body=%q batches=%d prompts=%d", rr.Code, rr.Body.String(), batches, prompts)
	}
}
