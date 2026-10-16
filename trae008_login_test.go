package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mockAuthProof(w http.ResponseWriter, r *http.Request, key []byte) {
	challenge, err := hex.DecodeString(r.Header.Get("X-Trae-Auth-Challenge"))
	if err != nil || len(challenge) != 32 {
		return
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(challenge)
	w.Header().Set("X-Trae-Auth-Proof", hex.EncodeToString(mac.Sum(nil)))
}

func TestTRAE008LoginCallsAuthWithoutGatewayToken(t *testing.T) {
	requests := 0
	proofKey := []byte(strings.Repeat("k", 32))
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		mockAuthProof(w, r, proofKey)
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
	_, err := loginCall(context.Background(), client, cfg, "/api/auth/login", map[string]string{}, proofKey)
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
	proofKey := []byte(strings.Repeat("k", 32))
	cfg := fixtureConfig()
	link, err := url.Parse(cfg.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := link.Query()
	query.Set("login_trace_id", id)
	link.RawQuery = query.Encode()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mockAuthProof(w, r, proofKey)
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
	err = runLoginFlow(context.Background(), cfg, &out, func(context.Context, string) error { return nil }, time.Millisecond, time.Second, proofKey)
	if err == nil || !strings.Contains(err.Error(), "durability") || strings.Contains(err.Error(), "invalid login status") {
		t.Fatalf("uncertain login silently succeeded or lacked guidance: %v", err)
	}
}

func TestTRAE009LoginStartsAuthAndStaysForRenewal(t *testing.T) {
	isolatedCredentialPath(t)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	reserved.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = addr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	opener := func(_ context.Context, link string) error {
		u, err := url.Parse(link)
		if err != nil {
			return err
		}
		callback := u.Query().Get("auth_callback_url")
		if callback != "http://"+addr+"/authorize" {
			return fmt.Errorf("unexpected callback address")
		}
		q := url.Values{
			"loginTraceID": {u.Query().Get("login_trace_id")},
			"userJwt":      {fmt.Sprintf(`{"Token":"standalone-access","RefreshToken":"standalone-refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}`, time.Now().Add(time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix())},
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
			return fmt.Errorf("callback HTTP %d", resp.StatusCode)
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runLogin(ctx, cfg, &out, opener, time.Millisecond, 2*time.Second) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := readAPICredential()
		if err == nil && c.Token == "standalone-access" {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("login exited before usable snapshot: %v", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("login did not complete: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case e := <-done:
		t.Fatalf("Auth exited after login, renewal stopped: %v", e)
	case <-time.After(25 * time.Millisecond):
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("foreground Auth listener missing: %v", err)
	}
	resp.Body.Close()
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("foreground exit: %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Auth did not stop")
	}
	store, err := openCredentialStore()
	if err != nil {
		t.Fatalf("writer lock leaked: %v", err)
	}
	store.Close()
	if strings.Contains(out.String(), "standalone-access") || strings.Contains(out.String(), "standalone-refresh") {
		t.Fatal("credential leaked")
	}
}

func TestTRAE009OccupiedPortIsNotTreatedAsAuth(t *testing.T) {
	isolatedCredentialPath(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprint(w, `{"status":"ok","logged_in":true}`)
			return
		}
		fmt.Fprint(w, `{"status":"pending"}`)
	}))
	defer fake.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = strings.TrimPrefix(fake.URL, "http://")
	opens := 0
	err := runLogin(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error { opens++; return nil }, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "occupied") || opens != 0 {
		t.Fatalf("other program was trusted: err=%v opens=%d", err, opens)
	}
	store, err := openCredentialStore()
	if err != nil {
		t.Fatalf("failed login leaked Auth writer lock: %v", err)
	}
	store.Close()
}

func TestTRAE009RunningAuthCannotBlessAnotherOccupiedPort(t *testing.T) {
	a := newIsolatedAuth(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.manager.mu.Lock()
		ready := a.manager.callbackURL != ""
		a.manager.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Auth not ready")
		}
		time.Sleep(time.Millisecond)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","logged_in":true}`)
	}))
	defer fake.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = strings.TrimPrefix(fake.URL, "http://")
	opens := 0
	err := runLogin(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error { opens++; return nil }, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "different Auth address") || opens != 0 {
		t.Fatalf("unrelated port accepted while Auth writer active: %v opens=%d", err, opens)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTRAE009LoginLocalhostUsesBoundAuthHost(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(syntheticCredential(t, "saved-access")); err != nil {
		t.Fatal(err)
	}
	store.Close()
	reserved, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(reserved.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	reserved.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = net.JoinHostPort("localhost", port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		done <- runLogin(ctx, cfg, &out, func(context.Context, string) error { return fmt.Errorf("unexpected browser") }, time.Millisecond, time.Second)
	}()
	select {
	case e := <-done:
		t.Fatalf("localhost login failed instead of serving: %v", e)
	case <-time.After(80 * time.Millisecond):
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("localhost Auth did not stop")
	}
}

func TestTRAE009LoginTimeoutAndCancellationCleanUp(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			isolatedCredentialPath(t)
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := reserved.Addr().String()
			reserved.Close()
			cfg := fixtureConfig()
			cfg.AuthAddr = addr
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opened := make(chan struct{}, 1)
			result := make(chan error, 1)
			go func() {
				result <- runLogin(ctx, cfg, &bytes.Buffer{}, func(context.Context, string) error { opened <- struct{}{}; return nil }, time.Millisecond, 80*time.Millisecond)
			}()
			if mode == "cancel" {
				select {
				case <-opened:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("login did not open browser")
				}
			}
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), map[string]string{"timeout": "timed out", "cancel": "cancelled"}[mode]) {
					t.Fatalf("wrong %s outcome: %v", mode, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("login did not clean up")
			}
			store, err := openCredentialStore()
			if err != nil {
				t.Fatalf("writer lock retained on %s: %v", mode, err)
			}
			store.Close()
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatalf("listener retained on %s: %v", mode, err)
			}
			l.Close()
		})
	}
}

