# AGENTS.md

Guide for AI agents and developers maintaining `metrics_proxy`. Usage and
configuration for users are in `README.md`, runtime settings in `config.yaml`,
manual tests (real servers, Windows) in `TESTING.md`, and the threat model and
security invariants in `SECURITY.md`. `CLAUDE.md` includes this file.

## 1. Overview

`metrics_proxy` is an HTTP reverse proxy in Go (1.26+) between LLM clients and
one local LLM server (LM Studio, Ollama or another OpenAI-compatible server).
It forwards requests, inspects responses for inference metrics (model,
tokens, tokens/s, time to first token, duration), keeps aggregates in memory,
prints one line per query and optionally appends rows to a CSV file.

All code is in `package main`: `main.go`, `main_test.go` and `fuzz_test.go`.
The only dependency is `go.yaml.in/yaml/v4`.

```
main.go, main_test.go, fuzz_test.go
config.yaml            # Default config, with comments for every setting
testdata/              # Captured responses: lm_* (LM Studio), ol_* (Ollama)
testdata/fuzz/         # Fuzz inputs that found bugs, replayed by go test
.github/workflows/ci.yml, .github/dependabot.yml
.golangci.yml, sonar-project.properties
.gitattributes         # Keeps testdata/ byte-exact
```

## 2. Commands

```bash
go build -o metrics_proxy .
go run . [--config file.yaml] [--csv stats.csv]
go test -race ./...
GOARCH=386 go test ./...       # 32-bit build (token limits must fit an int)
gofmt -s -w .
go vet ./...
go mod tidy && go mod verify
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3 -over 15 .
go test -run '^$' -fuzz='^FuzzCaptureSplit$' -fuzztime=30s .   # one target per run
```

## 3. Testing

- Tests start a fake upstream and the proxy with `httptest`. `startProxy()`
  resets the global state (`cfg`, `metrics`, `proxy`, `out`, `csvLog`);
  shutdown tests run `serve()` with their own contexts via `startServe()`.
- Metrics are recorded on the proxy's goroutine, so never assert right after a
  request. `waitFor`/`waitForQueries` wait for metrics. `QueryCount` advances
  before the status line and CSV row, so wait for those with `waitForLog` and
  `waitForCSVRows` (which treats a half-written row as "not yet"). Debug
  checks wait for the status or `[Proxy]` line, which follows a request's last
  `[DEBUG]` line.
