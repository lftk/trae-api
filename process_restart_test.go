package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
		go func() { _ = cmd.Wait(); close(done) }()
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
