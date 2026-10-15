package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountSlotsQueueCancellationAndRelease(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	var active, max atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, textFrames("ok"))
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	cfg.MaxConcurrent = 1
	cfg.QueueTimeout = 20 * time.Millisecond
	s := authenticatedServer(t, cfg)
	request := chatRequest{
		Model: "m",
		Messages: []message{{
			Role:    "user",
			Content: "hi",
		}},
	}
	first := make(chan error, 1)
	go func() {
		_, _, e := s.execute(context.Background(), request, nil)
		first <- e
	}()
	<-started
	_, _, e := s.execute(context.Background(), request, nil)
	if statusForError(e) != 503 || max.Load() != 1 {
		t.Fatalf("queue not limited: %v max=%d", e, max.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, e = s.execute(ctx, request, nil)
	if e != context.Canceled {
		t.Fatal(e)
	}
	release <- struct{}{}
	if e = <-first; e != nil {
		t.Fatal(e)
	}
	next := make(chan error, 1)
	go func() {
		_, _, e := s.execute(context.Background(), request, nil)
		next <- e
	}()
	<-started
	release <- struct{}{}
	if e = <-next; e != nil {
		t.Fatal(e)
	}
	if len(s.slots) != 0 {
		t.Fatal("slot leaked")
	}
}
func TestCancellationStopsUpstreamAndReleasesSlot(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, frame("output", map[string]string{"response": "partial"}))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, e := s.execute(ctx, chatRequest{
			Model: "m",
			Messages: []message{{
				Role:    "user",
				Content: "hi",
			}},
		}, nil)
		done <- e
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not cancel")
	}
	if len(s.slots) != 0 {
		t.Fatal("slot leaked")
	}
}

type qaRetestTransport func(*http.Request) (*http.Response, error)

func (f qaRetestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Switch B after all generation checks, at retry transport entry, before
// the underlying transport sends the actual request to the local upstream.
func TestQARetestSwitchImmediatelyBeforeRetrySend(t *testing.T) {
	for _, which := range []string{"prompt", "models"} {
		for _, code := range []int{200, 401, 403} {
			t.Run(fmt.Sprintf("%s/%d", which, code), func(t *testing.T) {
				wireTokens := make(chan string, 4)
				calls := 0
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "ExchangeToken") {
						fmt.Fprintf(w, `{"Result":{"Token":"refreshed-A","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
						return
					}
					wireTokens <- r.Header.Get("X-Cloudide-Token")
					calls++
					if calls == 1 {
						w.WriteHeader(401)
						return
					}
					if code != 200 {
						w.WriteHeader(code)
						return
					}
					if which == "models" {
						fmt.Fprint(w, `{"function_configs":[{"function":"chat_v3","config_info_list":[{"config_name":"A-model"}]}]}`)
					} else {
						fmt.Fprint(w, textFrames("A-response"))
					}
				}))
				defer up.Close()
				cfg := fixtureConfig()
				cfg.BaseURL = up.URL
				cfg.AuthBaseURL = up.URL
				s := authenticatedServer(t, cfg)
				s.auth.cred.RefreshToken = "synthetic-refresh"
				s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
				originalTransport := s.upstream.http.Transport
				defer originalTransport.(*http.Transport).CloseIdleConnections()
				transportCalls := 0
				s.upstream.http.Transport = qaRetestTransport(func(r *http.Request) (*http.Response, error) {
					transportCalls++
					if transportCalls == 2 {
						if r.Header.Get("X-Cloudide-Token") != "refreshed-A" {
							t.Errorf("retry credential unexpectedly changed before hook: %q", r.Header.Get("X-Cloudide-Token"))
						}
						link, _ := s.auth.beginLogin()
						u, _ := url.Parse(link)
						q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"account-B"}`}, "userInfo": {`{"UserID":"user-B"}`}}
						w := httptest.NewRecorder()
						s.auth.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
						if w.Code != 200 {
							t.Errorf("B login failed: %d", w.Code)
						}
					}
					return originalTransport.RoundTrip(r)
				})
				c, g := s.auth.snapshot()
				var err error
				if which == "models" {
					_, err = s.upstream.modelNames(context.Background(), c, g)
				} else {
					_, _, err = s.execute(context.Background(), chatRequest{
						Model: "m",
						Messages: []message{{
							Role:    "user",
							Content: "synthetic A history",
						}},
					}, nil)
				}
				close(wireTokens)
				var got []string
				for token := range wireTokens {
					got = append(got, token)
					if token == "account-B" {
						t.Error("A request sent B credential")
					}
				}
				b, _, bErr := s.auth.get(context.Background())
				if bErr != nil || b.Token != "account-B" || b.UserID != "user-B" {
					t.Fatalf("old retry invalidated B: credential=%+v err=%v", b, bErr)
				}
				if transportCalls != 2 || calls != 2 || len(got) != 2 || got[1] != "refreshed-A" {
					t.Fatalf("hook did not exercise retry boundary: wire=%v calls=%d transport=%d", got, calls, transportCalls)
				}
				if code != 200 && err != errLoginRequired {
					t.Fatalf("rejection result=%v", err)
				}
				t.Logf("retry-send switch: wire=%v, error=%v, B remains usable", got, err)
			})
		}
	}
}

