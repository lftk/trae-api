# Repository Instructions

## Scope and architecture

- Single Go module, root package main; only OpenAI Chat Completions is implemented.
- TRAE-008 replaces the prior CLI, single-listener and memory-only-auth design. The following is the approved target; it does not assert that the migration or verification is complete.
- Use `github.com/urfave/cli/v3` (approved version v3.14.0) to declare commands, flags and actions. Remove the custom parser, command enum and argv dispatch; do not wrap a handwritten parser in the library. Keep `version` overridable using `-X main.version=`. Other new dependencies require separate approval.
- API and Auth are independent components with their own HTTP servers, routes and lifecycle. Default execution coordinates both in one process; `api` and `auth` can run independently, while `login` contacts Auth. Do not spawn internal CLI subprocesses or parse the command line more than once.
- Auth owns browser authorization, callback attempts, device identity, token renewal and the sole credential-file writer. Its callback listener is loopback-only; derive callback URLs from trusted bound-listener state, never inbound or forwarded headers. Credentials never go to callback-provided hosts.
- Store versioned credential state at `filepath.Join(os.UserConfigDir(), "trae-api", "auth.json")`. Auth creates a private application directory/file (0700/0600), restores its own valid state and publishes complete atomic updates. Preserve the last valid file on write failure; do not automatically overwrite unknown formats or follow unsafe file paths.
- API reads only the latest access-token snapshot and necessary UID/device metadata from that file. It must not own login attempts, refresh tokens, device private keys, browser actions or refresh calls. Missing, invalid or expired access credentials produce a clear authentication error; API never triggers renewal or replays a request to obtain fresh credentials.
- CLI login opens the official browser flow with an argv-only OS opener and waits for correlated completion. It never secretly starts a background service or prints credentials or credential-bearing callback URLs.
- Command configuration belongs to the CLI library, not parallel environment/argv parsers. Distinguish the gateway access Token from the stored Trae access token. API binding is independent of the callback listener; non-loopback API exposure requires an access Token.
- Keep the coordinator thin: cancel and await both components on a combined-run failure or shutdown. Request concurrency belongs to API, authorization concurrency belongs to Auth. Do not retain message history, client sessions, response snapshots or disk expiry cleanup. Every request supplies complete messages/tools and gets a fresh upstream request ID.
- `types.go` and `http.go` normalize and validate text/function-tool inputs and reuse official SDK Chat response types. Add only reasoning_content; do not reimplement standard Chat output schemas.
- `upstream.go` directly converts requests to the known CN IDE protocol and parses SSE including multiple tool identities and native argument fragments. Never execute caller tools or invent tool success.
- `/v1/responses` explicitly returns 501 unsupported. Do not add Responses adapters or continuation IDs.

## Go style and maintainability

- Apply these conventions to production Go code and tests; format all Go files with gofmt.
- Declare one struct field per line. Do not group names such as `Addr, APIToken string` in one field declaration.
- Write every nonempty struct literal across multiple lines, with one field per line and keyed fields where available. Apply this to named, anonymous, nested, pointer and generic structs, including elements whose types are inferred inside slices or maps. Empty literals such as `T{}` may remain on one line; maps and slices are not themselves struct literals.
- Write normal multiline statements and error checks rather than semicolon-compressed functions. Use clear local names, guard clauses and small cohesive functions; avoid unnecessary wrappers, interfaces or package fragmentation.
- Prefer small named types for stable protocol shapes and internal model metadata. Keep dynamic JSON and raw schemas where polymorphism or compatibility requires them; preserve absent/null fields, numeric/string error codes and accepted aliases.
- Reuse the standard library and existing SDK types. Preserve protocol values, serialization, error behavior, concurrency, lock order and retry boundaries during style-only refactors. Investigate ignored errors before changing them; report behavior-changing fixes separately.
- Separate mechanical formatting commits from structural refactors. Retain existing regression assertions and use type-aware struct-layout checks in addition to gofmt; do not infer struct violations from comma-based regular expressions.

## Validation

Run uncached `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, and `go build -o trae-api .`, plus the type-aware struct-layout checker. Test CLI dispatch, independent/combined component lifecycle, API read-only behavior, private atomic credential storage and Auth recovery using synthetic credentials, local mocks and injected browser openers. Isolate the user config directory; on macOS set a separate HOME rather than relying only on XDG_CONFIG_HOME. Never load real user credentials for tests.

## Safety

- Persistence is authorized only for the new fixed credential state managed by Auth. Do not read, overwrite, remove or migrate unrelated legacy credential/history/session files, backups or stashes. Do not persist conversations.
- Non-loopback API binding requires the gateway access Token. Keep Auth management protected, and validate local Host/origin where applicable to prevent DNS rebinding. API must not expose Auth login-management or callback routes.
- Only Auth's exact GET /authorize callback permits the official cross-site browser redirect without a gateway Token; require trusted loopback Host/peer and a current, unexpired one-use authorization attempt. Cancelled, superseded or expired in-flight exchanges cannot install or persist credentials.
- Keep credentials, full authorization callbacks and user message/tool contents out of logs. Browser opener errors must be sanitized; disable environment proxies for local CLI management calls and never follow redirects with gateway tokens.
- Renewal belongs to Auth alone and must serialize credential publication. API does not refresh or replay rejected requests. Do not replay network errors, business errors or observed output; prevent late callbacks or old-account refreshes from overwriting newer account state.
- Do not claim real login/upstream validation from offline tests. Protocol references remain in REFERENCES.md.
