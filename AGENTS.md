# Repository Instructions

## Layout and Architecture

- This is a single Go 1.23 module and a single root `package main`; keep implementation and tests in the repository root.
- `main.go` wires configuration, HTTP routes, signal handling, and graceful shutdown. `config.go` is the source of truth for `TRAE_API_*` parsing and validation.
- `http.go` exposes the OpenAI-compatible API; `server.go` coordinates lifecycle; `session.go` owns one `trae-cli acp serve` process/session; `acp_client.go` routes ACP notifications.
- `process_pool.go` owns initialized, unassigned ACP processes. `session_manager.go` owns stable caller session IDs. A stable external session gets a dedicated process; an anonymous request gets a fresh process and is closed when the request ends. When `TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT` is non-zero, anonymous requests are instead routed through `acquireImplicitSession`: message-transcript fingerprints (`session.lastUserFP`/`lastFullFP`, see `fingerprintMessages`) detect conversation continuity so an anonymous client like VS Code Chat reuses a dedicated process; sessions idle-reap after the implicit timeout.
- Keep Unix/Windows process termination behavior in `process_unix.go` and `process_windows.go` respectively.

## Commands

Run from the repository root:

```bash
gofmt -w *.go
go test ./...
go test -race ./...
go build -o trae-api .
go run .
```

`trae-cli` must be installed and authenticated for the running service or integration test. The real CLI test is opt-in and makes model requests: `TRAE_API_RUN_INTEGRATION=1 go test -run TestTraeCLIAllowsConcurrentPromptsAcrossDedicatedProcesses ./...`.

## Operational Constraints

- The default listener is loopback (`127.0.0.1:8723`). Binding a non-loopback address requires `TRAE_API_TOKEN`; authentication applies to every route.
- `TRAE_API_WORKDIR` is required for real project-file access. If unset, startup creates a temporary isolation workspace that is removed on shutdown; do not infer project contents from it.
- The default ACP command includes `acp serve --yolo`, so treat the service as trusted-local-only unless permissions and workdir are explicitly reviewed.
- Stable sessions are in-memory and expire according to `TRAE_API_SESSION_IDLE_TIMEOUT` (default 720h), then are closed by the scan loop. Service restart loses all stable sessions.
- `TRAE_API_WARM_PROCESSES=0` disables startup warming but the first demand still creates a process. `TRAE_API_MAX_PROCESSES` can make acquisition wait rather than create another process.
