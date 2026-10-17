# metrics_proxy — core

HTTP reverse proxy between LLM clients and a local LLM server (LM Studio primary; Ollama and other OpenAI-compatible servers also targeted); extracts per-request inference metrics (tokens, TPS, duration, model) and prints them to stdout.

## Source map
- `main.go` — entire app (single `package main`): config (`loadConfig`, `parseConfig` with defaults), startup banner backend probe (`detectBackend`), server (`runProxy`, `serve` with graceful shutdown driven by `stop`/`force` contexts created in `main` (with `defer cancel()`) and cancelled by `setupSignalHandling(onFirst, onSecond)`, `handleRequest`), forwarding (`newReverseProxy`, `meterResponse`), body inspection (`meteredBody`, `newCapture` → `jsonCapture` / `sseCapture` / `ndjsonCapture`, streaming ones share `lineSplitter`), payload parsing (`llmPayload`, `MetricsData.merge`), aggregation (`updateGlobalMetrics`), terminal output (`displayMetrics`), CSV output (`--csv` flag → `openCSVLog` / `csvRecorder`, global `csvLog`).
- `main_test.go` — httptest tests; `startUpstream`/`startProxy` helpers reset globals.
- `testdata/` — real captured responses: `lm_*` LM Studio (native v1 + OpenAI-compat, JSON + SSE), `ol_*` Ollama (native JSON + NDJSON stream, OpenAI-compat SSE). Ground truth for parser tests.
- `config.yaml` — default runtime config; path set by `--config` (default `config.yaml`, relative to CWD).
- `AGENTS.md` (included by `CLAUDE.md`) — authoritative dev guide: architecture walkthrough (§5), config reference (§4), known limitations (§6), modification guidelines (§7). Read it instead of duplicating here.
- `.serena/` — committed: `project.yml` and `memories/` are shared and reviewed like code; `cache/` and `project.local.yml` are excluded by `.serena/.gitignore`.

## Invariants / gotchas
- Package-level globals: `cfg`, `proxy`, `metrics`, `out` (status-line writer, swapped in tests), `csvLog` (nil unless `--csv`; reset in `startProxy`).
- `--csv` is a command-line flag, not a `config.yaml` key. `recordQuery` writes one row per counted query (same filter as the status line). The `csvRecorder` has its own mutex and flushes every row. The header (`csvHeader`) is written only to an empty file. Unknown values are empty, not 0. Changing columns breaks appending to existing files: add new columns at the end.
- `metrics.mu` (RWMutex) guards all `Metrics` fields; never hold it across network I/O.
- Command-line flags: document and show them as GNU-style `--name` (user preference; `flagUsage` prints help that way). Go's `flag` package accepts both `-name` and `--name`, so no parsing change is needed.
- Config parsing is strict (`KnownFields(true)`): unknown or misspelled keys are errors with line numbers (`TestParseConfigRejectsUnknownKeys`). New options must be optional with defaults. Removing or renaming a key breaks files that use it; the user accepted that for unused settings (2026-10-17). Defaults: port 8080, upstream localhost:1234, `api_endpoints` `/api/v1/*` + `/v1/*` (also when empty), `history_points` 10. An empty file is valid.
- Parsed-but-unimplemented config: `logging.debug`. `metrics.mode` and `refresh_interval_ms` were removed and are now rejected as unknown keys.
- Flags are parsed by `parseFlags(args, output)` on its own `FlagSet` (`ContinueOnError`; returns `flag.ErrHelp` for `--help`). Flags: `--config`, `--csv`. Positional args are rejected (exit 2). Add new flags there and cover them in `TestParseFlags`.
- Response bodies are streamed via `httputil.ReverseProxy` and inspected through a `meteredBody` wrapper (set in `ModifyResponse`); metrics are recorded when the body ends, on the proxy goroutine — after or racing with the client finishing its read. Never read the body in `ModifyResponse` (blocks headers, breaks SSE).
- Upstream payload shapes differ per API: OpenAI-compat `/v1/*` (`model`, `usage.*`), LM Studio native v1 `/api/v1/*` (`model_instance_id`, `stats.{input_tokens,total_output_tokens,tokens_per_second,time_to_first_token_seconds}`; streamed `chat.end` wraps it in `result`), v0 (`usage` + `stats.time_to_first_token`). Source: lmstudio.ai/docs/developer/rest.
- Ollama shapes: native `/api/chat|generate` → `prompt_eval_count`, `eval_count`, `eval_duration` (ns), streamed as `application/x-ndjson` (final line `done:true` holds stats); OpenAI-compat SSE reports TPS only in `timings.predicted_per_second`. TPS = `eval_count / eval_duration`.
- `mergeJSON` tolerates `json.UnmarshalTypeError` (keeps other fields); `result`/`response` stay `json.RawMessage` (Ollama `/api/generate` `response` is a string).
- Config key `lm_studio` names the upstream for any server type; keep it (backward compat).
- Responses without model/tokens/TPS are not counted as queries.
- Shutdown: the first SIGINT/SIGTERM triggers `server.Shutdown`, waiting up to `shutdownTimeout` (60 s) for in-flight streams. A second signal or the timeout triggers `server.Close`, and cut-off queries are logged `incomplete`. Exit status is 0 either way. Test `serve` via the `startServe` helper with your own contexts, not real signals.
- TPS not reported by the upstream is derived in `completeDerived`, which sets `MetricsData.TPSEstimated`; the status line shows `(est.)`. LM Studio OpenAI-compat JSON has empty `stats: {}`, so its TPS is always estimated (badly understated for short replies).
- TTFT comes only from LM Studio native `/api/v1/*`; Ollama (native + OpenAI-compat) and LM Studio OpenAI-compat don't report it, so no `TTFT` field for them.
- LM Studio answers unknown endpoints with HTTP 200 + `{"error":...}` (Ollama: 404). Never identify the server by status code; `detectBackend` checks for a known JSON key per probe.
- Verified live on 2026-10-17 against LM Studio and Ollama (all native/OpenAI-compat endpoints, streamed and not).

## Further memories
- Language/deps/version pins: `mem:tech_stack`
- Build/run/test/lint commands: `mem:suggested_commands`
- Code style and design rules (stdlib-first, testing approach): `mem:conventions`
- Checks required before declaring a task done: `mem:task_completion`
