package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTRAE003AllRoutesRejectRebinding(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/models", "/healthz", "/v1/responses"} {
		r := httptest.NewRequest("GET", "http://attacker.invalid"+path, nil)
		w := httptest.NewRecorder()
		authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("%s allowed rebinding Host: %d", path, w.Code)
		}
	}
}
