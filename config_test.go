package main

import "testing"

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
}