func TestQARetestTemporaryRefreshFailureRecovers(t *testing.T) {
	for _, code := range []int{500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.WriteHeader(code)
					return
				}
				fmt.Fprintf(w, `{"Result":{"Token":"recovered-A","UserID":"account","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			defer a.client.CloseIdleConnections()
			a.cred = credential{
				Token:        "expired-A",
				UserID:       "account",
				RefreshToken: "synthetic-refresh",
				ExpiresAt:    time.Now().Add(-time.Second),
			}
			_, _, first := a.get(context.Background())
			if first == nil || first == errLoginRequired {
				t.Fatalf("5xx not temporary: %v", first)
			}
			c, _, err := a.get(context.Background())
			if err != nil || c.Token != "recovered-A" || calls != 2 {
				t.Fatalf("recovery failed: calls=%d credential=%+v err=%v", calls, c, err)
			}
		})
	}
}

func TestTRAE003RefreshRejectionIsStickyBut5xxIsNot(t *testing.T) {
	for _, code := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(code)
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			a.cred = credential{
				Token:        "synthetic",
				RefreshToken: "synthetic-refresh",
				ExpiresAt:    time.Now().Add(-time.Second),
			}
			_, _, first := a.get(context.Background())
			_, _, second := a.get(context.Background())
			if code == 500 {
				if first == errLoginRequired || second == errLoginRequired || calls != 2 {
					t.Fatalf("temporary failure poisoned credential: %v %v calls=%d", first, second, calls)
				}
			} else if first != errLoginRequired || second != errLoginRequired || calls != 1 {
				t.Fatalf("auth rejection retried: %v %v calls=%d", first, second, calls)
			}
		})
	}
}

func TestTRAE003RefreshIdentityFormats(t *testing.T) {
	for _, identity := range []string{`"UserID":"other"`, `"UserInfo":{"UserID":"other"}`, `"UserID":"other","UserInfo":{"UserID":"account"}`, `"UserID":"account","UserInfo":{"UserID":"other"}`} {
		t.Run(identity, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"Result":{"Token":"synthetic",%s}}`, identity)
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			_, err := a.exchange(context.Background(), credential{
				UserID:       "account",
				RefreshToken: "synthetic",
			})
			if err != errLoginRequired {
				t.Fatalf("identity mismatch accepted: %v", err)
			}
		})
	}
}

