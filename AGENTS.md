# AGENTS.md

This document serves as the guide for AI agents and developers working on the `metrics_proxy` codebase. It outlines the project's purpose, architecture, development workflows, configuration, conventions, and known limitations.

---

## 1. Project Overview

`metrics_proxy` is a lightweight HTTP reverse proxy written in Go (1.21+). It sits between an LLM client (such as an API client, IDE extension, or web UI) and a local LLM instance (primarily LM Studio; Ollama and other OpenAI-compatible HTTP backends are also targets, see `testdata/`).

Its primary responsibilities are:
- Forwarding matching API requests from clients to the local LLM server.
- Inspecting response payloads to extract inference performance metrics (token counts, tokens per second, query duration, model name).
- Maintaining aggregate query metrics in memory.
- Displaying real-time query statistics in the terminal.

The project currently consists of a single-file implementation (`main.go`) with minimal external dependencies (`go.yaml.in/yaml/v4`).

---

## 2. Repository Structure

```
metrics_proxy/
├── AGENTS.md          # Agent and developer guidelines (this file)
├── config.yaml        # Runtime YAML configuration file
├── go.mod             # Go module definition (metrics_proxy, Go 1.21)
├── go.sum             # Checksums for external dependencies
├── LICENSE            # MIT License
├── main.go            # Complete application source (proxy, metrics, config)
├── main_test.go       # httptest-based tests (proxying, streaming, parsing, concurrency)
├── README.md          # Minimal project overview
├── CLAUDE.md          # Includes this file (`@AGENTS.md`)
├── .gitattributes     # Keeps testdata/ byte-exact (no CRLF conversion)
├── .gitignore         # Build output, test binaries (go test -c), coverage.out
├── .golangci.yml      # golangci-lint v2 config (standard linters; errcheck off in tests)
├── sonar-project.properties  # SonarQube Cloud analysis settings (CI-based analysis, coverage)
├── .github/
│   ├── dependabot.yml       # Weekly update PRs for GitHub Actions (SHA pins) and Go modules
│   └── workflows/ci.yml     # CI: fmt, vet, tests + coverage, SonarQube Cloud, golangci-lint, govulncheck
├── docs/
│   └── windows-testing.md   # Checklist for testing on Windows 11
├── testdata/          # Real response bodies captured from LM Studio (lm_*) and Ollama (ol_*)
│   ├── lm_v1_chat.json, lm_v1_chat_stream.sse          # LM Studio native /api/v1/chat
│   ├── lm_oai.json, lm_oai_stream.sse                  # LM Studio /v1/chat/completions (stream with include_usage)
│   ├── lm_oai_stream_nousage.sse                       #   … streamed without include_usage
│   ├── ol_chat.json, ol_chat_stream.ndjson             # Ollama native /api/chat
│   ├── ol_generate.json, ol_generate_stream.ndjson     # Ollama native /api/generate
│   ├── ol_oai.json, ol_oai_stream.sse                  # Ollama /v1/chat/completions (stream with include_usage)
│   └── ol_oai_stream_nousage.sse                       #   … streamed without include_usage
└── .serena/           # Serena project config + memories (committed); cache/ and project.local.yml are gitignored
```

The current captures were taken on 2026-10-17. All of them used the prompt `Count from 1 to 5, separated by spaces.`, sent directly to the server (not through the proxy). Only response bodies were saved.
- **LM Studio:** `qwen/qwen2.5-coder-14b` with speculative decoding (draft model Qwen2.5 Coder 0.5B). Its OpenAI-compatible `stats` contain only the draft counters (`draft_model`, `*_draft_tokens_count`) and no rate.
- **Ollama:** `qwen3:4b-instruct-2507-q8_0`.

---

## 3. Development Workflow & Commands

### Build & Run
- **Build binary**:
  ```bash
  go build -o metrics_proxy .
  ```
- **Run directly**:
  ```bash
  go run .
  ```
  *Note: Without `--config`, the application reads `config.yaml` from the current working directory.*
- **Use another config file**:
  ```bash
  go run . --config /path/to/ollama.yaml
  ```
- **Save per-query statistics to CSV** (command-line flag, not a `config.yaml` setting):
  ```bash
  go run . --csv stats.csv
  ```

