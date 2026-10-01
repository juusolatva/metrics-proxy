# AGENTS.md

This document serves as the guide for AI agents and developers working on the `metrics_proxy` codebase. It outlines the project's purpose, architecture, development workflows, configuration, conventions, and known limitations.

---

## 1. Project Overview

`metrics_proxy` is a lightweight HTTP reverse proxy written in Go (1.21+). It sits between an LLM client (such as an API client, IDE extension, or web UI) and a local LLM instance (primarily targeted at LM Studio, or compatible OpenAI-like HTTP backends).

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
└── README.md          # Minimal project overview
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
Currently, the repository does not contain test files (`*_test.go`). When implementing tests:
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

# API endpoint patterns to intercept
api_endpoints:
  - "/api/v1/*"

# Metrics collection and display settings
metrics:
  mode: "realtime"          # "realtime" or "periodic"
  refresh_interval_ms: 1000 # Snapshot refresh interval in milliseconds
  history_points: 10        # Sliding window capacity for metrics history

# Logging settings
logging:
  debug: false
```

### Configuration Notes
- `loadConfig()` hardcodes the relative path `"config.yaml"`. Changing directories before starting the process will cause a file read error.
- Several configuration fields (`metrics.mode`, `metrics.refresh_interval_ms`, and `logging.debug`) are defined in the structs and parsed from YAML, but do not yet have corresponding runtime behavior implemented in `main.go`.

---

## 5. Architecture & Internal Components

All application logic is currently contained in `main.go`:

### Global State
- `cfg` (`Config`): Global configuration parsed once at startup.
- `lmClient` (`*http.Client`): Reused HTTP client configured with a 30-second timeout.
- `metrics` (`Metrics`): Thread-safe in-memory metrics store protected by `metrics.mu` (`sync.RWMutex`).

### Execution Flow
1. **Startup (`main`)**:
   - Loads configuration via `loadConfig()`.
   - Initializes `lmClient`.
   - Starts signal listener goroutine in `setupSignalHandling()` (handles `SIGINT`, `SIGTERM`).
   - Starts HTTP server via `runProxy()`.
2. **Request Routing (`handleRequest`)**:
   - Validates incoming path against `cfg.APIEndpoints` using `isAPIEndpoint()`.
   - Returns `404 Not Found` if the path does not match any configured prefix.
   - Forwards request to upstream via `forwardToLMStudio()`.
3. **Upstream Forwarding & Metrics Extraction (`forwardToLMStudio`)**:
   - Reconstructs target URL (`http://<host>:<port><path>?<query>`).
   - Executes request via `lmClient.Do()`.
   - Reads response body and calls `parseResponse()`.
4. **Metrics Parsing (`parseResponse`)**:
   - Calculates request duration.
   - Uses zero-regex string scanners (`extractInt`, `extractFloat`, `extractString`) to inspect response JSON for keys:
     - `total_tokens` / `generated_tokens`
     - `tokens_per_second`
     - `model_name`
   - Calls `updateGlobalMetrics()`.
5. **State Update (`updateGlobalMetrics`)**:
   - Locks `metrics.mu`.
   - Increments `metrics.QueryCount`.
   - Updates `metrics.AvgTimePerQuery`.
   - Appends a new `MetricsHistoryPoint` to `metrics.History` (bounded to `cfg.Metrics.HistoryPoints`).
6. **Response & Display (`displayMetrics`)**:
   - Headers and status code are copied to client `http.ResponseWriter`.
   - Prints status line to stdout with current query count, model name, average duration, tokens, and TPS.

---

## 6. Critical Bugs & Architectural Gotchas

Agents working on this repository should be aware of several existing defects and design constraints in `main.go`:

1. **Unregistered HTTP Handler in `runProxy()`**:
   - `runProxy()` constructs an `http.Server` with `Addr: fmt.Sprintf(":%d", cfg.ProxyPort)` without setting `server.Handler = http.HandlerFunc(handleRequest)` or registering routes on `http.DefaultServeMux`. As a result, the server currently returns 404 for all incoming requests.
2. **Drained / Closed Response Body**:
   - In `forwardToLMStudio()`, `io.ReadAll(resp.Body)` reads the entire response body, and immediately calls `resp.Body.Close()`.
   - In `handleRequest()`, it subsequently attempts `io.Copy(w, resp.Body)` followed by `resp.Body.Close()`. Because `resp.Body` is already drained and closed, the proxy sends an empty body to the client.
3. **Invalid `http.Client.Do(req)` Usage**:
   - Passing an incoming `*http.Request` from a server handler directly to `http.Client.Do(req)` fails or produces errors in Go because `req.RequestURI` is set on server requests. Outgoing requests must be constructed anew (e.g., using `http.NewRequestWithContext`) or sanitized (`req.RequestURI = ""`, URL rewritten).
4. **Glob Pattern Mismatch in `isAPIEndpoint()`**:
   - `isAPIEndpoint()` checks `strings.HasPrefix(path, pattern)`. With `pattern = "/api/v1/*"`, requests to `/api/v1/chat/completions` fail to match due to the literal `*`. The pattern matching should strip the trailing `*` or use proper path matching.
5. **Wrong History Index in `displayMetrics()`**:
   - `displayMetrics()` accesses `metrics.History[0].TokensPerSec`. Because new history items are appended to the slice, index `0` represents the oldest recorded query, not the most recent one.
6. **Streaming / Server-Sent Events (SSE) Buffering**:
   - LLMs frequently stream completions via SSE (`text/event-stream`). The current design reads the entire response via `io.ReadAll`, blocking until generation completes. Supporting real-time streaming requires chunked reading or reverse-proxy teeing.

---

## 7. Guidelines for Modifying Code

- **Standard Library First**: Prefer Go standard library packages (`net/http`, `net/http/httputil`, `sync`, `time`, `encoding/json`) over introducing new dependencies.
- **Maintain Concurrency Safety**: Ensure `metrics.mu` write locks are held when updating state and read locks when reading state. Avoid holding locks across network I/O.
- **Preserve Configuration Compatibility**: Keep `config.yaml` schema backward-compatible when adding new features or configurations.
- **Testing**: Whenever fixing bugs or adding features, write corresponding unit tests using `net/http/httptest` to verify proxying, metrics parsing, and concurrent safety.
- **Git Hygiene**: Do not commit build artifacts (`metrics_proxy`, `.exe`) or temporary files.
