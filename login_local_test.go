package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestTRAE003LoginRejectsRemoteAddress(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Addr = "203.0.113.1:8723"
	cfg.APIToken = "synthetic-gateway-token"
	err := runLogin(context.Background(), cfg, &bytes.Buffer{}, func(context.Context, string) error {
		t.Fatal("remote service opened browser")
		return nil
	}, time.Millisecond, 15*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "this computer") {
		t.Fatalf("remote target not rejected before network: %v", err)
	}
}
