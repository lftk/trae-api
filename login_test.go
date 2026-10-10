package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func loginRequest(t *testing.T, s *server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8723"+path, strings.NewReader(jsonText(body)))
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}
func TestTRAE003LoginLifecycle(t *testing.T) {
	s, err := newServer(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	start := loginRequest(t, s, "/api/auth/login", map[string]any{})
	if start.Code != 200 {
		t.Fatalf("login entry missing: %d %s", start.Code, start.Body.String())
	}
	obj := decoded(t, start)
	id, ok := obj["attempt_id"].(string)
	if !ok || id == "" {
		t.Fatal("missing attempt")
	}
	u, _ := url.Parse(obj["url"].(string))
	if u.Host != "www.trae.cn" {
		t.Fatal("not official authorization")
	}
	wait := loginRequest(t, s, "/api/auth/login/status", map[string]string{"attempt_id": id})
	if decoded(t, wait)["status"] != "pending" {
		t.Fatal(wait.Body.String())
	}
	q := url.Values{"loginTraceID": {id}, "userJwt": {`{"Token":"synthetic-callback-token"}`}, "userInfo": {`{"UserID":"fixture"}`}}
	cb := httptest.NewRecorder()
	s.auth.callback(cb, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
	if cb.Code != 200 {
		t.Fatal(cb.Body.String())
	}
	wait = loginRequest(t, s, "/api/auth/login/status", map[string]string{"attempt_id": id})
	if decoded(t, wait)["status"] != "completed" || strings.Contains(wait.Body.String(), "synthetic-callback-token") {
		t.Fatal("completion not correlated or leaked")
	}
	again := loginRequest(t, s, "/api/auth/login", map[string]any{})
	if decoded(t, again)["status"] != "already_logged_in" {
		t.Fatal("duplicate login opened flow")
	}
	restarted, _ := newServer(fixtureConfig())
	defer restarted.shutdown()
	c, _ := restarted.auth.snapshot()
	if c.Token != "" {
		t.Fatal("login survives restart")
	}
}
