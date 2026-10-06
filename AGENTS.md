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
├── testdata/          # Real responses captured from LM Studio (lm_*) and Ollama (ol_*)
│   ├── lm_v1_chat.json, lm_v1_chat_stream.sse   # LM Studio native /api/v1/chat
│   ├── lm_oai.json, lm_oai_stream.sse           # LM Studio OpenAI-compatible /v1/chat/completions
│   ├── ol_chat.json, ol_chat_stream.ndjson      # Ollama native /api/chat
│   └── ol_oai_stream.sse                        # Ollama OpenAI-compatible /v1/chat/completions
└── .serena/           # Serena project config + memories (committed); cache/ and project.local.yml are gitignored
```

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
  *Note: The application expects `config.yaml` to reside in the current working directory from which the command is executed.*

### Testing
Tests live in `main_test.go`. They start a fake upstream and the proxy with `httptest`, and reset the global state (`cfg`, `metrics`, `proxy`, `out`) per test via `startProxy()`. Metrics are recorded on the proxy's goroutine, so tests wait for them with `waitFor`/`waitForQueries` instead of asserting immediately.

Parser tests should use the captured payloads in `testdata/` rather than hand-written JSON where possible, and feed them both whole and in 1-byte chunks to exercise chunk boundaries (`TestCaptureFixtures`). When adding support for a new upstream or endpoint, capture a real response (JSON and streamed) into `testdata/` with a `<upstream>_<api>[_stream].<ext>` name.
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

Configuration is unmarshaled directly into the `Config` struct via `loadConfig()`.

```yaml
# Port the proxy server listens on
proxy_port: 8080

# Upstream LM Studio instance connection settings
lm_studio:
  host: "localhost"
  port: 1234

# API endpoint patterns to intercept (path prefixes; trailing "*" optional)
api_endpoints:
  - "/api/v1/*"  # LM Studio native REST API
  - "/v1/*"      # OpenAI-compatible API

# Metrics collection and display settings
metrics:
  mode: "realtime"          # "realtime" or "periodic"
  refresh_interval_ms: 1000 # Snapshot refresh interval in milliseconds
  history_points: 10        # Recent queries to average tokens/sec over

# Logging settings
logging:
  debug: false
