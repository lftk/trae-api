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
	WarmProcesses       int
	MaxSessions         int
}

func loadConfig() (config, error) {
	var err error
	idleTimeout, err := durationFromEnv("TRAE_API_SESSION_IDLE_TIMEOUT", 720*time.Hour)
	if err != nil {
		return config{}, err
	}
	scanInterval, err := durationFromEnv("TRAE_API_SESSION_SCAN_INTERVAL", time.Minute)
	if err != nil {
		return config{}, err
	}
	shutdownTimeout, err := durationFromEnv("TRAE_API_SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return config{}, err
	}
	warmProcesses, err := intFromEnv("TRAE_API_WARM_PROCESSES", 1)
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
		WarmProcesses:       warmProcesses,
		MaxSessions:         maxSessions,
	}
	if !isLoopbackAddr(cfg.Addr) && cfg.APIToken == "" {
		if workdirTemp {
			_ = os.RemoveAll(workdir)
		}
		return config{}, errors.New("TRAE_API_TOKEN is required when TRAE_API_ADDR is not loopback")
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

func durationFromEnv(key string, fallback time.Duration) (time.Duration, error) {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			if d <= 0 {
				return 0, fmt.Errorf("%s must be greater than zero", key)
			}
			return d, nil
		} else {
			return 0, fmt.Errorf("parse %s: %w", key, err)
		}
	}
	return fallback, nil
}
