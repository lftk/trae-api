package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