func TestTRAE003CallbackAllowsOfficialTopLevelRedirect(t *testing.T) {
	cfg := fixtureConfig()
	cfg.AuthAddr = "127.0.0.1:0"
	s, _ := newServer(cfg)
	defer s.shutdown()
	listener, err := s.startAuthListener()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	link, err := s.auth.beginLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"synthetic"}`}, "userInfo": {`{"UserID":"fixture"}`}}
	req, _ := http.NewRequest("GET", s.auth.callbackURL+"?"+q.Encode(), nil)
	req.Header.Set("Origin", u.Scheme+"://"+u.Host)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("official redirect rejected: %d", resp.StatusCode)
	}
	c, _ := s.auth.snapshot()
	if c.Token != "synthetic" {
		t.Fatal("redirect did not log in")
	}
}

func TestQA003ActualAPIRouteProtection(t *testing.T) {
	routes := []struct {
		method string
		path   string
		status int
	}{{
		"GET",
		"/healthz",
		200,
	}, {
		"GET",
		"/v1/models",
		401,
	}, {
		"POST",
		"/v1/chat/completions",
		400,
	}, {
		"POST",
		"/v1/responses",
		501,
	}, {
		"POST",
		"/api/auth/login",
		200,
	}, {
		"POST",
		"/api/auth/login/status",
		404,
	}, {
		"POST",
		"/api/auth/login/cancel",
		404,
	}}
	for _, token := range []string{"", "synthetic-gateway"} {
		cfg := fixtureConfig()
		cfg.APIToken = token
		s, err := newServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer s.shutdown()
		s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
		for _, route := range routes {
			for _, kind := range []string{"local", "evil-host", "evil-origin", "cross-site", "wrong-token"} {
				t.Run(token+route.path+kind, func(t *testing.T) {
					r := httptest.NewRequest(route.method, "http://127.0.0.1:8723"+route.path, strings.NewReader(`{}`))
					want := route.status
					if token != "" {
						r.Header.Set("Authorization", "Bearer "+token)
					}
					switch kind {
					case "evil-host":
						r.Host = "attacker.invalid:8723"
						if token == "" {
							want = 403
						}
					case "evil-origin":
						r.Header.Set("Origin", "https://attacker.invalid")
						if token == "" || strings.HasPrefix(route.path, "/api/auth/") {
							want = 403
						}
					case "cross-site":
						r.Header.Set("Sec-Fetch-Site", "cross-site")
						if token == "" || strings.HasPrefix(route.path, "/api/auth/") {
							want = 403
						}
					case "wrong-token":
						r.Header.Set("Authorization", "Bearer wrong")
						if token != "" {
							want = 401
						}
					}
					w := httptest.NewRecorder()
					s.routes().ServeHTTP(w, r)
					if w.Code != want {
						t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
					}
				})
			}
		}
	}
}

type qaDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *qaDeadlineWriter) SetWriteDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}
func TestQA003EachSSEWriteRenewsDeadline(t *testing.T) {
	w := &qaDeadlineWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	s := newStreamSink(w, httptest.NewRequest("GET", "http://127.0.0.1/", nil))
	defer s.close()
	for i, f := range []func() error{func() error { return s.send("", map[string]string{"x": "start"}) }, s.heartbeat, s.done} {
		start := time.Now()
		if err := f(); err != nil {
			t.Fatal(err)
		}
		if len(w.deadlines) != i+1 || w.deadlines[i].Before(start.Add(29*time.Second)) {
			t.Fatalf("write %d did not renew deadline: %v", i, w.deadlines)
		}
	}
	before := w.Body.String()
	_ = s.heartbeat()
	if err := s.send("", nil); err == nil {
		t.Fatal("write after DONE accepted")
	}
	if w.Body.String() != before {
		t.Fatal("post terminal data written")
	}
}

func TestQA003CurrentRejectedTokenRequiresLogin(t *testing.T) {
	for _, which := range []string{"prompt", "models"} {
		t.Run(which, func(t *testing.T) {
			calls, refresh := 0, 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "ExchangeToken") {
					refresh++
					fmt.Fprintf(w, `{"Result":{"Token":"retry-token","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
					return
				}
				calls++
				w.WriteHeader(401)
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL = up.URL
			cfg.AuthBaseURL = up.URL
			s := authenticatedServer(t, cfg)
			s.auth.cred.RefreshToken = "synthetic-refresh"
			c, g := s.auth.snapshot()
			var err error
			if which == "models" {
				_, err = s.upstream.modelNames(context.Background(), c, g)
			} else {
				_, err = s.upstream.prompt(context.Background(), c, g, chatRequest{
					Model: "m",
					Messages: []message{{
						Role:    "user",
						Content: "hi",
					}},
				}, "qa", nil)
			}
			if err != errLoginRequired || calls != 2 || refresh != 1 {
				t.Fatalf("err=%v calls=%d refresh=%d", err, calls, refresh)
			}
			if _, _, err = s.auth.get(context.Background()); err != errLoginRequired {
				t.Fatalf("current rejected token still usable: %v", err)
			}
		})
	}
}

