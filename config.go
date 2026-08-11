package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type config struct {
	Addr                string
	TraeBin             string
	Yolo                bool
	Debug               bool
	Workdir             string
	WorkdirTemp         bool
	APIToken            string
	SessionIdleTimeout  time.Duration
	SessionScanInterval time.Duration
	ShutdownTimeout     time.Duration
	ImplicitIdleTimeout time.Duration
	WarmProcesses       int
	MaxSessions         int
	MaxProcesses        int
}

const defaultWarmProcesses = 4

func loadConfig() (config, error) {
	idleTimeout, err := durationFromEnv("TRAE_API_SESSION_IDLE_TIMEOUT", 720*time.Hour, false)
	if err != nil {
		return config{}, err
	}
	scanInterval, err := durationFromEnv("TRAE_API_SESSION_SCAN_INTERVAL", time.Minute, false)
	if err != nil {
		return config{}, err
	}
	shutdownTimeout, err := durationFromEnv("TRAE_API_SHUTDOWN_TIMEOUT", 30*time.Second, false)
	if err != nil {
		return config{}, err
	}
	warmProcesses, err := intFromEnv("TRAE_API_WARM_PROCESSES", defaultWarmProcesses)
	if err != nil {
		return config{}, err
	}
	if warmProcesses < 0 {
		return config{}, errors.New("TRAE_API_WARM_PROCESSES must be non-negative")
	}
	maxSessions, err := intFromEnv("TRAE_API_MAX_SESSIONS", 100)
	if err != nil {
		return config{}, err
	}
	if maxSessions < 0 {
		return config{}, errors.New("TRAE_API_MAX_SESSIONS must be non-negative")
	}
	maxProcesses, err := intFromEnv("TRAE_API_MAX_PROCESSES", defaultMaxProcesses)
	if err != nil {
		return config{}, err
	}
	if maxProcesses < 1 {
		return config{}, errors.New("TRAE_API_MAX_PROCESSES must be at least 1")
	}
	if warmProcesses > maxProcesses {
		return config{}, errors.New("TRAE_API_WARM_PROCESSES must not exceed TRAE_API_MAX_PROCESSES")
	}
	implicitIdleTimeout, err := durationFromEnv("TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT", 30*time.Minute, true)
	if err != nil {
		return config{}, err
	}
	yolo, err := boolFromEnv("TRAE_API_YOLO", true)
	if err != nil {
		return config{}, err
	}
	debug, err := boolFromEnv("TRAE_API_DEBUG", false)
	if err != nil {
		return config{}, err
	}
	workdir := os.Getenv("TRAE_API_WORKDIR")
	workdirTemp := false
	if workdir == "" {
		workdir, err = os.MkdirTemp("", "trae-api-")
		if err != nil {
			return config{}, fmt.Errorf("create temporary work directory: %w", err)
		}
		workdirTemp = true
	}
	cfg := config{
		Addr:                getenv("TRAE_API_ADDR", "127.0.0.1:8723"),
		TraeBin:             getenv("TRAE_API_BIN", "trae-cli"),
		Yolo:                yolo,
		Debug:               debug,
		Workdir:             workdir,
		WorkdirTemp:         workdirTemp,
		APIToken:            os.Getenv("TRAE_API_TOKEN"),
		SessionIdleTimeout:  idleTimeout,
		SessionScanInterval: scanInterval,
		ShutdownTimeout:     shutdownTimeout,
		ImplicitIdleTimeout: implicitIdleTimeout,
		WarmProcesses:       warmProcesses,
		MaxSessions:         maxSessions,
		MaxProcesses:        maxProcesses,
	}
	if !isLoopbackAddr(cfg.Addr) && cfg.APIToken == "" {
		if workdirTemp {
			_ = os.RemoveAll(workdir)
		}
		return config{}, errors.New("TRAE_API_TOKEN is required when TRAE_API_ADDR is not loopback")
	}
	if !workdirTemp {
		if _, err := resolveWorkdir(workdir); err != nil {
			return config{}, err
		}
	}
	return cfg, nil
}

func intFromEnv(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func boolFromEnv(key string, fallback bool) (bool, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", key, err)
	}
	return parsed, nil
}

func acpArgs(yolo bool) []string {
	args := []string{"acp", "serve"}
	if yolo {
		args = append(args, "--yolo")
	}
	return args
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolveWorkdir(value string) (string, error) {
	if value == "" {
		return "", errors.New("project directory is required")
	}
	workdir, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve project directory: %w", err)
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return "", fmt.Errorf("stat project directory %q: %w", workdir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project directory is not a directory: %s", workdir)
	}
	return workdir, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// durationFromEnv parses a duration environment variable. With allowZero set,
// zero is accepted and disables the feature the variable controls; otherwise
// only strictly positive values are valid. Negative values are always rejected.
func durationFromEnv(key string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative", key)
	}
	if d == 0 && !allowZero {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return d, nil
}
