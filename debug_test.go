package main

import (
	"net/http"
	"testing"
)

func TestDebugHeadersRedactsCredentials(t *testing.T) {
	got := debugHeaders(http.Header{
		"Authorization": []string{"Bearer secret"},
		"Cookie":        []string{"session=secret"},
		"X-API-Key":     []string{"secret"},
		"X-Session-ID":  []string{"session-id"},
	})
	for _, key := range []string{"Authorization", "Cookie", "X-API-Key"} {
		if got[key][0] != "<redacted>" {
			t.Fatalf("header %s was not redacted: %v", key, got[key])
		}
	}
	if got["X-Session-ID"][0] != "session-id" {
		t.Fatalf("session header was unexpectedly redacted: %v", got["X-Session-ID"])
	}
}
