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

func TestQA004DefectString401MustRefreshOnce(t *testing.T) {
	var batch, refresh, detail int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			batch++
			if r.Header.Get("X-Cloudide-Token") == "fresh" {
				fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"m"}]}]}`)
			} else {
				fmt.Fprint(w, `{"code":"401","message":"expired"}`)
			}
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			refresh++
			fmt.Fprintf(w, `{"Result":{"Token":"fresh","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
		default:
			detail++
			fmt.Fprint(w, `{"config_info_list":[]}`)
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL, cfg.AuthBaseURL = up.URL, up.URL
	s := authenticatedServer(t, cfg)
	defer s.shutdown()
	s.auth.cred.RefreshToken = "synthetic-refresh"
	c, g := s.auth.snapshot()
	names, err := s.upstream.modelNames(context.Background(), c, g)
	if err != nil || strings.Join(names, ",") != "m" || batch != 2 || refresh != 1 || detail != 0 {
		t.Fatalf("string 401 should refresh once before fallback; names=%v err=%v batch=%d refresh=%d detail=%d", names, err, batch, refresh, detail)
	}
}

func TestQA004DefectMaxWindowNonnegative(t *testing.T) {
	body := buildRoutedIDEBody(chatRequest{Model: "m-max", MaxTokens: 2000}, "synthetic-id", modelRoute{function: "chat_v3", base: "m", name: "m__max", most: 2000, window: 1000})
	info := body["user_message_context"].(map[string]any)["model_info"].(map[string]any)
	budget := info["prompt_max_tokens"].(int)
	limit, ok := body["max_tokens"].(int)
	if !ok || limit <= 0 || limit > 1000 || budget < 0 || budget+limit > 1000 {
		t.Fatalf("max window=1000 output=%v sent prompt_max_tokens=%d", body["max_tokens"], budget)
	}
}
