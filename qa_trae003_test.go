package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
