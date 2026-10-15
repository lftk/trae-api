package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newIsolatedAuth(t *testing.T) *authService {
	t.Helper()
	isolatedCredentialPath(t)
	a, err := newAuthService(fixtureConfig(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.shutdown() })
	return a
}

func authRequest(a *authService, method, path, host, peer string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.RemoteAddr = peer
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func TestStandaloneAuthRoutesAndLocalBoundary(t *testing.T) {
	a := newIsolatedAuth(t)
	listener, err := a.listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host := listener.Addr().String()
	if w := authRequest(a, "GET", "/healthz", host, "127.0.0.1:1234", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := authRequest(a, "POST", "/api/auth/login", host, "127.0.0.1:1234", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		method string
		path   string
		host   string
		peer   string
	}{
		{
			method: "GET",
			path:   "/authorize?loginTraceID=missing",
			host:   "evil.test",
			peer:   "127.0.0.1:1234",
		},
		{
			method: "GET",
			path:   "/authorize?loginTraceID=missing",
			host:   host,
			peer:   "203.0.113.2:1234",
		},
		{
			method: "POST",
			path:   "/authorize",
			host:   host,
			peer:   "127.0.0.1:1234",
		},
		{
			method: "GET",
			path:   "/authorize/",
			host:   host,
			peer:   "127.0.0.1:1234",
		},
		{
			method: "POST",
			path:   "/api/auth/login",
			host:   "evil.test",
			peer:   "127.0.0.1:1234",
		},
		{
			method: "POST",
			path:   "/api/auth/login",
			host:   host,
			peer:   "203.0.113.2:1234",
		},
	} {
		w := authRequest(a, tc.method, tc.path, tc.host, tc.peer, `{}`)
		if w.Code == 200 {
			t.Errorf("accepted %s %s host=%s peer=%s", tc.method, tc.path, tc.host, tc.peer)
		}
	}
	r := httptest.NewRequest("POST", "http://"+host+"/api/auth/login", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross origin: %d", w.Code)
	}
}

func TestStandaloneAuthLockAndRecovery(t *testing.T) {
	path := isolatedCredentialPath(t)
	a, err := newAuthService(fixtureConfig(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newAuthService(fixtureConfig(), "127.0.0.1:0"); err == nil {
		t.Fatal("duplicate writer")
	}
	c := syntheticCredential(t, "saved")
	if err = a.store.Save(c); err != nil {
		t.Fatal(err)
	}
	a.shutdown()
	b, err := newAuthService(fixtureConfig(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()
	got, _ := b.manager.snapshot()
	if got.Token != c.Token {
		t.Fatal("state not recovered")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneAuthCallbackPersistsBeforePublishAndRejectsReplay(t *testing.T) {
	a := newIsolatedAuth(t)
	l, err := a.listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	host := l.Addr().String()
	start := authRequest(a, "POST", "/api/auth/login", host, "127.0.0.1:42", `{}`)
	var result struct {
		URL string `json:"url"`
		ID  string `json:"attempt_id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &result); err != nil || result.ID == "" {
		t.Fatalf("start: %s %v", start.Body.String(), err)
	}
	parsed, _ := url.Parse(result.URL)
	if parsed.Query().Get("auth_callback_url") != "http://"+host+"/authorize" {
		t.Fatal("callback not derived from bound listener")
	}
	expiry := time.Now().Add(time.Hour).Unix()
	refreshExpiry := time.Now().Add(24 * time.Hour).Unix()
	q := url.Values{"loginTraceID": {result.ID}, "userJwt": {fmt.Sprintf(`{"Token":"callback-token","RefreshToken":"refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}`, expiry, refreshExpiry)}, "userInfo": {`{"UserID":"user","Region":"CN"}`}}
	cb := "/authorize?" + q.Encode()
	w := authRequest(a, "GET", cb, host, "127.0.0.1:42", "")
	if w.Code != 200 {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	saved, err := readAPICredential()
	if err != nil || saved.Token != "callback-token" {
		t.Fatalf("not persisted: %+v %v", saved, err)
	}
	if w = authRequest(a, "GET", cb, host, "127.0.0.1:42", ""); w.Code == 200 {
		t.Fatal("replay accepted")
	}
}

func TestStandaloneAuthRunShutdown(t *testing.T) {
	a := newIsolatedAuth(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		a.manager.mu.Lock()
		ready := a.manager.callbackURL != ""
		a.manager.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("listener did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
}

func TestStandaloneAuthRunPropagatesStoreCloseFailure(t *testing.T) {
	a := newIsolatedAuth(t)
	// Force the lock release to fail without touching any real credential path.
	if err := a.store.lock.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "credential store") {
		t.Fatalf("store close error lost on Run shutdown: %v", err)
	}
}

func TestStandaloneAuthRefreshPersists(t *testing.T) {
	isolatedCredentialPath(t)
	cfg := fixtureConfig()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceExchangePath {
			t.Errorf("exchange path %s", r.URL.Path)
		}
		fmt.Fprintf(w, `{"Result":{"Token":"rotated","RefreshToken":"new-refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}}`, time.Now().Add(time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix())
	}))
	defer up.Close()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := syntheticCredential(t, "old")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	if err = a.store.Save(c); err != nil {
		t.Fatal(err)
	}
	a.shutdown()
	b, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()
	b.refreshIfNeeded(context.Background())
	memory, _ := b.manager.snapshot()
	disk, err := b.store.Load()
	if err != nil || memory.Token != "rotated" || disk.Token != "rotated" || disk.RefreshToken != "new-refresh" {
		t.Fatalf("memory=%q disk=%q err=%v", memory.Token, disk.Token, err)
	}
}

func TestStandaloneAuthLateCallbackCannotReplaceNewAttempt(t *testing.T) {
	a := newIsolatedAuth(t)
	l, err := a.listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	host := l.Addr().String()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		fmt.Fprintf(w, `{"Result":{"Token":"late","UserID":"user","TokenExpireAt":%d,"RefreshExpireAt":%d}}`, time.Now().Add(time.Hour).Unix(), time.Now().Add(24*time.Hour).Unix())
	}))
	defer up.Close()
	a.manager.cfg.AuthBaseURL = up.URL
	first := authRequest(a, "POST", "/api/auth/login", host, "127.0.0.1:42", `{}`)
	var result struct {
		ID string `json:"attempt_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"loginTraceID": {result.ID}, "refreshToken": {"synthetic"}}
	done := make(chan int, 1)
	go func() { done <- authRequest(a, "GET", "/authorize?"+q.Encode(), host, "127.0.0.1:42", "").Code }()
	<-started
	second := authRequest(a, "POST", "/api/auth/login", host, "127.0.0.1:42", `{}`)
	if second.Code != 200 {
		t.Fatal(second.Code)
	}
	close(release)
	if code := <-done; code == 200 {
		t.Fatal("late callback committed")
	}
	if _, err := readAPICredential(); !errors.Is(err, errLoginRequired) {
		t.Fatalf("late credential on disk: %v", err)
	}
}

func TestStandaloneAuthRejectsNonLoopbackBeforeOpeningStore(t *testing.T) {
	isolatedCredentialPath(t)
	for _, addr := range []string{"0.0.0.0:0", "203.0.113.2:0"} {
		if _, err := newAuthService(fixtureConfig(), addr); err == nil {
			t.Fatalf("accepted %s", addr)
		}
	}
}

func TestStandaloneAuthStaleRefreshCannotOverwriteNewAccount(t *testing.T) {
	isolatedCredentialPath(t)
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprintf(w, `{"Result":{"Token":"stale","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.shutdown()
	old := syntheticCredential(t, "old")
	old.ExpiresAt = time.Now().Add(-time.Second)
	if err := a.store.Save(old); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.cred = old
	a.manager.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); _, _, _ = a.manager.get(context.Background()) }()
	<-started
	newAccount := syntheticCredential(t, "new-account")
	newAccount.UserID = "different-user"
	a.manager.mu.Lock()
	if err := a.store.Save(newAccount); err != nil {
		a.manager.mu.Unlock()
		t.Fatal(err)
	}
	a.manager.cred = newAccount
	a.manager.generation++
	a.manager.mu.Unlock()
	close(release)
	<-done
	c, err := a.store.Load()
	if err != nil || c.Token != "new-account" {
		t.Fatalf("stale refresh replaced disk: %q %v", c.Token, err)
	}
	memory, _ := a.manager.snapshot()
	if memory.Token != "new-account" {
		t.Fatal("stale refresh replaced memory")
	}
}

func TestStandaloneAuthRejectedRefreshInvalidatesSavedAccess(t *testing.T) {
	isolatedCredentialPath(t)
	cfg := fixtureConfig()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer up.Close()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := syntheticCredential(t, "rejected")
	c.ExpiresAt = time.Now().Add(time.Minute)
	if err := a.store.Save(c); err != nil {
		t.Fatal(err)
	}
	a.shutdown()
	b, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()
	b.refreshIfNeeded(context.Background())
	if _, err := readAPICredential(); !errors.Is(err, errLoginRequired) {
		t.Fatalf("rejected access remains valid: %v", err)
	}
}

func TestStandaloneAuthIncompleteExchangePreservesUsableSnapshot(t *testing.T) {
	isolatedCredentialPath(t)
	cfg := fixtureConfig()
	requests := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Result":{"RefreshToken":"replacement-but-no-access-token"}}`)
	}))
	defer up.Close()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.shutdown()
	old := syntheticCredential(t, "still-valid")
	old.ExpiresAt = time.Now().Add(time.Minute)
	if err := a.store.Save(old); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.cred = old
	a.manager.mu.Unlock()
	got, _, err := a.manager.get(context.Background())
	if err != nil || got.Token != old.Token || requests != 1 {
		t.Fatalf("proactive exchange: token=%q requests=%d err=%v", got.Token, requests, err)
	}
	memory, _ := a.manager.snapshot()
	disk, err := a.store.Load()
	if err != nil || memory.Token != old.Token || disk.Token != old.Token || disk.RefreshToken != old.RefreshToken || !disk.ExpiresAt.Equal(old.ExpiresAt) {
		t.Fatalf("snapshot changed: memory=%q disk=%q err=%v", memory.Token, disk.Token, err)
	}
	if api, err := readAPICredential(); err != nil || api.Token != old.Token {
		t.Fatalf("API lost usable snapshot: token=%q err=%v", api.Token, err)
	}
}

func TestStandaloneAuthAmbiguousExchangeFenceSurvivesRestart(t *testing.T) {
	isolatedCredentialPath(t)
	requests := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprint(w, `{"Result":`) // Incomplete HTTP 200 may have consumed refresh token.
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	old := syntheticCredential(t, "still-valid")
	old.ExpiresAt = time.Now().Add(time.Minute)
	if err := a.store.Save(old); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.cred = old
	a.manager.mu.Unlock()
	got, _, err := a.manager.get(context.Background())
	if err != nil || got.Token != old.Token || requests != 1 {
		t.Fatalf("first exchange: %q %d %v", got.Token, requests, err)
	}
	disk, err := a.store.Load()
	if err != nil || !disk.RefreshPending {
		t.Fatalf("missing durable refresh fence: %v %+v", err, disk)
	}
	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	b, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()
	b.refreshIfNeeded(context.Background())
	if requests != 1 {
		t.Fatalf("potentially spent refresh replayed after restart: %d", requests)
	}
	if api, err := readAPICredential(); err != nil || api.Token != old.Token {
		t.Fatalf("valid access lost: %q %v", api.Token, err)
	}
}

func TestTRAE008RefreshWriteFaultsReconcileSnapshot(t *testing.T) {
	for _, stage := range []string{"before-pending", "before-publish", "after-publish"} {
		t.Run(stage, func(t *testing.T) {
			isolatedCredentialPath(t)
			requests := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				fmt.Fprintf(w, `{"Result":{"Token":"fresh-access","RefreshToken":"fresh-refresh","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, err := newAuthService(cfg, "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			old := syntheticCredential(t, "old-access")
			old.ExpiresAt = time.Now().Add(time.Minute)
			if err := a.store.Save(old); err != nil {
				t.Fatal(err)
			}
			a.manager.mu.Lock()
			a.manager.cred = old
			a.manager.mu.Unlock()
			if stage != "after-publish" {
				createCount := 0
				a.store.createTemp = func(dir, pattern string) (*os.File, error) {
					createCount++
					if stage == "before-pending" || createCount == 2 {
						return nil, errors.New("injected pre-replace failure")
					}
					return os.CreateTemp(dir, pattern)
				}
			} else {
				syncCount := 0
				a.store.syncDir = func(d *os.File) error {
					syncCount++
					if syncCount == 2 {
						return errors.New("injected post-rename directory sync failure")
					}
					return d.Sync()
				}
			}
			got, _, refreshErr := a.manager.get(context.Background())
			disk, err := a.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			memory, _ := a.manager.snapshot()
			if stage == "after-publish" {
				if refreshErr == nil || !strings.Contains(refreshErr.Error(), "injected post-rename") || got.Token != "fresh-access" || disk.Token != got.Token || memory.Token != got.Token || disk.RefreshPending || requests != 1 {
					t.Fatalf("published refresh diverged: return=%q disk=%q memory=%q pending=%t requests=%d err=%v", got.Token, disk.Token, memory.Token, disk.RefreshPending, requests, refreshErr)
				}
			} else if disk.Token != old.Token || memory.Token != old.Token || got.Token != old.Token || refreshErr != nil || requests != map[string]int{"before-pending": 0, "before-publish": 1}[stage] || disk.RefreshPending != (stage == "before-publish") {
				t.Fatalf("pre-replace changed state: return=%q disk=%q memory=%q pending=%t requests=%d err=%v", got.Token, disk.Token, memory.Token, disk.RefreshPending, requests, refreshErr)
			}
			if err := a.shutdown(); err != nil {
				t.Fatal(err)
			}
			b, err := newAuthService(cfg, "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer b.shutdown()
			restored, _ := b.manager.snapshot()
			if restored.Token != disk.Token {
				t.Fatalf("restart diverged: %q vs %q", restored.Token, disk.Token)
			}
			if stage != "before-pending" {
				b.refreshIfNeeded(context.Background())
				if requests != 1 {
					t.Fatalf("replayed refresh after restart: %d", requests)
				}
			}
		})
	}
}

func TestTRAE008CallbackPublishedDespiteDirectorySyncError(t *testing.T) {
	a := newIsolatedAuth(t)
	l, err := a.listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	host := l.Addr().String()
	start := authRequest(a, "POST", "/api/auth/login", host, "127.0.0.1:42", `{}`)
	var result struct {
		URL string `json:"url"`
		ID  string `json:"attempt_id"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &result); err != nil || result.ID == "" {
		t.Fatalf("start: %s %v", start.Body.String(), err)
	}
	a.store.syncDir = func(*os.File) error { return errors.New("injected callback directory sync failure") }
	expiry := time.Now().Add(time.Hour).Unix()
	refreshExpiry := time.Now().Add(24 * time.Hour).Unix()
	q := url.Values{
		"loginTraceID": {result.ID},
		"userJwt":      {fmt.Sprintf(`{"Token":"callback-token","RefreshToken":"refresh","TokenExpireAt":%d,"RefreshExpireAt":%d}`, expiry, refreshExpiry)},
		"userInfo":     {`{"UserID":"user","Region":"CN"}`},
	}
	cb := "/authorize?" + q.Encode()
	w := authRequest(a, "GET", cb, host, "127.0.0.1:42", "")
	memory, _ := a.manager.snapshot()
	disk, err := a.store.Load()
	state := a.manager.loginResults[result.ID].Status
	if w.Code != http.StatusServiceUnavailable || memory.Token != "callback-token" || err != nil || disk.Token != memory.Token || state != "durability_uncertain" {
		t.Fatalf("callback divergence: status=%d state=%q memory=%q disk=%q err=%v", w.Code, state, memory.Token, disk.Token, err)
	}
	if replay := authRequest(a, "GET", cb, host, "127.0.0.1:42", ""); replay.Code == http.StatusOK {
		t.Fatal("replayed uncertain callback")
	}
	retry := authRequest(a, "POST", "/api/auth/login", host, "127.0.0.1:42", `{}`)
	var retryResult struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &retryResult); err != nil || retryResult.Status != "pending" {
		t.Fatalf("uncertain durability blocked re-login: %s %v", retry.Body.String(), err)
	}
	if err := a.shutdown(); err != nil {
		t.Fatal(err)
	}
	b, err := newAuthService(fixtureConfig(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.shutdown()
	restored, _ := b.manager.snapshot()
	if restored.Token != memory.Token {
		t.Fatalf("callback not restored after restart: %q", restored.Token)
	}
}

func TestTRAE008BackgroundRefreshReportsUncertainDurability(t *testing.T) {
	isolatedCredentialPath(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Result":{"Token":"fresh-access","RefreshToken":"fresh-refresh","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.shutdown()
	old := syntheticCredential(t, "old-access")
	old.ExpiresAt = time.Now().Add(time.Minute)
	if err := a.store.Save(old); err != nil {
		t.Fatal(err)
	}
	a.manager.mu.Lock()
	a.manager.cred = old
	a.manager.mu.Unlock()
	syncCount := 0
	a.store.syncDir = func(d *os.File) error {
		syncCount++
		if syncCount == 2 {
			return errors.New("injected post-rename sync failure")
		}
		return d.Sync()
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	a.refreshIfNeeded(context.Background())
	if !strings.Contains(logs.String(), "durability unconfirmed") || strings.Contains(logs.String(), "fresh-refresh") || strings.Contains(logs.String(), "fresh-access") {
		t.Fatalf("background persistence failure not surfaced safely: %s", logs.String())
	}
}
