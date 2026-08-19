# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A single-file Go HTTP reverse proxy that sits in front of a local LM Studio instance, forwards
matching API requests to it, and parses metrics (tokens, tokens/sec, model name, timing) out of
the JSON responses to display in the terminal. Everything lives in `main.go`.

## Commands

```bash
go build -o metrics_proxy .   # build
go run .                      # run (reads ./config.yaml from cwd)
go vet ./...                  # static checks
gofmt -l .                    # formatting check (no test suite exists in this repo)
```

There are currently no `_test.go` files, so there is no `go test` suite to run.

## Configuration

`config.yaml` is read from the current working directory at startup (`loadConfig()` hardcodes the
path `"config.yaml"`, not a flag or env var). It is unmarshaled into the `Config` struct in
`main.go` via `go.yaml.in/yaml/v4`. Key fields:

- `proxy_port` — port the proxy listens on.
- `lm_studio.host` / `lm_studio.port` — upstream LM Studio address requests are forwarded to.
- `api_endpoints` — list of path prefixes (e.g. `/api/v1/*`, matched via `strings.HasPrefix`, so
  the trailing `*` is not a real glob) that are proxied; everything else 404s.
- `metrics.history_points` — bounds the ring buffer of recent `MetricsHistoryPoint`s kept in memory.

## Architecture

Request flow: `handleRequest` → `isAPIEndpoint` (prefix-matches `cfg.APIEndpoints`) →
`forwardToLMStudio` (rebuilds the URL against `cfg.LmStudio.Host:Port`, reuses the original method
and path, does a blocking `lmClient.Do`) → `parseResponse` (regex-free, hand-rolled string
scanning via `extractInt`/`extractFloat`/`extractString` looking for `total_tokens`,
`generated_tokens`, `tokens_per_second`, `model_name` keys) → `updateGlobalMetrics` (locks the
global `metrics` mutex, updates running average query time and appends to the bounded `History`
slice) → response body/headers are streamed back to the client → `displayMetrics` prints a
one-line terminal summary.

Global mutable state: `cfg` (loaded once at startup), `lmClient` (shared `http.Client`, 30s
timeout), and `metrics` (a `Metrics` struct guarded by its own `sync.RWMutex`) are package-level
vars shared across concurrent request goroutines.

Note: `runProxy()` constructs the `http.Server` without ever registering `handleRequest` (no
`http.HandleFunc`/`ServeMux` wiring), so the server currently serves `http.DefaultServeMux` with
no routes attached — keep this in mind if a change appears to have no effect when hitting the
proxy.
