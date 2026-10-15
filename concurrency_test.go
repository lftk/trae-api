package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	request := chatRequest{
		Model: "m",
		Messages: []message{{
			Role:    "user",
			Content: "hi",
		}},
	}
	first := make(chan error, 1)
	go func() {
		_, _, e := s.execute(context.Background(), request, nil)
		first <- e
	}()
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
	go func() {
		_, _, e := s.execute(context.Background(), request, nil)
		next <- e
	}()
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
		_, _, e := s.execute(ctx, chatRequest{
			Model: "m",
			Messages: []message{{
				Role:    "user",
				Content: "hi",
			}},
		}, nil)
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

type qaRetestTransport func(*http.Request) (*http.Response, error)

func (f qaRetestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Switch B after all generation checks, at retry transport entry, before
// the underlying transport sends the actual request to the local upstream.
func TestQARetestTemporaryRefreshFailureRecovers(t *testing.T) {
	for _, code := range []int{500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.WriteHeader(code)
					return
				}
				fmt.Fprintf(w, `{"Result":{"Token":"recovered-A","UserID":"account","TokenExpireAt":%d}}`, time.Now().Add(time.Hour).Unix())
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			defer a.client.CloseIdleConnections()
			a.cred = credential{
				Token:        "expired-A",
				UserID:       "account",
				RefreshToken: "synthetic-refresh",
				ExpiresAt:    time.Now().Add(-time.Second),
			}
			_, _, first := a.get(context.Background())
			if first == nil || first == errLoginRequired {
				t.Fatalf("5xx not temporary: %v", first)
			}
			c, _, err := a.get(context.Background())
			if err != nil || c.Token != "recovered-A" || calls != 2 {
				t.Fatalf("recovery failed: calls=%d credential=%+v err=%v", calls, c, err)
			}
		})
	}
}

func TestTRAE003RefreshRejectionIsStickyBut5xxIsNot(t *testing.T) {
	for _, code := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(code)
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			a.cred = credential{
				Token:        "synthetic",
				RefreshToken: "synthetic-refresh",
				ExpiresAt:    time.Now().Add(-time.Second),
			}
			_, _, first := a.get(context.Background())
			_, _, second := a.get(context.Background())
			if code == 500 {
				if first == errLoginRequired || second == errLoginRequired || calls != 2 {
					t.Fatalf("temporary failure poisoned credential: %v %v calls=%d", first, second, calls)
				}
			} else if first != errLoginRequired || second != errLoginRequired || calls != 1 {
				t.Fatalf("auth rejection retried: %v %v calls=%d", first, second, calls)
			}
		})
	}
}

func TestTRAE003RefreshIdentityFormats(t *testing.T) {
	for _, identity := range []string{`"UserID":"other"`, `"UserInfo":{"UserID":"other"}`, `"UserID":"other","UserInfo":{"UserID":"account"}`, `"UserID":"account","UserInfo":{"UserID":"other"}`} {
		t.Run(identity, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"Result":{"Token":"synthetic",%s}}`, identity)
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.AuthBaseURL = up.URL
			a, _ := newAuthManager(cfg)
			_, err := a.exchange(context.Background(), credential{
				UserID:       "account",
				RefreshToken: "synthetic",
			})
			if err != errLoginRequired {
				t.Fatalf("identity mismatch accepted: %v", err)
			}
		})
	}
}

type qaDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (w *qaDeadlineWriter) SetWriteDeadline(d time.Time) error {
	w.deadlines = append(w.deadlines, d)
	return nil
}

func TestQA003EachSSEWriteRenewsDeadline(t *testing.T) {
	w := &qaDeadlineWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	s := newStreamSink(w, httptest.NewRequest("GET", "http://127.0.0.1/", nil))
	defer s.close()
	for i, f := range []func() error{func() error { return s.send("", map[string]string{"x": "start"}) }, s.heartbeat, s.done} {
		start := time.Now()
		if err := f(); err != nil {
			t.Fatal(err)
		}
		if len(w.deadlines) != i+1 || w.deadlines[i].Before(start.Add(29*time.Second)) {
			t.Fatalf("write %d did not renew deadline: %v", i, w.deadlines)
		}
	}
	before := w.Body.String()
	_ = s.heartbeat()
	if err := s.send("", nil); err == nil {
		t.Fatal("write after DONE accepted")
	}
	if w.Body.String() != before {
		t.Fatal("post terminal data written")
	}
}

func TestTRAE001ToolIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		records []map[string]any
	}{
		{
			"separate IDs without indices",
			[]map[string]any{
				{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
				{"id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
			},
		},
		{
			"ID acquires index",
			[]map[string]any{
				{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{"}},
				{"id": "a", "index": 7, "function_call": map[string]string{"arguments": ""}},
				{"index": 7, "function_call": map[string]string{"arguments": "}"}},
			},
		},
		{
			"unindexed continuation follows latest",
			[]map[string]any{
				{"index": 0, "id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
				{"index": 1, "id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{"}},
				{"function_call": map[string]string{"arguments": "}"}},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			wire := ""
			for _, record := range scenario.records {
				wire += frame("output", map[string]any{"tool_call": record})
			}
			wire += frame("done", map[string]any{})
			var streamed []delta
			result, err := readIDEStream(strings.NewReader(wire), chatRequest{
				Tools:  []tool{fileTool()},
				Choice: "auto",
			}, func(d delta) error {
				streamed = append(streamed, d)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if scenario.name == "ID acquires index" {
				want = 1
			}
			if len(result.Message.ToolCalls) != want {
				t.Fatalf("calls=%+v", result.Message.ToolCalls)
			}
			args := make([]string, want)
			ids := make([]string, want)
			for _, d := range streamed {
				if d.Call != nil {
					if d.Index >= want {
						t.Fatalf("index %d", d.Index)
					}
					args[d.Index] += d.Call.Function.Arguments
					if d.Call.ID != "" {
						ids[d.Index] = d.Call.ID
					}
				}
			}
			for i, c := range result.Message.ToolCalls {
				if c.Function.Arguments != "{}" || args[i] != c.Function.Arguments || ids[i] != c.ID {
					t.Fatalf("call=%+v stream=%v ids=%v", c, args, ids)
				}
			}
		})
	}
}