func TestTRAE009AuthLockAndHealthShapeDoNotAuthenticateWrongService(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","logged_in":true}`)
	}))
	defer fake.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = strings.TrimPrefix(fake.URL, "http://")
	if _, err := store.PublishAuthInstance(cfg.AuthAddr); err != nil {
		t.Fatal(err)
	}
	opens := 0
	err = runLogin(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error { opens++; return nil }, time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "not the verified Auth") || opens != 0 {
		t.Fatalf("health shape trusted as Auth proof: %v opens=%d", err, opens)
	}
}

func TestTRAE009ReuseLocalhostAuthUsesBoundHost(t *testing.T) {
	isolatedCredentialPath(t)
	reserved, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(reserved.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	reserved.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = net.JoinHostPort("localhost", port)
	a, err := newAuthService(cfg, cfg.AuthAddr)
	if err != nil {
		t.Fatal(err)
	}
	c := syntheticCredential(t, "saved-access")
	if err := a.store.Save(c); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.cred = c
	a.manager.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.manager.mu.Lock()
		ready := a.manager.callbackURL != ""
		a.manager.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Auth not ready")
		}
		time.Sleep(time.Millisecond)
	}
	var out bytes.Buffer
	err = runLogin(context.Background(), cfg, &out, func(context.Context, string) error { return fmt.Errorf("unexpected browser") }, time.Millisecond, time.Second)
	if err != nil || !strings.Contains(out.String(), "Already logged in.") {
		t.Fatalf("localhost Auth not reused: %v %s", err, out.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTRAE009VerifiedAuthReplacementCannotReceiveLogin(t *testing.T) {
	a := newIsolatedAuth(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	var callback string
	for {
		a.manager.mu.Lock()
		callback = a.manager.callbackURL
		a.manager.mu.Unlock()
		if callback != "" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Auth did not start")
		}
		time.Sleep(time.Millisecond)
	}
	u, err := url.Parse(callback)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cfg := fixtureConfig()
	cfg.AuthAddr = u.Host
	bound, proofKey, err := verifyRunningAuth(context.Background(), cfg)
	if err != nil || bound != cfg.AuthAddr || len(proofKey) != 32 {
		cancel()
		t.Fatalf("real Auth proof failed: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", bound)
	if err != nil {
		t.Fatal(err)
	}
	fakeCalls := 0
	fake := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fakeCalls++
		url := cfg.AuthURL + "?login_trace_id=" + strings.Repeat("a", 32)
		fmt.Fprintf(w, `{"status":"pending","attempt_id":"%s","url":%q}`, strings.Repeat("a", 32), url)
	}))
	fake.Listener = listener
	fake.Start()
	defer fake.Close()
	opens := 0
	err = runLoginFlow(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error { opens++; return nil }, time.Millisecond, time.Second, proofKey)
	if err == nil || !strings.Contains(err.Error(), "unverified Auth response") || fakeCalls != 1 || opens != 0 {
		t.Fatalf("replacement listener accepted: err=%v calls=%d opens=%d", err, fakeCalls, opens)
	}
}

func TestTRAE009ForegroundLoginKeepsAuthRefreshRunning(t *testing.T) {
	isolatedCredentialPath(t)
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != deviceExchangePath {
			t.Errorf("unexpected exchange path %s", r.URL.Path)
		}
		fmt.Fprintf(w, `{"Result":{"Token":"rotated-access","RefreshToken":"rotated-refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}}`, time.Now().Add(time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix())
	}))
	defer up.Close()
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	old := syntheticCredential(t, "old-access")
	old.ExpiresAt = time.Now().Add(time.Minute)
	if err := store.Save(old); err != nil {
		t.Fatal(err)
	}
	store.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	reserved.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = addr
	cfg.AuthBaseURL = up.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runLogin(ctx, cfg, &bytes.Buffer{}, func(context.Context, string) error { return fmt.Errorf("unexpected browser") }, time.Millisecond, time.Second)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := readAPICredential()
		if err == nil && c.Token == "rotated-access" {
			break
		}
		select {
		case e := <-done:
			t.Fatalf("foreground Auth exited before refresh: %v", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("foreground refresh not published: %v requests=%d", err, requests.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if requests.Load() != 1 {
		t.Fatalf("refresh replayed: %d", requests.Load())
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground Auth did not stop")
	}
}

func TestTRAE009LoginWaitsForVerifiedAuthStartup(t *testing.T) {
	isolatedCredentialPath(t)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	reserved.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = addr
	a, err := newAuthService(cfg, addr)
	if err != nil {
		t.Fatal(err)
	}
	c := syntheticCredential(t, "saved-access")
	if err := a.store.Save(c); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.cred = c
	a.manager.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loginDone := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		loginDone <- runLogin(ctx, cfg, &out, func(context.Context, string) error { return fmt.Errorf("unexpected browser") }, time.Millisecond, time.Second)
	}()
	select {
	case err := <-loginDone:
		a.shutdown()
		t.Fatalf("login rejected starting Auth: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- a.Run(ctx) }()
	select {
	case err := <-loginDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("login did not reuse started Auth")
	}
	if !strings.Contains(out.String(), "Already logged in.") {
		t.Fatalf("reuse output: %s", out.String())
	}
	cancel()
	if err := <-serviceDone; err != nil {
		t.Fatal(err)
	}
}
