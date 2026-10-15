package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTRAE001ManagementHost(t *testing.T) {
	s := authenticatedServer(t, fixtureConfig())
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	for _, host := range []string{"attacker.invalid:8723", "localhost.attacker.invalid:8723", "127.0.0.1.attacker.invalid:8723", "0.0.0.0:8723", "[::]:8723"} {
		for _, route := range []struct {
			method string
			path   string
		}{{
			"GET",
			"/web/login",
		}, {
			"GET",
			"/api/auth/status",
		}, {
			"POST",
			"/api/auth/login",
		}, {
			"POST",
			"/api/auth/logout",
		}} {
			t.Run(host+route.path, func(t *testing.T) {
				r := httptest.NewRequest(route.method, "http://"+host+route.path, nil)
				r.Header.Set("Origin", "http://"+host)
				r.Header.Set("X-Forwarded-Host", "localhost:8723")
				w := httptest.NewRecorder()
				authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })).ServeHTTP(w, r)
				if w.Code != 403 {
					t.Fatalf("malicious host allowed: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	c, _ := s.auth.snapshot()
	if c.Token == "" {
		t.Fatal("denied logout changed account")
	}
	for _, host := range []string{"localhost:8723", "127.0.0.1:8723", "[::1]:8723"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/auth/status", nil)
		w := httptest.NewRecorder()
		authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })).ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("local host rejected %s: %d", host, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://localhost:8723/api/auth/status", nil)
	r.Header.Set("Origin", "https://attacker.invalid")
	w := httptest.NewRecorder()
	authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })).ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), `"user_id"`) {
		t.Fatal("cross-origin management read allowed")
	}
}
