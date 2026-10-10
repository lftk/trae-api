package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestTRAE003RefreshRejectionIsStickyBut5xxIsNot(t *testing.T) {
	for _, code := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(code) }))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			a.cred = credential{Token: "synthetic", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Second)}
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
			_, err := a.exchange(context.Background(), credential{UserID: "account", RefreshToken: "synthetic"})
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
