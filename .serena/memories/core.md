# metrics_proxy — core

HTTP reverse proxy between LLM clients and a local LM Studio (OpenAI-compatible) server; extracts per-request inference metrics (tokens, TPS, duration, model) and prints them to stdout.

## Source map
- `main.go` — entire app (single `package main`): config (`loadConfig`, `parseConfig` with defaults), server (`runProxy`, `handleRequest`), forwarding (`newReverseProxy`, `meterResponse`), body inspection (`meteredBody`, `jsonCapture`, `sseCapture`), payload parsing (`llmPayload`, `MetricsData.merge`), aggregation (`updateGlobalMetrics`), terminal output (`displayMetrics`).
- `main_test.go` — httptest tests; `startUpstream`/`startProxy` helpers reset globals.
- `config.yaml` — runtime config; read from CWD (hardcoded relative path).
- `AGENTS.md` (included by `CLAUDE.md`) — authoritative dev guide: architecture walkthrough (§5), config reference (§4), known limitations (§6), modification guidelines (§7). Read it instead of duplicating here.
- `.serena/` — committed: `project.yml` and `memories/` are shared and reviewed like code; `cache/` and `project.local.yml` are excluded by `.serena/.gitignore`.

## Invariants / gotchas
- Package-level globals: `cfg`, `proxy`, `metrics`, `out` (status-line writer, swapped in tests). `metrics.mu` (RWMutex) guards all `Metrics` fields; never hold it across network I/O.
- `config.yaml` schema must stay backward-compatible.
- Parsed-but-unimplemented config: `metrics.mode`, `metrics.refresh_interval_ms`, `logging.debug`.
- Response bodies are streamed via `httputil.ReverseProxy` and inspected through a `meteredBody` wrapper (set in `ModifyResponse`); metrics are recorded when the body ends, on the proxy goroutine — after or racing with the client finishing its read. Never read the body in `ModifyResponse` (blocks headers, breaks SSE).
- Upstream payload shapes differ per API: OpenAI-compat `/v1/*` (`model`, `usage.*`), LM Studio native v1 `/api/v1/*` (`model_instance_id`, `stats.{input_tokens,total_output_tokens,tokens_per_second,time_to_first_token_seconds}`; streamed `chat.end` wraps it in `result`), v0 (`usage` + `stats.time_to_first_token`). Source: lmstudio.ai/docs/developer/rest.
- Responses without model/tokens/TPS are not counted as queries.

## Further memories
- Language/deps/version pins: `mem:tech_stack`
- Build/run/test/lint commands: `mem:suggested_commands`
- Code style and design rules (stdlib-first, testing approach): `mem:conventions`
- Checks required before declaring a task done: `mem:task_completion`