### Testing
Tests live in `main_test.go`. They start a fake upstream and the proxy with `httptest`, and reset the global state (`cfg`, `metrics`, `proxy`, `out`, `csvLog`) per test via `startProxy()`. Graceful-shutdown tests run the proxy through `serve()` with their own contexts via `startServe()`. Metrics are recorded on the proxy's goroutine, so tests wait for them with `waitFor`/`waitForQueries` instead of asserting immediately.

Parser tests should use the captured payloads in `testdata/` (read with `readFixture()`) rather than hand-written JSON where possible. `TestCaptureFixtures` feeds every capture whole and in 1-byte chunks, to exercise chunk boundaries, and checks the model, tokens, TPS, TTFT and that no document was invalid. `TestProxyOllamaNativeNDJSON` sends a capture end to end through the proxy. When adding support for a new upstream or endpoint, capture a real response (JSON and streamed) with a `<upstream>_<api>[_stream][_variant].<ext>` name, using `curl -sN -o testdata/<file>` directly against the server, and add it to `TestCaptureFixtures`. Captures are committed; check them for anything private before adding them.

Testing on Windows: see `docs/windows-testing.md`.
- **Run tests**:
  ```bash
  go test -v ./...
  ```
- **Run tests with race detection**:
  ```bash
  go test -race -v ./...
  ```

### Linting & Formatting
- **Format code**:
  ```bash
  gofmt -s -w .
  ```
- **Check formatting without modifying**:
  ```bash
  gofmt -l .
  ```
- **Static analysis**:
  ```bash
  go vet ./...
  ```
- **Lint** (same version as CI, config in `.golangci.yml`):
  ```bash
  go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run
  ```
- **Vulnerability check** (same version as CI):
  ```bash
  go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
  ```
- **CI** (`.github/workflows/ci.yml`, on pushes to `main`, on pull requests, weekly on Mondays and manually via `workflow_dispatch`): `gofmt -s -l`, `go mod verify`, `go vet` and `go test -race` with coverage on the Go version from `go.mod` and on `stable`; the `stable` run uploads `coverage.out` to SonarQube Cloud on pushes and on pull requests from this repository, except Dependabot's (forks and Dependabot get no `SONAR_TOKEN`; scheduled runs have no new code). golangci-lint and govulncheck run on `stable`. Jobs have `timeout-minutes`, and checkouts use `persist-credentials: false`. Actions are pinned by commit SHA with a version comment.
- **Dependabot** (`.github/dependabot.yml`): weekly PRs for GitHub Actions (grouped) and Go modules, skipping releases younger than 7 days (`cooldown`). It doesn't update the golangci-lint and govulncheck versions in `ci.yml`; bump those by hand, together with the commands in this file, README.md and `.serena/memories/`.
- **CodeQL** runs as GitHub's default setup (a repository setting, not a workflow file).

### Dependencies
- **Tidy dependencies**:
  ```bash
  go mod tidy
  ```
- **Verify dependencies**:
  ```bash
  go mod verify
  ```

---

## 4. Configuration Reference (`config.yaml`)

Configuration is read from the file given by `--config` (default `config.yaml`) and unmarshaled into the `Config` struct via `loadConfig(path)` / `parseConfig()`.

```yaml
# Port the proxy server listens on
proxy_port: 11436

# Upstream LLM server (LM Studio, Ollama or OpenAI-compatible)
server:
  host: "localhost"
  port: 1234

# API endpoint patterns to meter (path prefixes; trailing "*" optional)
api_endpoints:
  - "/api/v1/*"  # LM Studio native REST API
  - "/v1/*"      # OpenAI-compatible API

# Forward other paths unmetered (false: answer 404 without forwarding)
forward_unmatched: true

# Metrics settings
metrics:
  history_points: 10        # Recent queries to average tokens/sec over

# Logging settings
logging:
  debug: false              # Print [DEBUG] lines explaining each request's metrics
```

