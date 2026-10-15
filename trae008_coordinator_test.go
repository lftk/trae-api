package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTRAE008CombinedFailureClosesAuthAndWriter(t *testing.T) {
	isolatedCredentialPath(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	authAddr := reserve.Addr().String()
	reserve.Close()
	cfg := fixtureConfig()
	cfg.Addr = occupied.Addr().String()
	cfg.AuthAddr = authAddr
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runCombined(ctx, cfg); err == nil {
		t.Fatal("API bind failure was ignored")
	}
	listener, err := net.Listen("tcp", authAddr)
	if err != nil {
		t.Fatalf("Auth listener leaked: %v", err)
	}
	listener.Close()
	store, err := openCredentialStore()
	if err != nil {
		t.Fatalf("Auth writer lock leaked: %v", err)
	}
	store.Close()
}

func TestTRAE008CombinedAuthBindFailureClosesAPI(t *testing.T) {
	isolatedCredentialPath(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiAddr := reserve.Addr().String()
	reserve.Close()
	cfg := fixtureConfig()
	cfg.Addr = apiAddr
	cfg.AuthAddr = occupied.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runCombined(ctx, cfg); err == nil {
		t.Fatal("Auth bind failure was ignored")
	}
	listener, err := net.Listen("tcp", apiAddr)
	if err != nil {
		t.Fatalf("API listener leaked: %v", err)
	}
	listener.Close()
	store, err := openCredentialStore()
	if err != nil {
		t.Fatalf("Auth writer lock leaked: %v", err)
	}
	store.Close()
}

func TestTRAE008CombinedJoinsBothComponentErrors(t *testing.T) {
	isolatedCredentialPath(t)
	api, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	auth, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	cfg := fixtureConfig()
	cfg.Addr = api.Addr().String()
	cfg.AuthAddr = auth.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runCombined(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "API stopped:") || !strings.Contains(err.Error(), "Auth stopped:") {
		t.Fatalf("both component failures must be labeled: %v", err)
	}
}

func TestTRAE008APIOnlyDoesNotAcquireAuthWriter(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := fixtureConfig()
	cfg.Addr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runAPIOnly(ctx, cfg); err != nil {
		t.Fatalf("API-only required Auth writer: %v", err)
	}
}