```

### Configuration Notes
- `loadConfig()` hardcodes the relative path `"config.yaml"`. Changing directories before starting the process will cause a file read error.
- `parseConfig()` applies defaults for unset fields: `proxy_port` 8080, `lm_studio` `localhost:1234`, `history_points` 10.
- Several configuration fields (`metrics.mode`, `metrics.refresh_interval_ms`, and `logging.debug`) are defined in the structs and parsed from YAML, but do not yet have corresponding runtime behavior implemented in `main.go`.
- The `lm_studio` key names the upstream regardless of server type; to front Ollama, point it at `localhost:11434` and add `"/api/*"` (Ollama native `/api/chat`, `/api/generate`) to `api_endpoints`. Keep the key name for backward compatibility; an alias (e.g. `upstream`) would have to fall back to it.

---

## 5. Architecture & Internal Components

All application logic is currently contained in `main.go`:

### Global State
- `cfg` (`Config`): Global configuration parsed once at startup.
- `proxy` (`*httputil.ReverseProxy`): Reverse proxy to LM Studio, built by `newReverseProxy()`.
- `metrics` (`Metrics`): Thread-safe in-memory metrics store protected by `metrics.mu` (`sync.RWMutex`).
- `out` (`io.Writer`): Destination for per-request status lines (stdout; replaced in tests).

### Execution Flow
1. **Startup (`main`)**:
   - Loads configuration via `loadConfig()` / `parseConfig()`.
   - Builds `proxy` targeting `upstreamURL()`.
   - Starts signal listener goroutine in `setupSignalHandling()` (handles `SIGINT`, `SIGTERM`).
   - Starts HTTP server via `runProxy()` with `handleRequest` as handler. No read/write timeouts (generations can take minutes; an expiring `ReadTimeout` cancels the request context), only `ReadHeaderTimeout`/`IdleTimeout`.
2. **Request Routing (`handleRequest`)**:
   - Validates incoming path against `cfg.APIEndpoints` using `isAPIEndpoint()` (prefix match, trailing `*` stripped).
   - Returns `404 Not Found` if the path does not match any configured prefix.
   - Stores a `requestInfo` (start time, method, path) in the request context and calls `proxy.ServeHTTP`.
3. **Forwarding (`newReverseProxy`)**:
   - `Rewrite` points the request at the upstream, sets `X-Forwarded-*`, and drops `Accept-Encoding` so the transport decompresses bodies before they are inspected.
   - `FlushInterval: -1` flushes every write, so streamed tokens reach the client immediately.
   - `ErrorHandler` (`proxyError`) logs and returns `502 Bad Gateway`.
4. **Metrics Capture (`meterResponse` → `meteredBody`)**:
   - `ModifyResponse` wraps the upstream body in a `meteredBody` (except `101` upgrades). As the proxy copies the body to the client, each chunk is also written to a `capture` chosen by Content-Type (`newCapture`): `jsonCapture` (buffers, parses at end), `sseCapture` (`text/event-stream`, parses event by event) or `ndjsonCapture` (`application/x-ndjson`, Ollama native streaming, parses line by line). The streaming captures share a `lineSplitter`. Other content types are not inspected. Captures are bounded by `maxCaptureBytes` (16 MiB); oversized bodies/lines are proxied but not parsed.
   - On EOF or `Close` (e.g. client disconnect), `finish()` runs once: gets `MetricsData` from the capture, sets the duration, and fills totals/TPS the upstream didn't report (`completeDerived`; for streamed captures the first-to-last-byte time is used as generation time).
5. **Payload Parsing (`llmPayload`, `MetricsData.merge`)**:
   - `encoding/json` into a struct covering OpenAI-compatible responses (`model`, `usage.*`; Ollama adds `timings.predicted_per_second`), Ollama native (`prompt_eval_count`, `eval_count`, `eval_duration` in ns → TPS = `eval_count / eval_duration`), LM Studio API v0 (`usage`, `stats.tokens_per_second`, `stats.time_to_first_token`) and v1 (`model_instance_id`, `stats.input_tokens`, `stats.total_output_tokens`, `stats.tokens_per_second`, `stats.time_to_first_token_seconds`). Wrapped payloads in `result` (v1 `chat.end` SSE event) and `response` (OpenAI Responses API events) are merged recursively. Later SSE events / NDJSON lines override earlier fields. Non-object payloads are ignored; a field of unexpected type (`json.UnmarshalTypeError`) is skipped without discarding the others.
6. **State Update & Display (`recordQuery`)**:
   - Responses without inference data (no model, tokens or TPS, e.g. `/v1/models`, errors) are logged with a `[Proxy]` line and not counted.
   - Otherwise `updateGlobalMetrics()` locks `metrics.mu`, updates count, cumulative tokens, running average time, latest model/TPS, appends to `History` (newest last, bounded to `cfg.Metrics.HistoryPoints`), and returns a `metricsSnapshot` (incl. average TPS over `History`).
   - `displayMetrics()` prints one line per query from the snapshot without holding the lock.

---

## 6. Known Limitations & Gotchas

1. **Streamed OpenAI-compatible responses may lack token counts**:
   - OpenAI-style SSE chunks only carry `usage` when the client requests it (`"stream_options": {"include_usage": true}`). Without it, the query is recorded with its model and duration but no tokens or TPS.
2. **Derived TPS is an estimate**:
   - When the upstream reports no `tokens_per_second`, `completeDerived()` computes completion tokens / generation time. For non-streamed responses without a reported time to first token, that time includes prompt processing, so TPS is understated.
3. **Do not buffer response bodies before forwarding**:
   - Bodies must stay streamed through `meteredBody`; reading the body in `ModifyResponse` would also hold back response headers and break SSE. `TestProxyStreamsSSEWithoutBuffering` guards this.
4. **Unimplemented configuration**:
   - `metrics.mode`, `metrics.refresh_interval_ms` and `logging.debug` are parsed but have no effect.
5. **Abrupt shutdown**:
   - `setupSignalHandling()` calls `os.Exit(0)` immediately; in-flight requests are dropped.
6. **Go version**:
   - `go.mod` declares Go 1.21. Avoid standard library APIs newer than 1.21 (e.g. `http.ServeMux` method/wildcard patterns from 1.22) unless the `go` directive is bumped. The `min`/`max` builtins (1.21) are fine.
7. **Single upstream, no auth/TLS options**:
   - The upstream is always `http://host:port`; there is no HTTPS upstream, API key injection, or per-endpoint routing.

---

## 7. Guidelines for Modifying Code

- **Standard Library First**: Prefer Go standard library packages (`net/http`, `net/http/httputil`, `sync`, `time`, `encoding/json`) over introducing new dependencies.
- **Maintain Concurrency Safety**: Ensure `metrics.mu` write locks are held when updating state and read locks when reading state. Avoid holding locks across network I/O.
- **Preserve Configuration Compatibility**: Keep `config.yaml` schema backward-compatible when adding new features or configurations.
- **Testing**: Whenever fixing bugs or adding features, write corresponding unit tests using `net/http/httptest` to verify proxying, metrics parsing, and concurrent safety.
- **Git Hygiene**: Do not commit build artifacts (`metrics_proxy`, `.exe`) or temporary files.
- **Merges**: Before committing a merge, run `git diff --check` and `go build ./... && go test -race ./...`; conflict markers have been committed to `main` before (merge `6a83ff7`, fixed afterwards).
- **Keep docs in sync**: Behavior, config or limitation changes must update this file, `config.yaml` comments, and the affected `.serena/memories/` (committed, reviewed like code).