- Parser tests use the captures in `testdata/` (`readFixture()`) rather than
  hand-written JSON. `TestCaptureFixtures` feeds every capture whole and in
  1-byte chunks. New upstreams or endpoints need a new capture; see
  `TESTING.md` (requests to a real server need the user's approval first).
- Fuzz targets (`fuzz_test.go`): `FuzzCaptureSplit`, `FuzzEscapeControls`,
  `FuzzEscapeTruncated`, `FuzzCSVText`, `FuzzHostName`, `FuzzPathChecks`,
  `FuzzRequestTarget`. `go test` runs their seeds and `testdata/fuzz/`; commit
  a failing input with its fix. Huge inputs are out of the fuzzer's reach, so
  `TestCaptureOversizedInChunks` covers `maxCaptureBytes`.
- Keep the security checks (`hasPathTarget`, `allowedHost`, `hostName`,
  `hasDotDotSegment`, `cleanPath`, `isManagementEndpoint`, `offersUpgrade`,
  `refuseManagement`, `escapeControls`, `csvText`, `startsFormula`) and the
  captures and parsers at 100% coverage. Deliberately uncovered: `main()`,
  `setupSignalHandling` and the no-`requestInfo` branch in `meterResponse`.
- Under WSL2, a connection to a closed loopback port can hang instead of being
  refused, so `TestUpstreamUnavailable` and `TestLogEscapesControlCharacters`
  take ~30 s each. Tests waiting for a listener to close use
  `net.DialTimeout` and count a timeout as closed.

### Backlog

Remove an item once done.

1. **`testing/synctest`** for the shutdown and streaming tests instead of
   `waitFor` polling and real timeouts; `t.Context()` where a test needs no
   cancel of its own.
2. **Windows in CI**: a `windows-latest` job running `go test ./...` (no
   `-race`, no coverage upload), automating the path, CSV and
   refused-connection items of `TESTING.md`. Possibly `macos-latest` too.
3. **More captures**: OpenAI Responses API (`/v1/responses`), error bodies
   (Ollama `{"error": …}` as JSON and mid-stream), embeddings (`/api/embed`,
   `/v1/embeddings`; they count as queries, pin that in the test), tool-call
   and reasoning-model streams, legacy `/v1/completions`.
4. Optional: `BenchmarkSSECapture` with `b.Loop()` over
   `lm_v1_chat_stream.sse`, and a one-off mutation-testing run.

## 4. CI and tooling

- **CI** (`ci.yml`; pushes to `main`, pull requests, weekly, manual): gofmt,
  `go mod verify`, vet and `go test -race` with coverage on the Go version from
  `go.mod` and on `stable`; golangci-lint and govulncheck on `stable`. The
  `stable` test job uploads coverage to SonarQube Cloud, except for forks and
  Dependabot (no `SONAR_TOKEN`) and scheduled/manual runs. The `fuzz` job
  fuzzes each target for 30 s on scheduled and manual runs only and uploads
  failing inputs. Only superseded pull request runs are canceled. Actions are
  pinned by commit SHA.
- **Dependabot** updates Actions and Go modules weekly (7-day cooldown). It
  doesn't bump golangci-lint and govulncheck: change those by hand in
  `ci.yml`, this file and `README.md`.
- **SonarQube Cloud** (project `juusolatva_metrics_proxy`): Automatic Analysis
  must stay disabled, or the CI scan fails. Rules hit so far:
  - Cognitive complexity at most 15 per function, tests included (`go:S3776`);
    move loop bodies and checks into named helpers.
  - Defer a context's cancel in the function that creates it (`godre:S8188`).
  - `defer resp.Body.Close()` right after the error check (`godre:S8208`).
  - Derive contexts from an existing one (`go:S8239`). In `main`, `stop` is
    derived from `force` on purpose: the reverse would let the first signal
    skip the graceful wait.
- **golangci-lint** must report 0 issues. Discard deliberately ignored errors
  explicitly with a reason (`_ = f() // why`) instead of `//nolint`; the one
  exception is `<-errc` in `serve()`, which keeps `//nolint:errcheck`.
- **CodeQL** runs as GitHub's default setup (no workflow file).

## 5. Architecture

### Startup (`main`)

1. `parseFlags()`: `--config` (default `config.yaml`, relative to the working
   directory) and `--csv`. Positional arguments are an error. Document flags
   GNU-style (`--name`).
2. `loadConfig()` / `parseConfig()`: strict decoding (`decodeConfig()`: known
   fields, unique keys, a single document), then defaults, then
   `Config.validate()`, which returns the first problem. `forward_unmatched` is
   a `*bool` so unset means `true`; read it via `cfg.forwardsUnmatched()`.
   `TestParseConfigYAMLRules` pins the yaml behavior the config relies on; if
   a yaml upgrade breaks it, update the error example in `README.md` too.
3. `setupSignalHandling()`: the first `SIGINT`/`SIGTERM` cancels `stop`
   (graceful), the second `force`; a third uses Go's default handling. It runs
   before the probe below, so Ctrl+C can cancel that.
4. `listen()` binds the port before the banner, so a port in use fails early.
   Then `setupCSVLog()` opens the CSV (append, `0600`, header if empty), and
   `detectBackend(stop, …)` probes `/api/version`, `/api/v1/models`,
   `/v1/models` (2 s timeout, no redirects, checks for a known JSON key since
   LM Studio answers unknown paths with `200`). The probe only affects the
   banner.
5. `runProxy()` → `serve()`. The server has only `ReadHeaderTimeout` and
   `IdleTimeout`: generations can take minutes.

### Request routing (`handleRequest`)

Checks in order; refusals never contact the upstream and print a `[Proxy]`
line:

1. `hasPathTarget()`: targets that aren't a path (`*`, `a:x:/api/delete`,
   `http://host` without path) → `400`. The later checks only see `URL.Path`,
   while Go would send `URL.Opaque` upstream as is.
