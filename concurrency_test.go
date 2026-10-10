package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountSlotsQueueCancellationAndRelease(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	var active, max atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, textFrames("ok"))
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	cfg.MaxConcurrent = 1
	cfg.QueueTimeout = 20 * time.Millisecond
	s := authenticatedServer(t, cfg)
	request := chatRequest{Model: "m", Messages: []message{{Role: "user", Content: "hi"}}}
	first := make(chan error, 1)
	go func() { _, _, e := s.execute(context.Background(), request, nil); first <- e }()
	<-started
	_, _, e := s.execute(context.Background(), request, nil)
	if statusForError(e) != 503 || max.Load() != 1 {
		t.Fatalf("queue not limited: %v max=%d", e, max.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, e = s.execute(ctx, request, nil)
	if e != context.Canceled {
		t.Fatal(e)
	}
	release <- struct{}{}
	if e = <-first; e != nil {
		t.Fatal(e)
	}
	next := make(chan error, 1)
	go func() { _, _, e := s.execute(context.Background(), request, nil); next <- e }()
	<-started
	release <- struct{}{}
	if e = <-next; e != nil {
		t.Fatal(e)
	}
	if len(s.slots) != 0 {
		t.Fatal("slot leaked")
	}
}
func TestCancellationStopsUpstreamAndReleasesSlot(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, frame("output", map[string]string{"response": "partial"}))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, e := s.execute(ctx, chatRequest{Model: "m", Messages: []message{{Role: "user", Content: "hi"}}}, nil)
		done <- e
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not cancel")
	}
	if len(s.slots) != 0 {
		t.Fatal("slot leaked")
	}
}