### Configuration Notes
- The config path comes from `--config` (`parseFlags()`), default `config.yaml`. Relative paths are resolved against the working directory, not the binary's location. A missing, unreadable or invalid file exits with status 1 and an error naming the file. Positional arguments (e.g. `metrics_proxy my.yaml`) are rejected with exit status 2 instead of being ignored.
- `parseConfig()` applies defaults for unset fields: `proxy_port` 11436, `server` `localhost:1234`, `history_points` 10, and `api_endpoints` `["/api/v1/*", "/v1/*"]` (also when given as an empty list). `forward_unmatched` is a `*bool` so that unset means `true`; read it via `cfg.forwardsUnmatched()`, which also treats the `nil` of test-built `Config`s as `true`. The port was 8080 until 2026-10-17; it moved to stay clear of 8080 (llama.cpp's `llama-server` and many others), Ollama's 11434 and a second Ollama instance's 11435. An empty config file is valid (`io.EOF` from the loader is treated as "no settings").
- `parseConfig()` decodes strictly via `decodeConfig()` (`yaml.NewLoader` with `yaml.WithKnownFields()` and `yaml.WithUniqueKeys()`; not the legacy `yaml.NewDecoder` shim): unknown keys, i.e. typos or removed settings, fail with `field <key> not found in type main.<Struct>` and the line number (`TestParseConfigRejectsUnknownKeys`). Duplicate keys fail too. A second YAML document (after `---`) fails with `line N: a second YAML document starts here`, and content after a `...` end marker with a syntax error; until 2026-10-22 both were silently dropped. `metrics.mode` and `metrics.refresh_interval_ms` were removed (never implemented), so old files containing them now fail to load until the keys are deleted. Removing or renaming a key has the same effect, so mention it in README.md.
- The yaml module is a pre-release, and its error wording changed between RCs (rc.6: type errors on one line joined by `; `, syntax errors as `go-yaml load error in <stage> … at L<line>.C<column>`). `TestParseConfigYAMLRules` pins what the config relies on (YAML 1.1 booleans such as `no`/`on`, duplicate keys, single document, error positions); when a yaml bump fails it, update README.md's error examples along with the test.
- `logging.debug` enables `[DEBUG]` output (see §5, "Debug Logging").
- The `server` key (`Config.Server`, `ServerConfig`) names the upstream regardless of server type; to front Ollama, point it at `localhost:11434` and add `"/api/*"` (Ollama native `/api/chat`, `/api/generate`) to `api_endpoints`. It was called `lm_studio` until 2026-10-17 and was renamed without an alias, since the project has a single user; old files fail with `field lm_studio not found`.

---

## 5. Architecture & Internal Components

All application logic is currently contained in `main.go`:

### Global State
- `cfg` (`Config`): Global configuration parsed once at startup.
- `proxy` (`*httputil.ReverseProxy`): Reverse proxy to the upstream server, built by `newReverseProxy()`.
- `metrics` (`Metrics`): Thread-safe in-memory metrics store protected by `metrics.mu` (`sync.RWMutex`).
- `out` (`io.Writer`): Destination for per-request status lines (stdout; replaced in tests).
- `csvLog` (`*csvRecorder`): Per-query CSV output opened by `openCSVLog()` when `--csv` is given; `nil` when disabled (reset by `startProxy()` in tests).

### Execution Flow
1. **Startup (`main`)**:
   - Parses command-line flags with `parseFlags()` (own `flag.FlagSet`, `ContinueOnError`, testable): `--config <file>` and `--csv <file>`. Document and show flags GNU-style (`--name`). `flagUsage()` prints `--help` that way, and the `flag` package also accepts the single-dash form.
   - Loads configuration via `loadConfig(opts.configPath)` / `parseConfig()` and prints which file was loaded.
   - With `--csv`, opens the file via `openCSVLog()`: append mode, created if missing, header (`csvHeader`) written only when the file is empty. An open error exits with status 1. The file is closed (deferred) after `runProxy()` returns.
   - Builds `proxy` targeting `upstreamURL()`.
   - Names the upstream in the startup banner via `detectBackend()`: probes `/api/version` (Ollama), `/api/v1/models` (LM Studio), `/v1/models` (OpenAI-compatible) with a 2 s timeout, checking the JSON body for a known key because LM Studio answers unknown endpoints with `200` and an error body. Falls back to "upstream … (server type not detected)" and stops probing on the first connection error. Only affects the banner.
   - `main` creates two cancelable contexts, `stop` and `force`, with `defer cancel()`. `setupSignalHandling(stopNow, forceNow)` calls the first cancel on the first `SIGINT`/`SIGTERM` and the second on the next. Keep context creation and `defer cancel()` in `main`: SonarCloud rule `godre:S8188` flags cancel functions that aren't deferred where they're created.
   - `runProxy()` listens on `cfg.ProxyPort` and hands the `http.Server` (handler `handleRequest`) to `serve()`. No read/write timeouts (generations can take minutes; an expiring `ReadTimeout` cancels the request context), only `ReadHeaderTimeout`/`IdleTimeout`.
2. **Request Routing (`handleRequest`)**:
   - Validates incoming path against `cfg.APIEndpoints` using `isAPIEndpoint()` (prefix match, trailing `*` stripped).
   - Paths that match are metered. Others are forwarded unmetered when `cfg.forwardsUnmatched()` (default), otherwise answered with `404 Not Found` and a `[Proxy] Non-API request` line, without contacting the upstream.
   - Stores a `requestInfo` (request ID, start time, method, path, `metered`) in the request context and calls `proxy.ServeHTTP`.
3. **Forwarding (`newReverseProxy`)**:
   - `Rewrite` points the request at the upstream, sets `X-Forwarded-*`, and drops `Accept-Encoding` so the transport decompresses bodies before they are inspected.
   - `FlushInterval: -1` flushes every write, so streamed tokens reach the client immediately.
   - `ErrorHandler` (`proxyError`) logs an `[ERROR]` line and returns `502 Bad Gateway` ("Failed to connect to the upstream LLM server").
4. **Metrics Capture (`meterResponse` → `meteredBody`)**:
   - For unmetered requests (`requestInfo.metered` false), `meterResponse` only prints `[Proxy] METHOD PATH STATUS (Ns, not metered)` when the headers arrive (time to headers, not the body) and leaves the body unwrapped; nothing is counted or written to CSV.
   - Otherwise `ModifyResponse` wraps the upstream body in a `meteredBody` (except `101` upgrades). As the proxy copies the body to the client, each chunk is also written to a `capture` chosen by Content-Type (`newCapture`): `jsonCapture` (buffers, parses at end), `sseCapture` (`text/event-stream`, parses event by event) or `ndjsonCapture` (`application/x-ndjson`, Ollama native streaming, parses line by line). The streaming captures share a `lineSplitter`. Other content types are not inspected. Captures are bounded by `maxCaptureBytes` (16 MiB); oversized bodies/lines are proxied but not parsed.
   - On EOF or `Close` (e.g. client disconnect), `finish()` runs once: gets `MetricsData` from the capture, sets the duration, and fills totals/TPS the upstream didn't report (`completeDerived`; for streamed captures the first-to-last-byte time is used as generation time). A derived TPS sets `MetricsData.TPSEstimated`, and the status line shows it as `TPS: 40.98 (est.)`.
5. **Payload Parsing (`llmPayload`, `MetricsData.merge`)**:
   - `encoding/json` into a struct covering OpenAI-compatible responses (`model`, `usage.*`; Ollama adds `timings.predicted_per_second`), Ollama native (`prompt_eval_count`, `eval_count`, `eval_duration` in ns → TPS = `eval_count / eval_duration`), LM Studio API v0 (`usage`, `stats.tokens_per_second`, `stats.time_to_first_token`) and v1 (`model_instance_id`, `stats.input_tokens`, `stats.total_output_tokens`, `stats.tokens_per_second`, `stats.time_to_first_token_seconds`). Wrapped payloads in `result` (v1 `chat.end` SSE event) and `response` (OpenAI Responses API events) are merged recursively. Later SSE events / NDJSON lines override earlier fields. Non-object payloads are ignored; a field of unexpected type (`json.UnmarshalTypeError`) is skipped without discarding the others.
6. **State Update & Display (`recordQuery`)**:
   - Responses without inference data (no model, tokens or TPS, e.g. `/v1/models`, errors) are logged with a `[Proxy]` line and not counted.
   - Otherwise `updateGlobalMetrics()` locks `metrics.mu`, updates count, cumulative tokens, running average time, latest model/TPS, appends to `History` (newest last, bounded to `cfg.Metrics.HistoryPoints`), and returns a `metricsSnapshot` (incl. average TPS over `History`).
   - `displayMetrics()` prints one line per query from the snapshot without holding the lock.
   - If `csvLog` is set, `csvRecorder.record()` appends the query as a CSV row and flushes it. The writer is guarded by its own mutex, not `metrics.mu`. Columns: `timestamp` (RFC 3339, ms, local zone), `method`, `path`, `status`, `model`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `tokens_per_second`, `tps_estimated`, `ttft_seconds`, `duration_seconds`, `complete`. Unknown tokens, TPS and TTFT are left empty, not written as `0`. Rows are per-query values only; aggregates (`Avg`, running totals) are not written. Write errors are logged as `[ERROR] writing CSV` and don't affect proxying. Rows arriving after `Close()` (requests cut off by shutdown) are dropped.
7. **Shutdown (`serve`)**:
   - `serve()` wraps the handler with an in-flight request counter and runs `server.Serve` until `stop` is done.
   - It then calls `server.Shutdown`, which closes the listener and waits for in-flight requests, streamed generations included, to finish normally and be recorded. The wait is bounded by `shutdownTimeout` (60 s, below systemd's default 90 s stop timeout) and aborted early by `force`.
   - On timeout or force it calls `server.Close()`. That cancels the upstream requests, so their bodies end without EOF and are logged as `incomplete`. It waits up to 2 s for those handlers to record, then prints `Proxy stopped` and returns `nil` (exit status 0).
   - Upgraded (`101`, hijacked) connections are not tracked by `Shutdown` and are dropped.
8. **Debug Logging (`debugf`, `logging.debug`)**:
   - `debugf()` prints `[DEBUG] …` lines to `out` only when `cfg.Logging.Debug` is set. Each forwarded request gets an ID from `requestSeq` (`requestInfo.id`), and its lines start with `req N:`.
   - Startup: the effective config after defaults, and each `detectBackend()` probe's result (status, non-JSON body, missing or found key, connection error).
   - `handleRequest`: method, request URI, client address and whether the path is metered.
   - `meterResponse`: upstream URL, status, time to response headers, Content-Type, and whether the body is inspected (not for unknown Content-Types or `101` upgrades).
   - `meteredBody.debugSummary()` (from `finish()`): whether the body reached EOF, its byte count and stream time; the capture's `captureDiag` (parser name; JSON documents parsed, invalid, or skipped for exceeding `maxCaptureBytes`); the basis of an estimated TPS (returned by `completeDerived()`); and hints when nothing counts as inference, or when an SSE stream has no usage (the `include_usage` hint).
   - The counters live in each capture (`captureDiag.count()` from `mergeJSON`'s return value, `lineSplitter.oversized`). `mergeJSON` returns `false` for non-objects; a type error on the whole document (`UnmarshalTypeError.Field == ""`) counts as invalid, a field-level one doesn't. When adding a capture, implement `diag()`.

---

## 6. Known Limitations & Gotchas

1. **Streamed OpenAI-compatible responses may lack token counts**:
   - OpenAI-style SSE chunks only carry `usage` when the client requests it (`"stream_options": {"include_usage": true}`). Without it, the query is recorded with its model and duration but no tokens or TPS.
2. **Derived TPS is an estimate**:
   - When the upstream reports no `tokens_per_second`, `completeDerived()` computes completion tokens / generation time. For non-streamed responses without a reported time to first token, that time includes prompt processing, so TPS is understated.
   - Estimated values are marked `(est.)` in the status line but still count towards the `Avg … tok/s` over `History`.
   - The error grows as replies get shorter, since request overhead dominates: in a live test, a 6-token non-streamed LM Studio `/v1/chat/completions` reply showed ~41 tok/s against a real ~250. LM Studio's OpenAI-compatible API (`/v1/*`) never reports a rate, streamed or not: `stats` is empty, or with speculative decoding holds only the draft counters. So its TPS is always estimated; for streams the stream time is used, which is closer.
3. **Time to first token is only shown when the upstream reports it**:
   - Only the LM Studio native API (`/api/v1/*`) reports TTFT. Ollama (native and OpenAI-compatible) and LM Studio's OpenAI-compatible API don't, so their status lines have no `TTFT` field. For streamed responses it could be measured by the proxy (request start to first byte), which is not implemented.
4. **Do not buffer response bodies before forwarding**:
   - Bodies must stay streamed through `meteredBody`; reading the body in `ModifyResponse` would also hold back response headers and break SSE. `TestProxyStreamsSSEWithoutBuffering` guards this.
5. **Debug logging is config-only and unstructured**:
   - It is enabled only by `logging.debug: true` (there is no `--debug` flag). `[DEBUG]` lines go to `out` (stdout), interleaved with the status lines, and lines of concurrent requests interleave too (match them by `req N`).
   - Bodies and headers other than Content-Type are never logged, only sizes and counts. Client addresses and upstream URLs are logged.
6. **Shutdown waits for in-flight requests**:
   - The first signal waits up to `shutdownTimeout` (60 s, a constant, not configurable) for running generations; a second signal aborts them. Tests drive `serve()` directly with their own contexts and timeout (`startServe`), not real signals. Aggregates are in memory only and are lost on exit; with `--csv`, per-query rows are kept in the file.
   - `--csv` does not check an existing file's header. Appending to a CSV with different columns (e.g. from a future version) mixes formats.
7. **Go version**:
   - `go.mod` declares Go 1.21. Avoid standard library APIs newer than 1.21 (e.g. `http.ServeMux` method/wildcard patterns from 1.22) unless the `go` directive is bumped. The `min`/`max` builtins (1.21) are fine.
8. **Single upstream, no auth/TLS options**:
   - The upstream is always `http://host:port`; there is no HTTPS upstream, API key injection, or per-endpoint routing.
9. **Not yet tested on Windows**:
   - The binary and tests cross-compile (`GOOS=windows go build ./... && GOOS=windows go vet ./... && GOOS=windows go test -c -o /dev/null .`), but nothing has been run on Windows. `docs/windows-testing.md` lists what to check (Ctrl+C handling, Excel's CSV lock and separators, paths, firewall and reserved ports, slow refused connections in two tests).

---

## 7. Guidelines for Modifying Code

- **Standard Library First**: Prefer Go standard library packages (`net/http`, `net/http/httputil`, `sync`, `time`, `encoding/json`) over introducing new dependencies.
- **Maintain Concurrency Safety**: Ensure `metrics.mu` write locks are held when updating state and read locks when reading state. Avoid holding locks across network I/O.
- **Configuration Compatibility**: New options must be optional with sane defaults, so existing files keep working. Config parsing is strict: removing or renaming a key makes files that still use it fail with an "unknown field" error. The project has a single user so far, so renames and removals don't need aliases or deprecation periods (as with `metrics.mode` and `lm_studio` → `server`); note them in README.md and `config.yaml` comments.
- **Testing**: Whenever fixing bugs or adding features, write corresponding unit tests using `net/http/httptest` to verify proxying, metrics parsing, and concurrent safety.
- **SonarCloud**: Analyzed by the CI workflow (project `juusolatva_metrics_proxy`, public; settings in `sonar-project.properties`, token in the `SONAR_TOKEN` repository secret). Automatic Analysis must stay disabled in SonarQube Cloud, or the CI scan fails. Pull requests from forks and Dependabot skip the scan (no token). Rules hit so far:
  - Keep each function's cognitive complexity at 15 or less, tests included (`go:S3776`). Move loop bodies, handlers and checks into named helpers. Check locally with `go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3 -over 15 .`, which scores function literals somewhat higher than Sonar.
  - Defer a context's cancel function in the function that creates it (`godre:S8188`).
  - Close response bodies with `defer resp.Body.Close()` right after the error check (`godre:S8208`).
- **golangci-lint**: Must report `0 issues` (CI fails otherwise; config in `.golangci.yml`). `errcheck` is on for non-test code, apart from the `std-error-handling` preset (unchecked `Close`, `fmt.Fprint*` etc.). Where an error is deliberately ignored, discard it explicitly with a reason, e.g. `_ = r.w.Write(row) // write errors are sticky; returned by r.w.Error()`, rather than adding `//nolint`. Exception: a discarded receive from a `chan error` (`<-errc` in `serve()`) is flagged by errcheck, and `_ = <-errc` by staticcheck S1005, so it keeps `//nolint:errcheck` with a reason.
- **Git Hygiene**: Do not commit build artifacts (`metrics_proxy`, `.exe`) or temporary files.
- **Merges**: Before committing a merge, run `git diff --check` and `go build ./... && go test -race ./...`; conflict markers have been committed to `main` before (merge `6a83ff7`, fixed afterwards).
- **Keep docs in sync**: Behavior, config or limitation changes must update this file, `README.md`, `config.yaml` comments, and the affected `.serena/memories/` (committed, reviewed like code).
