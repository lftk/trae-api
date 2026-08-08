package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// TestTraeCLIAllowsConcurrentPromptsAcrossDedicatedProcesses verifies that
// separate sessions can execute concurrently when each owns its own process.
//
// This is opt-in because it requires an installed and authenticated trae-cli
// and makes real model requests.
func TestTraeCLIAllowsConcurrentPromptsAcrossDedicatedProcesses(t *testing.T) {
	if os.Getenv("TRAE_API_RUN_INTEGRATION") != "1" {
		t.Skip("set TRAE_API_RUN_INTEGRATION=1 to run against the local trae-cli")
	}

	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(config{
		TraeBin:       getenv("TRAE_API_BIN", "trae-cli"),
		Yolo:          true,
		Workdir:       workdir,
		WarmProcesses: 1,
	})
	defer s.shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	oneLease, err := s.acquireSession(ctx, "integration-one")
	if err != nil {
		t.Fatal(err)
	}
	twoLease, err := s.acquireSession(ctx, "integration-two")
	if err != nil {
		t.Fatal(err)
	}
	one, two := oneLease.session, twoLease.session
	if one.process == two.process {
		t.Fatal("dedicated sessions unexpectedly share a process")
	}

	type result struct{ err error }
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, s := range []*session{one, two} {
		wg.Add(1)
		go func(session *session) {
			defer wg.Done()
			_, _, _, _, err := session.prompt(ctx, "Reply with exactly one word: READY.", nil)
			results <- result{err: err}
		}(s)
	}
	wg.Wait()
	close(results)

	var failures []error
	for result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
		}
	}
	if len(failures) != 0 {
		t.Fatalf("concurrent prompts failed: %v", failures)
	}
}
