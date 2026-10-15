package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTRAE003LoginCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stop   bool
		output string
	}{
		{
			[]string{"login", "--help"},
			0,
			true,
			"trae-api login",
		},
		{
			[]string{"unknown"},
			2,
			true,
			"",
		},
		{
			[]string{"login", "extra"},
			2,
			true,
			"",
		},
		{
			[]string{"login"},
			0,
			false,
			"",
		},
	} {
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		code, stop := parseFlags(tc.args, out, errOut)
		if code != tc.code || stop != tc.stop || !strings.Contains(out.String(), tc.output) {
			t.Fatalf("args=%v code=%d stop=%v output=%s", tc.args, code, stop, out)
		}
	}
}

func TestTRAE003CLIWaitAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "browser", true: "fallback"}[fallback], func(t *testing.T) {
			s, _ := newServer(fixtureConfig())
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			api := httptest.NewServer(s.routes())
			defer api.Close()
			u, _ := url.Parse(api.URL)
			cfg := fixtureConfig()
			cfg.Addr = u.Host
			out := &bytes.Buffer{}
			opened := 0
			err := runLogin(context.Background(), cfg, out, func(ctx context.Context, link string) error {
				opened++
				a, _ := url.Parse(link)
				id := a.Query().Get("login_trace_id")
				q := url.Values{"loginTraceID": {id}, "userJwt": {`{"Token":"synthetic-sensitive-fixture"}`}, "userInfo": {`{"UserID":"fixture"}`}}
				w := httptest.NewRecorder()
				s.auth.callback(w, httptest.NewRequest("GET", "/authorize?"+q.Encode(), nil))
				if w.Code != 200 {
					t.Error("callback failed")
				}
				if fallback {
					return errors.New("synthetic-sensitive-fixture /authorize?userJwt=secret")
				}
				return nil
			}, time.Millisecond, time.Second)
			if err != nil || opened != 1 || !strings.Contains(out.String(), "Login successful") {
				t.Fatalf("err=%v opened=%d out=%s", err, opened, out)
			}
			if strings.Contains(out.String(), "synthetic-sensitive-fixture") || strings.Contains(out.String(), "userJwt") {
				t.Fatal("credentials or full callback leaked")
			}
			if fallback && !strings.Contains(out.String(), "https://www.trae.cn/authorization?") {
				t.Fatal("fallback URL missing")
			}
			out.Reset()
			err = runLogin(context.Background(), cfg, out, func(context.Context, string) error {
				t.Fatal("opened browser for logged in service")
				return nil
			}, time.Millisecond, time.Second)
			if err != nil || !strings.Contains(out.String(), "Already logged in") {
				t.Fatal("repeat failed")
			}
		})
	}
}
func TestTRAE003CLIErrorBoundaries(t *testing.T) {
	for _, kind := range []string{"cancel", "timeout", "unauthorized", "failed"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixtureConfig()
			if kind == "unauthorized" {
				cfg.APIToken = "fixture-gateway"
			}
			s, _ := newServer(cfg)
			defer s.shutdown()
			s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
			api := httptest.NewServer(s.routes())
			defer api.Close()
			u, _ := url.Parse(api.URL)
			cfg.Addr = u.Host
			cfg.APIToken = ""
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var id string
			out := &bytes.Buffer{}
			err := runLogin(ctx, cfg, out, func(ctx context.Context, link string) error {
				u, _ := url.Parse(link)
				id = u.Query().Get("login_trace_id")
				if kind == "cancel" {
					cancel()
				}
				if kind == "failed" {
					s.auth.callback(httptest.NewRecorder(), httptest.NewRequest("GET", "/authorize?loginTraceID="+id, nil))
				}
				return nil
			}, time.Millisecond, 25*time.Millisecond)
			if err == nil {
				t.Fatal("expected error")
			}
			want := map[string]string{
				"cancel":       "cancelled",
				"timeout":      "timed out",
				"unauthorized": "authentication failed",
				"failed":       "authorization failed",
			}[kind]
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("err=%v", err)
			}
			if id != "" && kind != "failed" {
				s.auth.mu.Lock()
				status := s.auth.loginResults[id].Status
				s.auth.mu.Unlock()
				if status != "cancelled" {
					t.Fatalf("attempt not cleaned up: %s", status)
				}
			}
		})
	}
}
func TestTRAE003ManagementRoutesProtected(t *testing.T) {
	s, _ := newServer(fixtureConfig())
	defer s.shutdown()
	s.auth.callbackURL = "http://127.0.0.1:8765/authorize"
	for _, path := range []string{"/api/auth/login", "/api/auth/login/status", "/api/auth/login/cancel"} {
		r := httptest.NewRequest(http.MethodPost, "http://evil.invalid:8723"+path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "http://evil.invalid:8723")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s bypass: %d", path, w.Code)
		}
	}
}

func TestTRAE003LoginCommandServiceUnavailable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trae-api")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "login")
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TRAE_API_") {
			env = append(env, v)
		}
	}
	cmd.Env = append(env, "TRAE_API_ADDR="+addr)
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "service is not running") {
		t.Fatalf("login did not fail safely: err=%v context=%v output=%s", err, ctx.Err(), out)
	}
}

