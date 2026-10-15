package main

import (
	"net/http"
	"net/url"
	"time"
)

func (a *authManager) finishLogin(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result, ok := a.loginResults[id]
	if ok && result.Status == "pending" {
		result.Status = "failed"
		a.loginResults[id] = result
	}
}
func (s *server) startLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		writeError(w, 403, "cross-origin management request denied")
		return
	}
	s.auth.mu.Lock()
	logged := s.auth.cred.Token != "" && s.auth.refreshErr == nil && (s.auth.cred.ExpiresAt.IsZero() || time.Now().Before(s.auth.cred.ExpiresAt))
	s.auth.mu.Unlock()
	if logged {
		writeJSON(w, 200, map[string]string{"status": "already_logged_in"})
		return
	}
	link, err := s.auth.beginLogin()
	if err != nil {
		writeError(w, 503, "authorization callback listener is unavailable")
		return
	}
	u, _ := url.Parse(link)
	writeJSON(w, 200, map[string]string{
		"status":     "pending",
		"url":        link,
		"attempt_id": u.Query().Get("login_trace_id"),
	})
}
func (s *server) cancelLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		writeError(w, 403, "cross-origin management request denied")
		return
	}
	body, err := decodeBody(w, r)
	if err != nil {
		writeError(w, 400, "invalid cancellation request")
		return
	}
	id := firstString(objectValue(string(body.Body)), "attempt_id")
	s.auth.mu.Lock()
	result, ok := s.auth.loginResults[id]
	if ok && result.Status == "pending" {
		result.Status = "cancelled"
		s.auth.loginResults[id] = result
		delete(s.auth.pending, id)
		if s.auth.latestLogin == id {
			s.auth.latestLogin = ""
		}
	}
	s.auth.mu.Unlock()
	if !ok {
		writeError(w, 404, "login attempt not found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": result.Status})
}

func (s *server) loginStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		writeError(w, 403, "cross-origin management request denied")
		return
	}
	body, err := decodeBody(w, r)
	if err != nil {
		writeError(w, 400, "invalid login status request")
		return
	}
	id := firstString(objectValue(string(body.Body)), "attempt_id")
	s.auth.mu.Lock()
	result, ok := s.auth.loginResults[id]
	if ok && result.Status == "pending" && time.Now().After(result.Expires) {
		result.Status = "expired"
		s.auth.loginResults[id] = result
		delete(s.auth.pending, id)
		if s.auth.latestLogin == id {
			s.auth.latestLogin = ""
		}
	}
	s.auth.mu.Unlock()
	if !ok {
		writeError(w, 404, "login attempt not found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": result.Status})
}
