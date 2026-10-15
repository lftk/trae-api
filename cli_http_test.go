package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestTRAE007ServeErrorSurvivesShutdownSignal(t *testing.T) {
	serveFailure := errors.New("synthetic Serve failure")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		serveErr := make(chan error, 1)
		serveErr <- serveFailure
		if err := waitForHTTP(ctx, serveErr, func() error { return nil }); !errors.Is(err, serveFailure) {
			t.Fatalf("iteration %d: Serve failure lost: %v", i, err)
		}
	}
}

func TestTRAE007ServeAndShutdownFailuresBothReported(t *testing.T) {
	serveFailure := errors.New("synthetic Serve failure")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	serveErr := make(chan error, 1)
	err := waitForHTTP(ctx, serveErr, func() error {
		serveErr <- serveFailure
		return context.DeadlineExceeded
	})
	if !errors.Is(err, serveFailure) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost Serve or shutdown failure: %v", err)
	}
}

func TestTRAE007WaitForHTTPShutdownOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		shutdownErr error
		wantErr     error
	}{
		{
			name: "clean shutdown",
		},
		{
			name:        "failed shutdown",
			shutdownErr: context.DeadlineExceeded,
			wantErr:     context.DeadlineExceeded,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancel()
			serveErr := make(chan error, 1)
			if err := waitForHTTP(ctx, serveErr, func() error {
				serveErr <- http.ErrServerClosed
				return tt.shutdownErr
			}); !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("waitForHTTP error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestTRAE007ShutdownDeadlineIsFailure(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.shutdown()
	listener, err := s.listenAPI()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	service := s.httpServer()
	service.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // Simulate a handler that ignores cancellation.
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- service.Serve(listener) }()
	defer service.Close()
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	requestDone := make(chan struct{})
	go func() {
		resp, _ := client.Get("http://" + listener.Addr().String() + "/healthz")
		if resp != nil {
			resp.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	if err := s.shutdownHTTP(service, 10*time.Millisecond); err != context.DeadlineExceeded {
		t.Fatalf("shutdown should fail on timeout, got %v", err)
	}
	close(release)
	<-requestDone
	if err := <-serveErr; err != http.ErrServerClosed {
		t.Fatalf("Serve returned %v", err)
	}
}
