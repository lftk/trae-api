package main

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("TRAE_API_YOLO", "")
	t.Setenv("TRAE_API_ARGS", "invalid args that must be ignored")
	t.Setenv("TRAE_API_DEFAULT_MODEL", "legacy-model")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.WorkdirTemp {
		t.Cleanup(func() { _ = os.RemoveAll(cfg.Workdir) })
	}
	if !cfg.Yolo {
		t.Fatal("loadConfig() Yolo = false, want true")
	}
	if got, want := acpArgs(cfg.Yolo), []string{"acp", "serve", "--yolo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("acpArgs() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigYoloFalse(t *testing.T) {
	t.Setenv("TRAE_API_YOLO", "false")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.WorkdirTemp {
		t.Cleanup(func() { _ = os.RemoveAll(cfg.Workdir) })
	}
	if cfg.Yolo {
		t.Fatal("loadConfig() Yolo = true, want false")
	}
	if got, want := acpArgs(cfg.Yolo), []string{"acp", "serve"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("acpArgs() = %#v, want %#v", got, want)
	}
}

func TestLoadConfigInvalidYolo(t *testing.T) {
	t.Setenv("TRAE_API_YOLO", "sometimes")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() error = nil, want invalid TRAE_API_YOLO error")
	}
}

func TestSessionSetModelDefaultsToFirstAdvertisedModel(t *testing.T) {
	session := &traeSession{models: []string{"first", "second"}}

	got, err := session.setModel(context.Background(), "")
	if err != nil {
		t.Fatalf("setModel() error = %v", err)
	}
	if got != "first" {
		t.Fatalf("setModel() = %q, want %q", got, "first")
	}
}

func TestParseModelsEmptyOutput(t *testing.T) {
	for _, output := range [][]byte{nil, []byte("[]"), []byte("\n")} {
		models, err := parseModels(output)
		if err != nil {
			t.Fatalf("parseModels(%q) error = %v", output, err)
		}
		if len(models) != 0 {
			t.Fatalf("parseModels(%q) = %#v, want empty list", output, models)
		}
	}
}
