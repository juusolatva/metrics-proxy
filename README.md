# metrics proxy

A lightweight HTTP reverse proxy that sits between an LLM client and a local
LLM server and prints inference metrics for every query: model, token counts,
tokens per second, time to first token and duration, plus running aggregates.

Supported upstreams:

- **LM Studio**: native REST API (`/api/v1/...`) and OpenAI-compatible API (`/v1/...`)
- **Ollama**: native API (`/api/chat`, `/api/generate`) and OpenAI-compatible API (`/v1/...`)
- Other OpenAI-compatible HTTP servers

Both regular JSON responses and streamed responses (SSE and NDJSON) are
supported. Streams are passed through to the client as they arrive and are
never buffered.

## Requirements

- Go 1.21 or newer

## Usage

```bash
go build -o metrics_proxy .
./metrics_proxy
```

The proxy reads `config.yaml` from the current working directory, so start it
from the repository root (or wherever your `config.yaml` is).

Point your client at the proxy instead of the LLM server, e.g.
`http://localhost:8080/v1` instead of `http://localhost:1234/v1`.

Each completed query prints a line like this:

```
[14:02:31] #3 POST /api/v1/chat 200 | Model: qwen3-8b | Tokens: 42 in / 311 out | TPS: 48.12 | TTFT: 0.21s | Time: 6.68s | Avg: 5.90s @ 47.80 tok/s | Total tokens: 1024
```

`Avg` is the average query duration over all queries and the average tokens
per second over the last `history_points` queries. Requests without inference
data (such as `/v1/models` or errors) are logged as `[Proxy]` lines and not
counted.

`TPS` is the rate reported by the server. When the server doesn't report one,
the proxy estimates it and marks it `(est.)`, e.g. `TPS: 40.98 (est.)`. `TTFT`
(time to first token) only appears when the server reports it (see
[Limitations](#limitations)).

On startup the proxy probes the upstream and names it in the banner
(`Forwarding to Ollama at localhost:11434`). If the server is unreachable or
not recognized, it prints `upstream … (server type not detected)` and proxies
as usual.

### Stopping

Press Ctrl+C (or send `SIGTERM`). The proxy stops accepting connections and
waits up to 60 seconds for in-flight requests, including streamed
generations, to finish and be recorded. Press Ctrl+C again to stop at once.
Requests cut off this way are logged as `incomplete`.

## Configuration

`config.yaml`:

```yaml
proxy_port: 8080          # port the proxy listens on

lm_studio:                # upstream LLM server (any supported server type)
  host: "localhost"
  port: 1234

api_endpoints:            # path prefixes to forward; others get 404
  - "/api/v1/*"           # LM Studio native REST API
  - "/v1/*"               # OpenAI-compatible API

metrics:
  history_points: 10      # recent queries to average tokens/sec over
```

To front Ollama, set the upstream to `localhost:11434` and add `"/api/*"` to
`api_endpoints`.

`metrics.mode`, `metrics.refresh_interval_ms` and `logging.debug` are accepted
but have no effect yet.

## Limitations

- Streamed OpenAI-compatible responses only include token counts when the
  client sends `"stream_options": {"include_usage": true}`. Without it, the
  query is recorded with model and duration only.
- When the upstream doesn't report tokens per second, it is estimated from the
  completion tokens and generation time and marked `(est.)`. For non-streamed
  responses this includes prompt processing and request overhead, so the
  value is understated, badly so for short replies. LM Studio's
  OpenAI-compatible API (`/v1/...`) never reports TPS for non-streamed
  responses.
- Time to first token is only reported by LM Studio's native API
  (`/api/v1/...`). Ollama (native and OpenAI-compatible) and LM Studio's
  OpenAI-compatible API don't report it, so their lines have no `TTFT`.
- One upstream over plain HTTP; no HTTPS upstream or API key injection.
- Metrics are kept in memory only and are lost when the proxy stops.

## Development

```bash
go test -race ./...
go vet ./...
gofmt -l .
```

See [AGENTS.md](AGENTS.md) for architecture and contribution guidelines.

## License

MIT, see [LICENSE](LICENSE).
