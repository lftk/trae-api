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

func qa009Proof(w http.ResponseWriter, r *http.Request, key []byte) {
	challenge, err := hex.DecodeString(r.Header.Get("X-Trae-Auth-Challenge"))
	if err != nil || len(challenge) != 32 {
		return
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(challenge)
	w.Header().Set("X-Trae-Auth-Proof", hex.EncodeToString(mac.Sum(nil)))
}

func qa009LoginLink(t *testing.T, cfg config, id string) string {
	t.Helper()
	u, err := url.Parse(cfg.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("login_trace_id", id)
	u.RawQuery = query.Encode()
	return u.String()
}

type qa009WriterFunc func([]byte) (int, error)

func (f qa009WriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestQA009RedirectNeverCarriesLoginRequestToAnotherListener(t *testing.T) {
	isolatedCredentialPath(t)
	key := []byte(strings.Repeat("k", 32))
	id := strings.Repeat("a", 32)
	cfg := fixtureConfig()
	link := qa009LoginLink(t, cfg, id)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	var statuses atomic.Int32
	var cancels atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qa009Proof(w, r, key)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/login":
			fmt.Fprintf(w, `{"status":"pending","attempt_id":%q,"url":%q}`, id, link)
		case "/api/auth/login/status":
			statuses.Add(1)
			w.Header().Set("Location", target.URL+"/stolen-status")
			w.WriteHeader(http.StatusFound)
		case "/api/auth/login/cancel":
			cancels.Add(1)
			w.Header().Set("Location", target.URL+"/stolen-cancel")
			w.WriteHeader(http.StatusFound)
		default:
			t.Errorf("unexpected management route %s", r.URL.Path)
		}
	}))
	defer first.Close()
	cfg.AuthAddr = strings.TrimPrefix(first.URL, "http://")
	var output bytes.Buffer
	opened := 0
	err := runLoginFlow(context.Background(), cfg, &output, func(context.Context, string) error {
		opened++
		return nil
	}, time.Millisecond, time.Second, key)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || opened != 1 || statuses.Load() != 1 || cancels.Load() != 1 || redirected.Load() != 0 {
		t.Fatalf("redirected management request: error=%v open=%d status=%d cancel=%d redirected=%d", err, opened, statuses.Load(), cancels.Load(), redirected.Load())
	}
}

func TestQA009ReplacedListenerAtStatusCannotContinueLogin(t *testing.T) {
	isolatedCredentialPath(t)
	key := []byte(strings.Repeat("q", 32))
	id := strings.Repeat("b", 32)
	cfg := fixtureConfig()
	link := qa009LoginLink(t, cfg, id)
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qa009Proof(w, r, key)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/auth/login" {
			t.Errorf("old listener received %s after replacement", r.URL.Path)
			return
		}
		fmt.Fprintf(w, `{"status":"pending","attempt_id":%q,"url":%q}`, id, link)
	}))
	addr := source.Listener.Addr().String()
	source.Start()
	defer source.Close()
	cfg.AuthAddr = addr
	var fakeCalls atomic.Int32
	var fake *httptest.Server
	var output bytes.Buffer
	opened := 0
	err := runLoginFlow(context.Background(), cfg, &output, func(context.Context, string) error {
		opened++
		source.Close()
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		fake = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fakeCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"completed"}`)
			// Deliberately no HMAC proof: a replacement cannot impersonate Auth.
		}))
		fake.Listener = listener
		fake.Start()
		return nil
	}, time.Millisecond, time.Second, key)
	if fake != nil {
		defer fake.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "unverified Auth response") || opened != 1 || fakeCalls.Load() == 0 || strings.Contains(output.String(), "Login successful") {
		t.Fatalf("replacement accepted at status: error=%v open=%d fake_calls=%d success=%t", err, opened, fakeCalls.Load(), strings.Contains(output.String(), "Login successful"))
	}
}

func TestQA009SelfStartedAuthRenewsBeforeForegroundExit(t *testing.T) {
	isolatedCredentialPath(t)
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		if r.URL.Path != deviceExchangePath {
			t.Errorf("unexpected synthetic refresh route %s", r.URL.Path)
		}
		fmt.Fprintf(w, `{"Result":{"Token":"synthetic-new-access","RefreshToken":"synthetic-new-refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}}`, time.Now().Add(time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix())
	}))
	defer upstream.Close()
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	old := syntheticCredential(t, "synthetic-old-access")
	old.ExpiresAt = time.Now().Add(time.Minute)
	if err := store.Save(old); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	reserved.Close()
	cfg := fixtureConfig()
	cfg.AuthAddr = addr
	cfg.AuthBaseURL = upstream.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	ready := make(chan struct{}, 1)
	output := qa009WriterFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("Auth remains running")) {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
		return len(p), nil
	})
	go func() {
		result <- runLogin(ctx, cfg, output, func(context.Context, string) error {
			return fmt.Errorf("unexpected browser")
		}, time.Millisecond, 2*time.Second)
	}()
	deadline := time.Now().Add(4 * time.Second)
	for {
		current, err := readAPICredential()
		if err == nil && current.Token == "synthetic-new-access" {
			break
		}
		select {
		case stopped := <-result:
			t.Fatalf("Auth exited before refresh publication: %v", stopped)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no published synthetic refresh; exchanges=%d error=%v", exchanges.Load(), err)
		}
		time.Sleep(time.Millisecond)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("refresh exchange repeated: count=%d", exchanges.Load())
	}
	select {
	case <-ready:
	case stopped := <-result:
		t.Fatalf("foreground Auth exited after refresh: %v", stopped)
	case <-time.After(3 * time.Second):
		t.Fatal("foreground Auth never announced renewal service")
	}
	select {
	case stopped := <-result:
		t.Fatalf("foreground Auth stopped before cancellation: %v", stopped)
	default:
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground Auth did not stop")
	}
	newWriter, err := openCredentialStore()
	if err != nil {
		t.Fatalf("foreground Auth retained writer lock: %v", err)
	}
	newWriter.Close()
}

func TestQA009ReplacedListenerAtCancelDoesNotFollowRedirect(t *testing.T) {
	isolatedCredentialPath(t)
	key := []byte(strings.Repeat("c", 32))
	id := strings.Repeat("d", 32)
	cfg := fixtureConfig()
	link := qa009LoginLink(t, cfg, id)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qa009Proof(w, r, key)
		if r.URL.Path != "/api/auth/login" {
			t.Errorf("unexpected request before cancel replacement: %s", r.URL.Path)
			return
		}
		fmt.Fprintf(w, `{"status":"pending","attempt_id":%q,"url":%q}`, id, link)
	}))
	addr := source.Listener.Addr().String()
	source.Start()
	defer source.Close()
	cfg.AuthAddr = addr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fake *httptest.Server
	var cancels atomic.Int32
	var output bytes.Buffer
	err := runLoginFlow(ctx, cfg, &output, func(context.Context, string) error {
		source.Close()
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		fake = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/login/cancel" {
				cancels.Add(1)
			}
			w.Header().Set("Location", target.URL+"/redirected-cancel")
			w.WriteHeader(http.StatusFound)
		}))
		fake.Listener = listener
		fake.Start()
		cancel()
		return nil
	}, time.Millisecond, time.Second, key)
	if fake != nil {
		defer fake.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "cancelled") || cancels.Load() != 1 || redirected.Load() != 0 || strings.Contains(output.String(), "Login successful") {
		t.Fatalf("cancel replacement bypassed protection: error=%v fake_cancel=%d redirected=%d success=%t", err, cancels.Load(), redirected.Load(), strings.Contains(output.String(), "Login successful"))
	}
}