2. `allowedHost()` (DNS rebinding): accepts empty Host, IP literals,
   `localhost`, `localSuffixes`, `os.Hostname()` and `allowed_hosts`; else
   `403`. Needed in the proxy because `SetURL` replaces the Host header, so the
   upstream's own check always sees `localhost`.
3. `hasDotDotSegment()`: decoded `..` segments (`\` counts as separator) →
   `400`. The later checks use cleaned paths, but the path is forwarded as
   sent, so cleaning could hide a management prefix.
4. Unless `forward_management`, `refuseManagement()` → `403` for
   `managementEndpoints` (matched exactly or below, after `cleanPath()` and
   lowercasing) and for any `Upgrade` offer other than `h2c` (an upgraded
   connection is a tunnel whose later requests bypass every check). A
   remaining `h2c` offer is deleted so `ReverseProxy` can't tunnel.
5. `isAPIEndpoint()` matches the cleaned path (case kept) against
   `api_endpoints`. Unmatched paths are forwarded unmetered, or `404` when
   `forward_unmatched` is false.
6. A `requestInfo` (ID, start, method, path, `metered`) goes into the context
   and `proxy.ServeHTTP` runs.

### Forwarding (`newReverseProxy`)

- `Rewrite` sets the upstream URL, clears URL user info, sets `X-Forwarded-*`
  and drops `Accept-Encoding` so bodies arrive uncompressed for inspection.
- `upstreamTransport` (shared with `detectBackend()`): `Proxy: nil` (ignore
  `HTTP_PROXY`), `MaxIdleConnsPerHost: 16`, `MaxResponseHeaderBytes` 64 KiB.
- `FlushInterval: -1` so streamed tokens reach the client at once.
- `proxyError` logs `[ERROR]` and answers `502`.

### Metering (`meterResponse`, `meteredBody`)

- A `Content-Type` over `maxContentTypeBytes` (1 KiB) fails with `502`, also
  for unmetered requests: `ReverseProxy` parses it with `mime.ParseMediaType`
  after the hook, which allocates per parameter.
- Unmetered responses get a `not metered` line at header time and an
  unwrapped body. `101` upgrades are never metered.
- Metered bodies are wrapped in `meteredBody`, which copies each chunk into a
  `capture` chosen by `newCapture()`: `jsonCapture` (buffers, parses at EOF),
  `sseCapture` (event by event) or `ndjsonCapture` (line by line), the last two
  sharing a `lineSplitter`. Other content types aren't inspected.
- Bounds: `maxCaptureBytes` (16 MiB) per body, line or SSE event; oversized
  ones are forwarded but skipped (an oversized SSE event is skipped up to its
  blank line so its later data lines aren't parsed as a fragment). Buffers
  over `reuseLimit` (64 KiB) are released rather than reused.
- `finish()` runs once on EOF or `Close`: takes `MetricsData` from the
  capture, sets the duration, fills missing totals and TPS
  (`completeDerived()`), then replaces the TTFT with the measured one when the
  capture saw output (`firstOutput()`). The order matters: subtracting a
  measured TTFT first would give huge rates for streams held back on the way.
- **Measured TTFT**: request start to the first document with output
  (`llmPayload.hasOutput()`) read from the upstream, so it includes model
  loading but not slow client writes. It wins over a reported TTFT (the user
  wants loading visible). JSON bodies are never timed. New output fields go
  into `llmPayload`/`outputFields` and `TestPayloadHasOutput`. Array fields are
  decoded through `outputChoices`/`nonEmptyArray`, one element at a time, so
  millions of tiny elements can't allocate far beyond the capture limit; keep
  new array fields on that pattern.
- **Stream time** for derived TPS: from the first output (or first byte, when
  no output field is recognized) to the last data read, used only if the body
  arrived in bursts: at least `minStreamWaits` reads that waited `streamGap`
  (1 ms) or more *on the upstream* (timed inside `meteredBody.Read`, so slow
  clients and parsing don't count). Otherwise the request duration is used;
  buffered bodies would otherwise get rates in the millions.

### Parsing (`llmPayload`, `MetricsData.merge`)

- One struct covers OpenAI-compatible (`usage`, Ollama's `timings`), Ollama
  native (`eval_count`/`eval_duration` → TPS), LM Studio v0 (`stats`) and v1
  (`stats.*`, `model_instance_id`) payloads. `result` and `response` wrappers
  are merged by `mergeJSONBounded` (at most `maxWrapperDepth` hops and
  `maxPayloadParseBytes` per document, root included).
- Later SSE events/NDJSON lines override earlier fields. Non-objects are
  ignored; a field of the wrong type is skipped without losing the others.
- Implausible numbers count as unreported: tokens over `maxTokenCount` (2^29,
  so sums fit a 32-bit `int`), rates over `maxTokensPerSecond`, TTFT over
  `maxTTFTSeconds`.

### Recording (`recordQuery`)

- Responses without model, tokens or TPS are logged, not counted.
- `updateGlobalMetrics()` updates `metrics` under `metrics.mu` and returns a
  snapshot; `displayMetrics()` prints it without the lock.
- `csvRecorder.record()` (own mutex) encodes each row with a fresh
  `csv.Writer` and writes it in one `Write`, so one failed write doesn't stop
  later rows. After a partial write, `dropPartial()` truncates those bytes
  before the next row, and rows are refused until that succeeds. The file is
  used through the `csvFile` interface so tests can fake failures. Rows after
  `Close()` are dropped.

### Shutdown (`serve`)

`serve()` counts in-flight requests, then on `stop` calls `server.Shutdown`
(waits for running generations, bounded by `shutdownTimeout`, 60 s, and by
`force`). On timeout or force it calls `server.Close()`, which cuts upstream
requests (logged as `incomplete`), waits up to 2 s for them to record and
returns `nil`. `hijackNotifier` stops counting hijacked (WebSocket)
connections, which `Shutdown` doesn't wait for.

### Debug logging (`debugf`)

With `logging.debug`, startup prints `[DEBUG] config: …` and
`[DEBUG] backend probe …` lines. Per-request lines start with
`[DEBUG] req N:` and show the routing, upstream status and Content-Type, and
`meteredBody.debugSummary()`: how the body ended, the capture's `captureDiag`
counters, the measured TTFT, the TPS basis and hints such as `usageHint()`.
Never log bodies, headers other than Content-Type, query strings or URL user
info. New captures must implement `diag()` and `firstOutput()`.

## 6. Rules for changes

- **Standard library first**; keep a single module. Splitting `main.go` into
  more files in `package main` is fine.
- **Don't buffer response bodies** before forwarding: reading the body in
  `ModifyResponse` would hold back headers and break SSE
  (`TestProxyStreamsSSEWithoutBuffering`).
- **Locks**: hold `metrics.mu` when touching `metrics`; never hold it across
  network I/O.
- **Security invariants** in `SECURITY.md` must keep holding; add tests for
  every bypass you fix. The management list must be extended by hand when an
  upstream adds management endpoints.
- **Untrusted text** (paths, URLs, model names, upstream errors) goes through
  `escapeControls()` before printing to `out`; the status line's model name
  uses `escapeTruncated()`, whose work is bounded by the 30 runes it keeps.
  `debugf()` evaluates its arguments even when debug is off, so wrap calls
  that escape client text in `if cfg.Logging.Debug { … }`. CSV cells use
  `csvText()` against formula injection; keep it separate from terminal
  escaping and CSV quoting.
- **Config**: new options must be optional with defaults. Parsing is strict,
  so renaming or removing a key breaks files that use it; the project has a
  single user, so no aliases are needed, but note it in `README.md` and
  `config.yaml`.
- **CSV**: files are appended to without checking the header, so add new
  columns only at the end and document the change.
- **Go version**: don't use APIs newer than the `go.mod` directive. Bump only
  when a feature is needed, to the oldest supported release: `go mod edit
  -go=1.N`, `go fix ./...` (review it), then update §1 and `README.md`.
- **Tests**: every fix or feature gets an `httptest`-based test.
- **Git**: open a pull request from a branch rather than committing to `main`;
  don't commit build artifacts. Before committing a merge, run
  `git diff --check` and `go build ./... && go test -race ./...`.
- **Docs**: update this file, `README.md`, `config.yaml` comments and
  `TESTING.md` when behavior, configuration or workflows change. Keep tool
  versions in sync with `ci.yml` and the Go version with `go.mod`. Write
  durable facts, not change history.