func TestTRAE003BinaryRestartAndLegacyFiles(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:8765")
	if err != nil {
		t.Skip("fixed authorization port is occupied")
	}
	probe.Close()
	home := t.TempDir()
	legacy := filepath.Join(home, ".local", "state", "trae-api")
	os.MkdirAll(filepath.Join(legacy, "conversations-v2"), 0700)
	files := map[string]string{"credential.json": `{"token":"synthetic-old-file-token","user_id":"fixture"}`, "conversations-v2/resp_fixture.json": `{"messages":[{"role":"user","content":"synthetic old history"}]}`}
	for p, data := range files {
		if err := os.WriteFile(filepath.Join(legacy, p), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "trae-api")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	base := "http://" + addr
	client := newHTTPClient(time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	start := func() func() {
		cmd := exec.Command(bin)
		env := []string{}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "TRAE_API_") && !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "XDG_STATE_HOME=") {
				env = append(env, entry)
			}
		}
		cmd.Env = append(env, "HOME="+home, "TRAE_API_ADDR="+addr)
		logs := &bytes.Buffer{}
		cmd.Stdout = logs
		cmd.Stderr = logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cmd.Process.Signal(os.Interrupt)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					cmd.Process.Kill()
					<-done
				}
				if strings.Contains(logs.String(), "synthetic-") || strings.Contains(logs.String(), "userJwt") {
					t.Error("sensitive callback logged")
				}
			})
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			r, e := client.Get(base + "/healthz")
			if e == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					return stop
				}
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-done:
				t.Fatalf("service exited before healthy")
			}
		}
		t.Fatal("service did not become healthy")
		return stop
	}
	logged := func() bool {
		r, err := client.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var v map[string]any
		json.NewDecoder(r.Body).Decode(&v)
		return v["logged_in"] == true
	}
	stop := start()
	if logged() {
		t.Fatal("legacy credential read")
	}
	r, err := client.Post(base+"/api/auth/login", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var attempt map[string]string
	json.NewDecoder(r.Body).Decode(&attempt)
	r.Body.Close()
	id := attempt["attempt_id"]
	if id == "" {
		t.Fatal("no attempt")
	}
	q := url.Values{"loginTraceID": {id}, "userJwt": {`{"Token":"synthetic-current-token"}`}, "userInfo": {`{"UserID":"fixture"}`}}
	r, err = client.Get("http://127.0.0.1:8765/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !logged() {
		t.Fatal("synthetic login failed")
	}
	stop()
	stop = start()
	if logged() {
		t.Fatal("credentials persisted across process restart")
	}
	stop()
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(legacy, p))
		if err != nil || string(got) != want {
			t.Fatal("legacy file modified")
		}
	}
	names := 0
	filepath.WalkDir(legacy, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names++
		}
		return err
	})
	if names != len(files) {
		t.Fatal("new state files written")
	}
	// Every operation above is local; no official authorization or model request.
}

func TestParseFlags(t *testing.T) {
	const testVersion = "v9.9.9-test"
	oldVersion := version
	version = testVersion
	defer func() { version = oldVersion }()

	tests := []struct {
		name       string
		args       []string
		wantStop   bool
		wantCode   int
		wantStdout string // substring expected in stdout
		wantStderr string // substring expected in stderr
	}{
		{
			"no args continues",
			nil,
			false,
			0,
			"",
			"",
		},
		{
			"unknown command",
			[]string{"whatever"},
			true,
			2,
			"",
			"unknown command",
		},
		{
			"long version",
			[]string{"--version"},
			true,
			0,
			"trae-api " + testVersion,
			"",
		},
		{
			"short version",
			[]string{"-version"},
			true,
			0,
			"trae-api " + testVersion,
			"",
		},
		{
			"long help",
			[]string{"--help"},
			true,
			0,
			"TRAE_API_ADDR",
			"",
		},
		{
			"short help",
			[]string{"-h"},
			true,
			0,
			"TRAE_API_ADDR",
			"",
		},
		{
			"unknown flag",
			[]string{"--wat"},
			true,
			2,
			"TRAE_API_ADDR",
			"flag provided but not defined: -wat",
		},
		{
			"flag with value",
			[]string{"-x=1"},
			true,
			2,
			"TRAE_API_ADDR",
			"flag provided but not defined: -x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			code, stop := parseFlags(tt.args, stdout, stderr)
			if code != tt.wantCode || stop != tt.wantStop {
				t.Fatalf("parseFlags(%q) = (%d, %v), want (%d, %v)", tt.args, code, stop, tt.wantCode, tt.wantStop)
			}
			if got := stdout.String(); !strings.Contains(got, tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", got, tt.wantStdout)
			}
			if got := stderr.String(); !strings.Contains(got, tt.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", got, tt.wantStderr)
			}
		})
	}
}

func TestTRAE003ConfigurationSurface(t *testing.T) {
	t.Setenv("TRAE_API_ADDR", "127.0.0.1:8723")
	t.Setenv("TRAE_API_TOKEN", "")
	for _, key := range []string{"TRAE_API_STATE_DIR", "TRAE_API_AUTH_ADDR", "TRAE_API_BASE_URL", "TRAE_API_MAX_CONCURRENT", "TRAE_API_DEBUG"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "legacy")
			if _, err := loadConfig(); err == nil {
				t.Fatalf("removed config %s accepted", key)
			}
		})
	}
}
