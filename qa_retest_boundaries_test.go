package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
