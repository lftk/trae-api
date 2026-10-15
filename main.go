package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is overridden at build time via
// -ldflags "-X main.version=vX.Y.Z" (e.g. by the Homebrew formula).
var version = "dev"

func main() {
	if code, stop := parseFlags(os.Args[1:], os.Stdout, os.Stderr); stop {
		os.Exit(code)
	}
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "login" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runLogin(ctx, cfg, os.Stdout, openOfficialBrowser, 500*time.Millisecond, 10*time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	setupLogger(false)
	s, err := newServer(cfg)
	if err != nil {
		slog.Error("initialize service", "error", err)
		os.Exit(1)
	}
	defer s.shutdown()
	authServer, err := s.startAuthListener()
	if err != nil {
		slog.Error("start authorization callback listener", "error", err)
		os.Exit(1)
	}
	defer authServer.Close()

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
		"upstream", cfg.BaseURL,
		"login", "run trae-api login",
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

// parseFlags parses command-line flags with the standard flag package. It
// returns (exitCode, true) when the flags were handled and the program should
// exit, and (0, false) when execution should continue. All runtime
// configuration comes from TRAE_API_* environment variables; the command line
// offers login, -h/--help and -version.
func parseFlags(args []string, stdout, stderr io.Writer) (int, bool) {
	if len(args) > 0 && args[0] == "login" {
		if len(args) == 1 {
			return 0, false
		}
		if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
			fmt.Fprintln(stdout, "Usage: trae-api login\nLog in to the running service via official browser authorization (10 minute timeout).\nUse the same TRAE_API_ADDR and TRAE_API_TOKEN as the service. Ctrl+C cancels.")
			return 0, true
		}
		fmt.Fprintln(stderr, "login accepts no additional arguments")
		return 2, true
	}
	fs := flag.NewFlagSet("trae-api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeUsage(fs, stdout) }
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		// -h/--help: flag already ran Usage, which prints to stdout; exit 0.
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if *showVersion {
		fmt.Fprintf(stdout, "trae-api %s\n", version)
		return 0, true
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unknown command; use trae-api or trae-api login")
		return 2, true
	}
	return 0, false
}

func writeUsage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintf(w, `trae-api %s

OpenAI-compatible HTTP gateway for Trae CN (direct HTTP).

Usage:
  trae-api [flags]        Start the API service
  trae-api login          Log in to the running service

Configuration is read from TRAE_API_* environment variables; see the
project README for the full list. Highlights:

  TRAE_API_ADDR          listen address (default 127.0.0.1:8723)
  TRAE_API_TOKEN         API token, required when binding a non-loopback address
  Authorization callback is fixed at 127.0.0.1:8765.
  Credentials exist only in memory; restart requires login again.

Flags:
`, version)
	fs.PrintDefaults()
}

func setupLogger(debug bool) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	if debug {
		level.Set(slog.LevelDebug)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}
