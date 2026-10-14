# Repository Instructions

## Scope and architecture

- Single Go module, root package main; only OpenAI Chat Completions is implemented.
- `main.go` parses service flags and the `login` subcommand, starts the API and fixed authorization listener, and handles shutdown. Keep `version` overridable using `-X main.version=`.
- `config.go` loads only `TRAE_API_ADDR` and `TRAE_API_TOKEN`. All protocol constants/timeouts are internal defaults. Reject other TRAE_API_* variables; no credential or state directory option exists.
- `auth.go` owns one in-memory CN account, one-use browser authorization traces, bounded login results, and shared refresh. Credentials never go to a callback-provided host. Callback is fixed at 127.0.0.1:8765 in production; ephemeral test listeners are test seams only.
- `login_cli.go` talks to the running service, opens the official browser flow with an argv-only OS opener, waits for correlated completion, and cancels on timeout/interrupt. It never starts a background service and never prints credentials or a credential-bearing callback URL. `login_http.go` provides authenticated start/status/cancel management routes.
- `server.go` only owns account request concurrency and cancellation. Do not retain message history, client sessions, response snapshots, storage, restore, or disk expiry cleanup. Every request supplies complete messages/tools and gets a fresh upstream request ID.
- `types.go`, `openai_requests.go`, `openai_types.go`, `http.go` normalize and validate text/function-tool inputs and reuse official SDK Chat response types. Add only reasoning_content; do not reimplement standard Chat output schemas.
- `upstream.go` directly converts requests to the known CN IDE protocol and parses SSE including multiple tool identities and native argument fragments. Never execute caller tools or invent tool success.
- `/v1/responses` explicitly returns 501 unsupported. Do not add Responses adapters or continuation IDs.

## Validation

Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `go build -o trae-api .`. Tests use synthetic credentials, mocked upstreams and injected browser openers. No real-model opt-in test or credential-file loading is included.

## Safety

- Never read, overwrite, remove or migrate old credential/history files. No service persistence code may be added.
- Non-loopback API binding requires Token. Token protects all API and login management routes; all unprotected routes additionally validate literal local Host and same origin to prevent DNS rebinding.
- Fixed callback only accepts unexpired, current, one-use authorization attempts. Cancelled, superseded or expired in-flight exchanges cannot install credentials.
- Keep credentials, full authorization callbacks and user message/tool contents out of logs. Browser opener errors must be sanitized; disable environment proxies for local CLI management calls and never follow redirects with gateway tokens.
- Auth rejection before output permits one refresh/retry; do not replay network errors, business errors or observed output.
- Do not claim real login/upstream validation from offline tests. Protocol references remain in REFERENCES.md.
