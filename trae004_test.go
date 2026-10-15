package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceProofAndBoundRefresh(t *testing.T) {
	var path string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if path != "/trae/api/v3/oauth/ExchangeToken" {
			t.Errorf("unexpected path %s", path)
			w.WriteHeader(500)
			return
		}
		var b struct {
			ClientID     string
			ClientSecret string
			RefreshToken string
			IDEVersion   string
			DeviceInfo   struct {
				DeviceID        string
				MachineID       string
				PlatformCode    string
				DevicePublicKey string
			}
			DeviceProof struct {
				Signature string
				Nonce     string
				Timestamp int64
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
			return
		}
		block, _ := pem.Decode([]byte(b.DeviceInfo.DevicePublicKey))
		if block == nil {
			t.Error("missing PEM key")
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			t.Error(err)
			return
		}
		sig, err := base64.StdEncoding.DecodeString(b.DeviceProof.Signature)
		if err != nil {
			t.Error(err)
			return
		}
		text := fmt.Sprintf("POST\n%s\n%s\n%s\n%d\n%s", path, b.ClientID, b.RefreshToken, b.DeviceProof.Timestamp, b.DeviceProof.Nonce)
		digest := sha256.Sum256([]byte(text))
		if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), digest[:], sig) {
			t.Error("invalid proof")
		}
		if b.ClientID != "en1oxy7wnw8j9n" || b.ClientSecret != "" || b.DeviceInfo.PlatformCode != "SOLO_PC" || b.DeviceInfo.DeviceID != "1234567890123456789" || b.DeviceInfo.MachineID != "machine" || b.DeviceProof.Nonce == "" || b.IDEVersion != "0.1.64" {
			t.Errorf("bad device request %+v", b)
		}
		writeJSON(w, 200, map[string]any{"Result": map[string]any{
			"Token":         "bound",
			"RefreshToken":  "rotated",
			"TokenExpireAt": time.Now().Add(time.Hour).Unix(),
		}})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, _ := newAuthManager(cfg)
	key, err := newDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{
		Token:        "old",
		RefreshToken: "refresh",
		UserID:       "u",
		ClientID:     cfg.ClientID,
		DeviceID:     "1234567890123456789",
		MachineID:    "machine",
		DeviceKey:    key,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	c, _, err := a.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "bound" || c.RefreshToken != "rotated" || c.DeviceID != "1234567890123456789" || c.DeviceKey == nil {
		t.Fatalf("credential lost binding: %+v", c)
	}
}

func TestLoginDeviceStableAndKeyInMemory(t *testing.T) {
	a, _ := newAuthManager(fixtureConfig())
	a.callbackURL = "http://127.0.0.1:8765/authorize"
	raw, err := a.beginLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	id := q.Get("device_id")
	if len(id) != 19 || id[0] == '0' || q.Get("x_device_id") != id || q.Get("x_app_version") != "3.3.65" || q.Get("x_device_type") != "windows" {
		t.Fatalf("bad device URL metadata %q", raw)
	}
	a.mu.Lock()
	p := a.pending[q.Get("login_trace_id")]
	a.mu.Unlock()
	if p.key == nil || p.device != id || len(p.machine) != 32 {
		t.Fatal("device not retained for callback")
	}
	callback := "/authorize?loginTraceID=" + q.Get("login_trace_id") + "&userInfo=%7B%22UserID%22%3A%22u%22%7D&userJwt=%7B%22Token%22%3A%22token%22%2C%22RefreshToken%22%3A%22refresh%22%7D"
	rr := httptest.NewRecorder()
	a.callback(rr, httptest.NewRequest("GET", callback, nil))
	if rr.Code != 200 {
		t.Fatal(rr.Code)
	}
	c, _ := a.snapshot()
	if c.DeviceID != id || c.DeviceKey == nil {
		t.Fatal("device changed after callback")
	}
	_ = a.logout()
	c, _ = a.snapshot()
	if c.DeviceKey != nil || c.Token != "" {
		t.Fatal("logout retained device key")
	}
}

