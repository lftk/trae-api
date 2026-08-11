package main

import (
	"testing"
	"time"
)

func TestLoadConfigProcessLimits(t *testing.T) {
	t.Setenv("TRAE_API_WORKDIR", t.TempDir())
	t.Setenv("TRAE_API_ADDR", "127.0.0.1:8723")

	t.Run("default max processes", func(t *testing.T) {
		t.Setenv("TRAE_API_MAX_PROCESSES", "")
		t.Setenv("TRAE_API_WARM_PROCESSES", "")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MaxProcesses != defaultMaxProcesses {
			t.Fatalf("max processes = %d, want %d", cfg.MaxProcesses, defaultMaxProcesses)
		}
		if cfg.WarmProcesses != defaultWarmProcesses {
			t.Fatalf("warm processes = %d, want %d", cfg.WarmProcesses, defaultWarmProcesses)
		}
	})

	t.Run("max processes must be positive", func(t *testing.T) {
		t.Setenv("TRAE_API_MAX_PROCESSES", "0")
		if _, err := loadConfig(); err == nil {
			t.Fatal("loadConfig succeeded with zero max processes")
		}
	})

	t.Run("warm processes cannot exceed max", func(t *testing.T) {
		t.Setenv("TRAE_API_MAX_PROCESSES", "1")
		t.Setenv("TRAE_API_WARM_PROCESSES", "2")
		if _, err := loadConfig(); err == nil {
			t.Fatal("loadConfig succeeded with warm processes above max")
		}
	})

	t.Run("implicit session timeout defaults to 30m", func(t *testing.T) {
		t.Setenv("TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT", "")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ImplicitIdleTimeout != 30*time.Minute {
			t.Fatalf("implicit session timeout = %s, want 30m0s", cfg.ImplicitIdleTimeout)
		}
	})

	t.Run("implicit session timeout can be zero to disable", func(t *testing.T) {
		t.Setenv("TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT", "0")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ImplicitIdleTimeout != 0 {
			t.Fatalf("implicit session timeout = %s, want 0s", cfg.ImplicitIdleTimeout)
		}
	})

	t.Run("implicit session timeout must not be negative", func(t *testing.T) {
		t.Setenv("TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT", "-1m")
		if _, err := loadConfig(); err == nil {
			t.Fatal("loadConfig succeeded with negative implicit session timeout")
		}
	})
}
