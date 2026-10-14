package main

import (
	"testing"
)

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
