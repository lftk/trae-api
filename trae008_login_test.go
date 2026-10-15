package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTRAE008LoginCallsAuthWithoutGatewayToken(t *testing.T) {
	requests := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/auth/login" || r.Header.Get("Authorization") != "" {
			t.Errorf("wrong Auth request %s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"pending"}`))
	}))
	defer mock.Close()
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:1"
	cfg.AuthAddr = strings.TrimPrefix(mock.URL, "http://")
	cfg.APIToken = "synthetic-gateway"
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	_, err := loginCall(context.Background(), client, cfg, "/api/auth/login", map[string]string{})
	if err != nil || requests != 1 {
		t.Fatalf("Auth calls=%d err=%v", requests, err)
	}
}

func TestTRAE008LoginTargetChecksAuthAddress(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "203.0.113.1:8723"
	cfg.AuthAddr = net.JoinHostPort("127.0.0.1", "8765")
	if !localLoginTarget(cfg) {
		t.Fatal("local Auth target rejected because API is remote")
	}
	cfg.AuthAddr = "203.0.113.1:8765"
	if localLoginTarget(cfg) {
		t.Fatal("remote Auth target accepted")
	}
}

func TestTRAE008CLILoginUsesAuthAndPublishesFile(t *testing.T) {
	a := newIsolatedAuth(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var callback string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.manager.mu.Lock()
		callback = a.manager.callbackURL
		a.manager.mu.Unlock()
		if callback != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if callback == "" {
		t.Fatal("Auth did not become ready")
	}
	callbackURL, _ := url.Parse(callback)
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:1"
	cfg.AuthAddr = callbackURL.Host
	cfg.APIToken = "different-gateway-token"
	var out bytes.Buffer
	opener := func(_ context.Context, link string) error {
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		q := url.Values{
			"loginTraceID": {u.Query().Get("login_trace_id")},
			"userJwt":      {fmt.Sprintf(`{"Token":"synthetic-upstream-access","RefreshToken":"synthetic-upstream-refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}`, time.Now().Add(time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix())},
			"userInfo":     {`{"UserID":"fixture","Region":"CN"}`},
		}
		client := newHTTPClient(time.Second)
		client.Transport.(*http.Transport).Proxy = nil
		defer client.CloseIdleConnections()
		resp, err := client.Get(callback + "?" + q.Encode())
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("synthetic callback HTTP %d", resp.StatusCode)
		}
		return nil
	}
	if err := runLogin(ctx, cfg, &out, opener, time.Millisecond, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "synthetic-upstream-access") || strings.Contains(out.String(), "synthetic-upstream-refresh") {
		t.Fatal("credential leaked to CLI output")
	}
	saved, err := readAPICredential()
	if err != nil || saved.Token != "synthetic-upstream-access" {
		t.Fatalf("Auth did not publish access snapshot: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTRAE008CLILoginReportsUncertainDurability(t *testing.T) {
	id := strings.Repeat("a", 32)
	cfg := fixtureConfig()
	link, err := url.Parse(cfg.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := link.Query()
	query.Set("login_trace_id", id)
	link.RawQuery = query.Encode()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/login":
			fmt.Fprintf(w, `{"status":"pending","attempt_id":%q,"url":%q}`, id, link.String())
		case "/api/auth/login/status":
			fmt.Fprint(w, `{"status":"durability_uncertain"}`)
		case "/api/auth/login/cancel":
			fmt.Fprint(w, `{"status":"cancelled"}`)
		default:
			t.Errorf("unexpected login route %s", r.URL.Path)
		}
	}))
	defer mock.Close()
	cfg.AuthAddr = strings.TrimPrefix(mock.URL, "http://")
	var out bytes.Buffer
	err = runLogin(context.Background(), cfg, &out, func(context.Context, string) error { return nil }, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "durability") || strings.Contains(err.Error(), "invalid login status") {
		t.Fatalf("uncertain login silently succeeded or lacked guidance: %v", err)
	}
}
