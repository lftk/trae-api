package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	setupLogger(cfg.Debug)
	if cfg.WorkdirTemp {
		defer func() {
			if err := os.RemoveAll(cfg.Workdir); err != nil {
				slog.Warn("remove temporary work directory", "workdir", cfg.Workdir, "error", err)
			}
		}()
	}

	s := newServer(cfg)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info(
		"trae-api listening",
		"addr", cfg.Addr,
		"trae", cfg.TraeBin,
		"args", acpArgs(cfg.Yolo),
	)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server stopped", "error", err)
			s.shutdown()
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
		s.shutdown()
	}
}

func setupLogger(debug bool) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	if debug {
		level.Set(slog.LevelDebug)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
