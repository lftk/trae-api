package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestTRAE003LoginAttemptBoundaries(t *testing.T) {
	for _, kind := range []string{"cancelled", "superseded", "expired"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newServer(fixtureConfig())
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			first := loginRequest(t, s, "/api/auth/login", map[string]any{})
			id := decoded(t, first)["attempt_id"].(string)
			switch kind {
			case "cancelled":
				w := loginRequest(t, s, "/api/auth/login/cancel", map[string]string{"attempt_id": id})
				if w.Code != 200 {
					t.Fatalf("cancel missing: %d", w.Code)
				}
			case "superseded":
				loginRequest(t, s, "/api/auth/login", map[string]any{})
			case "expired":
				s.auth.mu.Lock()
				r := s.auth.loginResults[id]
				r.Expires = time.Now().Add(-time.Second)
				s.auth.loginResults[id] = r
				s.auth.mu.Unlock()
			}
			state := loginRequest(t, s, "/api/auth/login/status", map[string]string{"attempt_id": id})
			if decoded(t, state)["status"] != kind {
				t.Fatalf("status=%s want=%s", state.Body.String(), kind)
			}
			cb := httptest.NewRecorder()
			s.auth.callback(cb, httptest.NewRequest("GET", "/authorize?loginTraceID="+id+"&userJwt=%7B%22Token%22%3A%22fixture%22%7D&userInfo=%7B%22UserID%22%3A%22fixture%22%7D", nil))
			if cb.Code == 200 {
				t.Fatal("inactive attempt logged in")
			}
		})
	}
}
func TestTRAE003ExpiredAccountCanLogin(t *testing.T) {
	s := authenticatedServer(t, fixtureConfig())
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	s.auth.cred.ExpiresAt = time.Now().Add(-time.Second)
	w := loginRequest(t, s, "/api/auth/login", map[string]any{})
	if decoded(t, w)["status"] != "pending" {
		t.Fatal("expired account treated as logged in")
	}
}
