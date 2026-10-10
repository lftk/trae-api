package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTRAE003IdleSSEDeadlineRenewal(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := newStreamSink(w, r)
		defer s.close()
		if err := s.send("", map[string]string{"text": "start"}); err != nil {
			return
		}
		timer := time.NewTimer(32 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		_ = s.done()
	}))
	defer up.Close()
	c := &http.Client{Timeout: 40 * time.Second}
	resp, err := c.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("idle stream lost terminal frame: %v %q", err, b)
	}
}