func TestBatchModelsRoutingAndMax(t *testing.T) {
	var batch int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ide/v1/batch_get_detail_param":
			batch++
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["functions"] == nil {
				t.Error("no functions")
			}
			fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"alpha","usage":"chat_completion","model_detail_list":[{"model_name":"alpha__max","max_tokens":4096}],"context_window_tokens":{"dev":8000,"max":16000}},{"config_name":"custom_model_slot"},{"config_name":"off","config_switch":false}]},{"function":"solo_agent","config_info_list":[{"config_name":"alpha","usage":"chat_completion","model_detail_list":[{"model_name":"alpha__dev","max_tokens":2048}],"context_window_tokens":{"dev":8000,"max":16000}},{"config_name":"beta","usage":"embedding"}]}]}`)
		case "/api/agent/v3/llm_utils_chat":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["config_name"] != "alpha" || b["function"] != "chat_v3" || b["model_name"] != "alpha__max" || b["max_tokens"] != float64(4096) {
				t.Errorf("bad max route: %+v", b)
			}
			ctx := b["user_message_context"].(map[string]any)["model_info"].(map[string]any)
			if ctx["prompt_max_tokens"] != float64(11904) {
				t.Errorf("bad max context: %v", ctx)
			}
			fmt.Fprint(w, textFrames("ok"))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, _ := newServer(cfg)
	defer s.shutdown()
	s.auth.cred = credential{
		Token:  "token",
		UserID: "u",
	}
	c, g, _ := s.auth.get(context.Background())
	names, err := s.upstream.modelNames(context.Background(), c, g)
	if err != nil || strings.Join(names, ",") != "alpha,alpha-max" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	_, res, err := s.execute(context.Background(), chatRequest{
		Model:     "alpha-max",
		MaxTokens: 5000,
		Messages: []message{{
			Role:    "user",
			Content: "hi",
		}},
	}, nil)
	if err != nil || res.Message.Content != "ok" || batch != 1 {
		t.Fatalf("result=%+v err=%v batch=%d", res, err, batch)
	}
}

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
	body := buildRoutedIDEBody(chatRequest{
		Model:     "m-max",
		MaxTokens: 2000,
	}, "synthetic-id", modelRoute{
		function: "chat_v3",
		base:     "m",
		name:     "m__max",
		most:     2000,
		window:   1000,
	})
	info := body["user_message_context"].(map[string]any)["model_info"].(map[string]any)
	budget := info["prompt_max_tokens"].(int)
	limit, ok := body["max_tokens"].(int)
	if !ok || limit <= 0 || limit > 1000 || budget < 0 || budget+limit > 1000 {
		t.Fatalf("max window=1000 output=%v sent prompt_max_tokens=%d", body["max_tokens"], budget)
	}
}

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
	w := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "missing", "stream": true, "messages": []message{{
		Role:    "user",
		Content: "hi",
	}}}, "")
	var response map[string]any
	if w.Code != 400 || strings.Contains(w.Body.String(), "data:") || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["error"] == nil {
		t.Fatalf("invalid explicit model returned HTTP %d; sse=%t; body=%q", w.Code, strings.Contains(w.Body.String(), "data:"), w.Body.String())
	}
}

type qa004WaitingContext struct {
	context.Context
	entered chan<- struct{}
}

func (c qa004WaitingContext) Done() <-chan struct{} {
	c.entered <- struct{}{}
	return c.Context.Done()
}

// All credentials and upstream responses here are synthetic; no login or model call is made.
func TestQA004ConcurrentRefreshSharesOneExchange(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var exchanges atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceExchangePath {
			t.Errorf("bound refresh used legacy path: %s", r.URL.Path)
		}
		if exchanges.Add(1) == 1 {
			close(started)
		}
		<-release
		writeJSON(w, 200, map[string]any{"Result": map[string]any{
			"Token": "fresh-A", "UserID": "user-A", "TokenExpireAt": time.Now().Add(time.Hour).Unix(),
		}})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.client.CloseIdleConnections()
	key, err := newDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{
		Token:        "expired-A",
		UserID:       "user-A",
		RefreshToken: "refresh-A",
		DeviceID:     "device-A",
		MachineID:    "machine-A",
		DeviceKey:    key,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	const callers = 12
	var wg sync.WaitGroup
	wg.Add(1)
	results := make(chan error, callers)
	check := func(ctx context.Context) {
		defer wg.Done()
		c, _, err := a.get(ctx)
		if err == nil && (c.Token != "fresh-A" || c.UserID != "user-A" || c.DeviceID != "device-A" || c.MachineID != "machine-A" || c.DeviceKey != key) {
			err = fmt.Errorf("refresh changed token, account or device identity")
		}
		results <- err
	}
	go check(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("refresh never started")
	}
	// Done() is reached by each follower only in get's refreshing wait branch.
	// Do not release the exchange until all followers actually reached it.
	entered := make(chan struct{}, callers-1)
	wg.Add(callers - 1)
	for i := 1; i < callers; i++ {
		go check(qa004WaitingContext{
			Context: context.Background(),
			entered: entered,
		})
	}
	for i := 1; i < callers; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("follower did not enter shared-refresh wait")
		}
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("concurrent refresh made %d exchanges; want 1", got)
	}
}

func TestQA004RefreshTokenRotationThenOmissionRetainsLatest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"RefreshToken"`
			DeviceInfo   struct {
				DeviceID  string `json:"DeviceID"`
				MachineID string `json:"MachineID"`
			} `json:"DeviceInfo"`
		}
		if r.URL.Path != deviceExchangePath || r.Header.Get("Authorization") == "" {
			t.Errorf("exchange lost binding: path=%s auth_present=%t", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.DeviceInfo.DeviceID != "device-A" || body.DeviceInfo.MachineID != "machine-A" {
			t.Errorf("exchange changed device binding: %+v", body.DeviceInfo)
		}
		mu.Lock()
		seen = append(seen, body.RefreshToken)
		attempt := len(seen)
		mu.Unlock()
		result := map[string]any{
			"Token":         fmt.Sprintf("fresh-%d", attempt),
			"UserID":        "user-A",
			"TokenExpireAt": time.Now().Add(time.Hour).Unix(),
		}
		if attempt == 1 {
			result["RefreshToken"] = "refresh-rotated"
		}
		writeJSON(w, 200, map[string]any{"Result": result})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.client.CloseIdleConnections()
	key, err := newDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{
		Token:        "expired",
		UserID:       "user-A",
		RefreshToken: "refresh-original",
		DeviceID:     "device-A",
		MachineID:    "machine-A",
		DeviceKey:    key,
		ExpiresAt:    time.Now().Add(-time.Second),
	}
	for attempt := 1; attempt <= 2; attempt++ {
		c, _, err := a.get(context.Background())
		if err != nil || c.RefreshToken != "refresh-rotated" || c.UserID != "user-A" || c.DeviceID != "device-A" || c.MachineID != "machine-A" || c.DeviceKey != key {
			t.Fatalf("exchange %d: token retention/device binding failed: %v (refresh=%q device=%q)", attempt, err, c.RefreshToken, c.DeviceID)
		}
		if attempt == 1 {
			a.mu.Lock()
			a.cred.ExpiresAt = time.Now().Add(-time.Second)
			a.mu.Unlock()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "refresh-original" || seen[1] != "refresh-rotated" {
		t.Fatalf("refresh rotation was not used on next exchange: %v", seen)
	}
}

func TestQA004StringAuthCodeRetriesOnceButBusinessCodeDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        string
		wantLists   int
		wantRefresh int
		wantDetails int
	}{
		{
			"rejected_after_refresh",
			"1001",
			2,
			1,
			0,
		},
		{
			"business_error_discovery_fallback",
			"1002",
			1,
			0,
			4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lists, refreshes, details int
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/ide/v1/batch_get_detail_param":
					lists++
					fmt.Fprintf(w, `{"code":%q,"message":"synthetic rejection"}`, tc.code)
				case "/trae/api/v3/oauth/ExchangeToken", "/cloudide/api/v3/trae/oauth/ExchangeToken":
					refreshes++
					fmt.Fprint(w, `{"Result":{"Token":"fresh-A","UserID":"user-A"}}`)
				default:
					details++
					if r.URL.Path != "/api/ide/v1/get_detail_param" {
						t.Errorf("unexpected request: %s", r.URL.Path)
					}
					fmt.Fprint(w, `{"config_info_list":[{"config_name":"fallback-model"}]}`)
				}
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL, cfg.AuthBaseURL = up.URL, up.URL
			s := authenticatedServer(t, cfg)
			defer s.shutdown()
			s.auth.mu.Lock()
			s.auth.cred.RefreshToken = "synthetic-refresh-A"
			s.auth.cred.UserID = "user-A"
			s.auth.mu.Unlock()
			c, g := s.auth.snapshot()
			names, err := s.upstream.modelNames(context.Background(), c, g)
			if lists != tc.wantLists || refreshes != tc.wantRefresh || details != tc.wantDetails {
				t.Fatalf("code=%s err=%v batch=%d refresh=%d detail=%d", tc.code, err, lists, refreshes, details)
			}
			if tc.code == "1001" {
				if err == nil || !strings.Contains(err.Error(), "login required") {
					t.Fatalf("repeated auth rejection not mapped to login-required: %v", err)
				}
			} else if err != nil || strings.Join(names, ",") != "fallback-model" {
				t.Fatalf("non-auth batch error did not use discovery fallback: names=%v err=%v", names, err)
			}
		})
	}
}
