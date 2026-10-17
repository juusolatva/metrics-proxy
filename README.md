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

By default the proxy reads `config.yaml` from the current working directory.
Use `--config` to load another file, e.g. one per upstream server:

```bash
./metrics_proxy --config ~/metrics_proxy/ollama.yaml
```

Run `./metrics_proxy --help` to list all options.

Point your client at the proxy instead of the LLM server, e.g.
`http://localhost:8080/v1` instead of `http://localhost:1234/v1`.

### Saving statistics to CSV

```bash
./metrics_proxy --csv stats.csv
```

Each recorded query is appended to the file as one row, as soon as it
finishes. The file is created with a header row if it doesn't exist; existing
files are appended to, so restarts keep adding to the same file. Requests
without inference data (such as `/v1/models`) are not written.

```csv
timestamp,method,path,status,model,prompt_tokens,completion_tokens,total_tokens,tokens_per_second,tps_estimated,ttft_seconds,duration_seconds,complete
2026-10-17T03:28:30.366+03:00,POST,/api/v1/chat,200,qwen2.5-coder-0.5b-instruct,32,10,42,271.22,false,0.005,0.054,true
2026-10-17T03:28:30.447+03:00,POST,/v1/chat/completions,200,qwen2.5-coder-0.5b-instruct,32,10,42,234.45,true,,0.043,true
```

Values the server didn't report (tokens, TPS, TTFT) are left empty.
`tps_estimated` is `true` when the TPS was estimated by the proxy (shown as
`(est.)` in the terminal), and `complete` is `false` for responses that were
cut off. Only per-query values are saved; the running averages are not.

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

`config.yaml` (or the file given with `--config`):

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

Unset values fall back to the defaults shown above; an empty file is a valid
config for LM Studio on `localhost:1234`. Unknown or misspelled keys are an
error naming the key and line, e.g.
`line 3: field histroy_points not found in type main.MetricsConfig`. This
includes the removed `metrics.mode` and `metrics.refresh_interval_ms`
settings, so delete them from older config files.

### Debug output

Set `logging.debug: true` to see how the proxy arrived at each query's
numbers. That is useful when a value is missing or looks wrong:

```
[DEBUG] req 2: POST /v1/chat/completions from [::1]:37316
[DEBUG] req 2: upstream http://10.0.0.2:1234/v1/chat/completions -> 200 after 0.00s, Content-Type "text/event-stream"
[DEBUG] req 2: body ended at EOF after 2797 bytes, stream time 0.03s
[DEBUG] req 2: parsed as SSE: 10 JSON document(s), 0 invalid, 0 skipped as larger than 16 MiB
[DEBUG] req 2: stream has no token usage; OpenAI-compatible clients must send "stream_options": {"include_usage": true}
[03:52:45] #2 POST /v1/chat/completions 200 | Model: qwen2.5-coder-0.5b-instruct | Time: 0.04s | ...
```

At startup it also prints the effective configuration and what the backend
probe found. Lines are tagged `req N` so concurrent requests can be told
apart. Request and response bodies are never logged.

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
- Running totals and averages are kept in memory only and are lost when the
  proxy stops. Use `--csv` to keep per-query statistics.

## Development

```bash
go test -race ./...
go vet ./...
gofmt -l .
```

See [AGENTS.md](AGENTS.md) for architecture and contribution guidelines.

## License

MIT, see [LICENSE](LICENSE).
