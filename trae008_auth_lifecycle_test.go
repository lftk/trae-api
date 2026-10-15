package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestTRAE008AuthShutdownCancelsInFlightExchange(t *testing.T) {
	isolatedCredentialPath(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer mock.Close()
	defer close(release)
	cfg := fixtureConfig()
	cfg.AuthBaseURL = mock.URL
	a, err := newAuthService(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.shutdown()
	a.manager.client.Transport.(*http.Transport).Proxy = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var callback string
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		a.manager.mu.Lock()
		callback = a.manager.callbackURL
		a.manager.mu.Unlock()
		if callback != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if callback == "" {
		t.Fatal("Auth did not listen")
	}
	link, err := a.manager.beginLogin()
	if err != nil {
		t.Fatal(err)
	}
	official, _ := url.Parse(link)
	q := url.Values{
		"loginTraceID": {official.Query().Get("login_trace_id")},
		"refreshToken": {"synthetic-refresh"},
		"userInfo":     {`{"UserID":"fixture"}`},
	}
	client := newHTTPClient(2 * time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	responseDone := make(chan struct{})
	go func() {
		resp, _ := client.Get(callback + "?" + q.Encode())
		if resp != nil {
			resp.Body.Close()
		}
		close(responseDone)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exchange not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Auth shutdown waited for in-flight exchange")
	}
	select {
	case <-responseDone:
	case <-time.After(2 * time.Second):
		t.Fatal("callback request did not cancel")
	}
	if _, err := readAPICredential(); err == nil {
		t.Fatal("cancelled callback published credential")
	}
}