func TestQA003LostStartResponseBoundedAndReplaceable(t *testing.T) {
	s, _ := newServer(fixtureConfig())
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	var orphan string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			link, err := s.auth.beginLogin()
			if err != nil {
				t.Error(err)
			}
			u, _ := url.Parse(link)
			orphan = u.Query().Get("login_trace_id")
			fmt.Fprint(w, `{broken`)
			return
		}
		s.routes().ServeHTTP(w, r)
	}))
	defer api.Close()
	u, _ := url.Parse(api.URL)
	cfg := fixtureConfig()
	cfg.Addr = u.Host
	err := runLogin(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error {
		t.Error("browser opened despite lost response")
		return nil
	}, time.Millisecond, time.Second)
	if err == nil || orphan == "" {
		t.Fatalf("err=%v orphan=%q", err, orphan)
	}
	s.auth.mu.Lock()
	result := s.auth.loginResults[orphan]
	pending := len(s.auth.pending)
	s.auth.mu.Unlock()
	if result.Status != "pending" || pending != 1 || time.Until(result.Expires) > 10*time.Minute {
		t.Fatalf("unbounded orphan: %+v pending=%d", result, pending)
	}
	link, _ := s.auth.beginLogin()
	v, _ := url.Parse(link)
	replacement := v.Query().Get("login_trace_id")
	s.auth.mu.Lock()
	old := s.auth.loginResults[orphan]
	_, exists := s.auth.pending[orphan]
	pending = len(s.auth.pending)
	s.auth.mu.Unlock()
	if old.Status != "superseded" || exists || pending != 1 || replacement == orphan {
		t.Fatal("orphan not replaced")
	}
	t.Log("Malformed/lost start response leaves a pending attempt; <=10 minute expiry and next start supersedes it. CLI cannot cancel it without ID.")
}

func TestQA003RefreshCannotReplayAcrossAccounts(t *testing.T) {
	for _, which := range []string{"prompt", "models"} {
		t.Run(which, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			observed := make(chan string, 4)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "ExchangeToken") {
					close(started)
					<-release
					fmt.Fprintf(w, `{"Result":{"Token":"refreshed-A","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
					return
				}
				token := r.Header.Get("X-Cloudide-Token")
				observed <- token
				if token != "account-B" {
					w.WriteHeader(401)
					return
				}
				if which == "models" {
					fmt.Fprint(w, `{"code":0,"data":{"list":[{"models":[{"name":"m"}]}]}}`)
				} else {
					fmt.Fprint(w, textFrames("B response"))
				}
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL = up.URL
			cfg.AuthBaseURL = up.URL
			s := authenticatedServer(t, cfg)
			s.auth.cred.RefreshToken = "synthetic-refresh"
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			c, g := s.auth.snapshot()
			done := make(chan error, 1)
			go func() {
				var err error
				if which == "models" {
					_, err = s.upstream.modelNames(context.Background(), c, g)
				} else {
					_, _, err = s.execute(context.Background(), chatRequest{
						Model: "m",
						Messages: []message{{
							Role:    "user",
							Content: "private account A history",
						}},
					}, nil)
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("refresh not started")
			}
			link, _ := s.auth.beginLogin()
			u, _ := url.Parse(link)
			q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"account-B"}`}, "userInfo": {`{"UserID":"user-B"}`}}
			w := httptest.NewRecorder()
			s.auth.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
			close(release)
			err := <-done
			close(observed)
			var tokens []string
			for token := range observed {
				tokens = append(tokens, token)
			}
			if w.Code != 200 {
				t.Fatal("synthetic B login failed")
			}
			for _, token := range tokens {
				if token == "account-B" {
					t.Errorf("old account request replayed with B token; observed=%v returned_error=%v", tokens, err)
				}
			}
			if err != errLoginRequired {
				t.Errorf("account switch must reject old request: %v", err)
			}
		})
	}
}

func TestQA003ExchangeHTTPRejectionRequiresLogin(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL = up.URL
			cfg.AuthBaseURL = up.URL
			s := authenticatedServer(t, cfg)
			s.auth.cred.RefreshToken = "synthetic-refresh"
			_, _, err := s.execute(context.Background(), chatRequest{
				Model: "m",
				Messages: []message{{
					Role:    "user",
					Content: "hi",
				}},
			}, nil)
			if err != errLoginRequired || statusForError(err) != 401 {
				t.Errorf("refresh HTTP %d maps to %v / gateway HTTP %d; want login-required / 401", code, err, statusForError(err))
			}
			s.auth.mu.Lock()
			blocked := s.auth.refreshErr != nil
			s.auth.mu.Unlock()
			if !blocked {
				t.Error("rejected refresh credential remains retryable")
			}
		})
	}
}

