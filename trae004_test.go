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
			ClientID, ClientSecret, RefreshToken, IDEVersion string
			DeviceInfo                                       struct{ DeviceID, MachineID, PlatformCode, DevicePublicKey string }
			DeviceProof                                      struct {
				Signature, Nonce string
				Timestamp        int64
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
		writeJSON(w, 200, map[string]any{"Result": map[string]any{"Token": "bound", "RefreshToken": "rotated", "TokenExpireAt": time.Now().Add(time.Hour).Unix()}})
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, _ := newAuthManager(cfg)
	key, err := newDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	a.cred = credential{Token: "old", RefreshToken: "refresh", UserID: "u", ClientID: cfg.ClientID, DeviceID: "1234567890123456789", MachineID: "machine", DeviceKey: key, ExpiresAt: time.Now().Add(-time.Second)}
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
	s.auth.cred = credential{Token: "token", UserID: "u"}
	c, g, _ := s.auth.get(context.Background())
	names, err := s.upstream.modelNames(context.Background(), c, g)
	if err != nil || strings.Join(names, ",") != "alpha,alpha-max" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	_, res, err := s.execute(context.Background(), chatRequest{Model: "alpha-max", MaxTokens: 5000, Messages: []message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil || res.Message.Content != "ok" || batch != 1 {
		t.Fatalf("result=%+v err=%v batch=%d", res, err, batch)
	}
}
