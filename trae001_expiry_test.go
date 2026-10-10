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

func TestTRAE001LegacyExpiry(t *testing.T) {
	expiry := time.Unix(1900000000, 0)
	t.Run("callback query refreshExpireAt", func(t *testing.T) {
		a, err := newAuthManager(fixtureConfig())
		if err != nil {
			t.Fatal(err)
		}
		a.callbackURL = "http://127.0.0.1:8765/authorize"
		login, err := a.beginLogin()
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(login)
		q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"fixture","tokenExpireAt":1900000000}`}, "userInfo": {`{"UserID":"account"}`}, "refreshExpireAt": {"1900000000"}}
		w := httptest.NewRecorder()
		a.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
		c, _ := a.snapshot()
		if w.Code != 200 || !c.ExpiresAt.Equal(expiry) || !c.RefreshExpiresAt.Equal(expiry) {
			t.Fatalf("status=%d expiry=%v refresh=%v", w.Code, c.ExpiresAt, c.RefreshExpiresAt)
		}
	})
	t.Run("exchange lower-case expiry", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"Result":{"Token":"fresh","tokenExpireAt":1900000000,"refreshExpireAt":1900000000}}`)
		}))
		defer up.Close()
		cfg := fixtureConfig()
		cfg.AuthBaseURL = up.URL
		a, err := newAuthManager(cfg)
		if err != nil {
			t.Fatal(err)
		}
		c, err := a.exchange(context.Background(), credential{RefreshToken: "fixture"})
		if err != nil || !c.ExpiresAt.Equal(expiry) || !c.RefreshExpiresAt.Equal(expiry) {
			t.Fatalf("expiry=%v refresh=%v err=%v", c.ExpiresAt, c.RefreshExpiresAt, err)
		}
	})
	t.Run("callback lower-case refresh expiry", func(t *testing.T) {
		a, _ := newAuthManager(fixtureConfig())
		a.callbackURL = "http://127.0.0.1:8765/authorize"
		login, _ := a.beginLogin()
		u, _ := url.Parse(login)
		q := url.Values{"loginTraceID": {u.Query().Get("login_trace_id")}, "userJwt": {`{"Token":"fixture","refreshExpireAt":1900000000}`}, "userInfo": {`{"UserID":"account"}`}}
		w := httptest.NewRecorder()
		a.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
		c, _ := a.snapshot()
		if w.Code != 200 || !c.RefreshExpiresAt.Equal(expiry) {
			t.Fatalf("status=%d refresh=%v", w.Code, c.RefreshExpiresAt)
		}
	})
}