func TestQA003TopLevelRefreshUserCannotChangeAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Result":{"Token":"different-account","UserID":"other-user","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.AuthBaseURL = up.URL
	s := authenticatedServer(t, cfg)
	c, _ := s.auth.snapshot()
	c.RefreshToken = "synthetic-refresh"
	fresh, err := s.auth.exchange(context.Background(), c)
	if err != errLoginRequired {
		t.Fatalf("top-level UserID mismatch accepted: old=%q new=%q error=%v", c.UserID, fresh.UserID, err)
	}
}

func TestQA003InFlightCancelAndReplaceCannotCommit(t *testing.T) {
	for _, kind := range []string{"cancel", "replace"} {
		t.Run(kind, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				fmt.Fprint(w, `{"Result":{"Token":"old-attempt","UserID":"fixture"}}`)
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			s, _ := newServer(cfg)
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			link, _ := s.auth.beginLogin()
			u, _ := url.Parse(link)
			id := u.Query().Get("login_trace_id")
			done := make(chan int, 1)
			go func() {
				w := httptest.NewRecorder()
				s.auth.callback(w, httptest.NewRequest("GET", "/authorize?loginTraceID="+id+"&refreshToken=synthetic", nil))
				done <- w.Code
			}()
			<-started
			if kind == "cancel" {
				loginRequest(t, s, "/api/auth/login/cancel", map[string]string{"attempt_id": id})
			} else {
				s.auth.beginLogin()
			}
			close(release)
			code := <-done
			c, _ := s.auth.snapshot()
			if code == 200 || c.Token != "" {
				t.Fatalf("in-flight %s committed: HTTP %d logged=%t", kind, code, c.Token != "")
			}
		})
	}
}

func TestQA003CallbackRejectsUntrustedHost(t *testing.T) {
	cfg := fixtureConfig()
	cfg.AuthAddr = "127.0.0.1:0"
	s, _ := newServer(cfg)
	defer s.shutdown()
	listener, err := s.startAuthListener()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	link, _ := s.auth.beginLogin()
	u, _ := url.Parse(link)
	id := u.Query().Get("login_trace_id")
	q := url.Values{"loginTraceID": {id}, "userJwt": {`{"Token":"synthetic-evil-host"}`}, "userInfo": {`{"UserID":"fixture"}`}}
	req, _ := http.NewRequest("GET", s.auth.callbackURL+"?"+q.Encode(), nil)
	req.Host = "attacker.invalid:8765"
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	c, _ := s.auth.snapshot()
	if resp.StatusCode != 403 || c.Token != "" {
		t.Fatalf("untrusted callback Host accepted: HTTP %d installed_token=%t; want HTTP 403 and no credential", resp.StatusCode, c.Token != "")
	}
}

func TestTRAE001ToolIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		records []map[string]any
	}{
		{
			"separate IDs without indices",
			[]map[string]any{
				{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
				{"id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
			},
		},
		{
			"ID acquires index",
			[]map[string]any{
				{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{"}},
				{"id": "a", "index": 7, "function_call": map[string]string{"arguments": ""}},
				{"index": 7, "function_call": map[string]string{"arguments": "}"}},
			},
		},
		{
			"unindexed continuation follows latest",
			[]map[string]any{
				{"index": 0, "id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
				{"index": 1, "id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{"}},
				{"function_call": map[string]string{"arguments": "}"}},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			wire := ""
			for _, record := range scenario.records {
				wire += frame("output", map[string]any{"tool_call": record})
			}
			wire += frame("done", map[string]any{})
			var streamed []delta
			result, err := readIDEStream(strings.NewReader(wire), chatRequest{
				Tools:  []tool{fileTool()},
				Choice: "auto",
			}, func(d delta) error {
				streamed = append(streamed, d)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if scenario.name == "ID acquires index" {
				want = 1
			}
			if len(result.Message.ToolCalls) != want {
				t.Fatalf("calls=%+v", result.Message.ToolCalls)
			}
			args := make([]string, want)
			ids := make([]string, want)
			for _, d := range streamed {
				if d.Call != nil {
					if d.Index >= want {
						t.Fatalf("index %d", d.Index)
					}
					args[d.Index] += d.Call.Function.Arguments
					if d.Call.ID != "" {
						ids[d.Index] = d.Call.ID
					}
				}
			}
			for i, c := range result.Message.ToolCalls {
				if c.Function.Arguments != "{}" || args[i] != c.Function.Arguments || ids[i] != c.ID {
					t.Fatalf("call=%+v stream=%v ids=%v", c, args, ids)
				}
			}
		})
	}
}
