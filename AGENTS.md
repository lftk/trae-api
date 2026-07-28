# Repository Guidelines

## Project Structure & Module Organization

This is a single-package Go service (`package main`) with no nested source or asset directories. The root files are organized by responsibility:

- `main.go`: process startup, HTTP server, and shutdown handling.
- `config.go`: `TRAE_API_*` environment configuration and validation.
- `server.go`, `session.go`, and `acp_client.go`: session lifecycle and ACP integration.
- `http.go` and `prompt.go`: OpenAI-compatible HTTP handlers and prompt conversion.
- `process_unix.go` / `process_windows.go`: OS-specific process behavior.
- `main_test.go`: unit and concurrency-oriented tests for the service.
- `README.md`: local setup, API examples, and operational warnings.

Keep new code in the root unless a feature clearly warrants a package; keep platform-specific behavior behind build-tagged files.

## Build, Test, and Development Commands

Run these from the repository root:

```bash
gofmt -w *.go       # Format Go sources
go test ./...       # Run the complete test suite
go test -race ./... # Check concurrency-sensitive code
go build -o trae-api .
go run .             # Start on 127.0.0.1:8723
```

The service requires an installed and authenticated `trae-cli`. Set `TRAE_API_WORKDIR` for a real project directory; use `TRAE_API_TOKEN` whenever binding beyond loopback. Treat the default `acp serve --yolo` arguments as trusted-local-only behavior.

## Coding Style & Naming Conventions

Follow standard Go formatting (`gofmt`) and idiomatic Go naming: exported identifiers use `PascalCase`, internal identifiers use `camelCase`, and acronyms remain capitalized (`HTTP`, `API`, `ACP`, `ID`). Prefer small functions, explicit error wrapping with context, and `context.Context` propagation. Use `slog` for service diagnostics rather than ad hoc output.

## Testing Guidelines

Use the standard `testing` package. Name tests `Test<Behavior>` and place them in `main_test.go` unless a new package is introduced. Cover configuration validation, HTTP behavior, session cleanup, and concurrent access; run `go test -race ./...` for changes involving session maps or goroutines.

## Commit & Pull Request Guidelines

No Git history or project-specific convention is available in this checkout. Use concise imperative commits, optionally scoped (for example, `fix: reap idle ACP sessions`). PRs should explain behavior changes, configuration or security impact, and testing performed. Include curl examples or screenshots when changing the HTTP API, and call out compatibility effects for Unix and Windows.

## Security & Configuration Tips

Do not expose a non-loopback listener without `TRAE_API_TOKEN`. Avoid committing tokens, local paths, generated binaries, or credentials. Review CLI permissions and network access before deploying the service outside a trusted local environment.
