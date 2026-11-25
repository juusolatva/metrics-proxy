# metrics proxy

[![CI](https://github.com/juusolatva/metrics_proxy/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/juusolatva/metrics_proxy/actions/workflows/ci.yml)
[![Quality Gate Status](https://sonarcloud.io/api/project_badges/measure?project=juusolatva_metrics_proxy&metric=alert_status)](https://sonarcloud.io/summary/new_code?id=juusolatva_metrics_proxy)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=juusolatva_metrics_proxy&metric=coverage)](https://sonarcloud.io/summary/new_code?id=juusolatva_metrics_proxy)
[![Go version](https://img.shields.io/github/go-mod/go-version/juusolatva/metrics_proxy)](go.mod)
[![License: MIT](https://img.shields.io/github/license/juusolatva/metrics_proxy)](LICENSE)

A lightweight HTTP reverse proxy that sits between an LLM client and a local
LLM server and prints inference metrics for every query: model, token counts,
tokens per second, time to first token and duration, plus running aggregates.

Supported upstreams:

- **LM Studio**: native REST API (`/api/v1/...`) and OpenAI-compatible API (`/v1/...`)
- **Ollama**: native API (`/api/chat`, `/api/generate`) and OpenAI-compatible API (`/v1/...`)
- Other OpenAI-compatible HTTP servers

Regular JSON responses and streamed responses (SSE and NDJSON) are both
supported. Streams reach the client as they arrive; a bounded copy is
inspected for metrics.

## Requirements

- Go 1.26 or newer

## Usage

```bash
go build -o metrics_proxy .
./metrics_proxy                                   # reads ./config.yaml
./metrics_proxy --config ~/metrics_proxy/ollama.yaml
./metrics_proxy --help
```

### Connecting clients

The proxy listens on `proxy_port` (default 11436) and forwards each request to
the server with the same path. In the client, replace only the server's host
and port with the proxy's, and **keep the path suffix (such as `/v1`)**:

| Client / API | URL directly to the server | URL through the proxy |
|---|---|---|
| OpenAI-compatible client (OpenAI SDK, most IDE tools) with Ollama | `http://localhost:11434/v1` | `http://localhost:11436/v1` |
| OpenAI-compatible client with LM Studio | `http://localhost:1234/v1` | `http://localhost:11436/v1` |
| Ollama-native client (Ollama libraries, `OLLAMA_HOST`) | `http://localhost:11434` | `http://localhost:11436` |
| LM Studio native REST API | `http://localhost:1234/api/v1` | `http://localhost:11436/api/v1` |

Without the `/v1`, OpenAI-compatible clients request `/models` instead of
`/v1/models`, the server answers `404`, and the proxy logs
`[Proxy] GET /models 404 (…, not metered)`. Ollama-native requests
(`/api/chat`, `/api/generate`) are metered only when `"/api/*"` is in
`api_endpoints` (see [Configuration](#configuration)).

Check the connection with `curl http://localhost:11436/v1/models`: it should
return the server's model list and log `[Proxy] GET /v1/models 200`.

From other machines, use the proxy machine's IP address or hostname; other
names need [`allowed_hosts`](#host-names). One proxy forwards to one server;
to meter several, run one proxy per server with its own `--config` and
`proxy_port`.

### Output

At startup the proxy names the upstream it detected
(`Forwarding to Ollama at localhost:11434`, or `(server type not detected)`).
Each completed query then prints a line like this:

```
[14:02:31] #3 POST /api/v1/chat 200 | Model: qwen3-8b | Tokens: 42 in / 311 out | TPS: 48.12 | TTFT: 0.21s | Time: 6.68s | Avg: 5.90s @ 47.80 tok/s | Total tokens: 1024
```

- `TPS` is the rate the server reports. When it reports none, the proxy
  estimates it and shows `(est.)`.
- `TTFT` (time to first token) is measured by the proxy for streams, from
  receiving the request to receiving the first generated output, so it
  includes model loading. For non-streamed responses it is shown only when
  the server reports it.
- `Avg` is the average duration over all queries and the average tokens per
  second over the last `history_points` queries.

Requests without inference data (model lists, errors) are logged as
`[Proxy]` lines and not counted. If the upstream can't be reached or sends
oversized headers, the client gets `502 Bad Gateway` and the proxy logs an
`[ERROR]` line. Requests to the upstream ignore `HTTP_PROXY` and similar
environment variables. Control characters, invalid UTF-8 and bidirectional text
controls in paths, model names and error messages are printed as Go escapes
(`\x1b`, `\xff`, `\u202e`).

### Saving statistics to CSV

```bash
./metrics_proxy --csv stats.csv
```

Each recorded query is appended as one row when it finishes. A header is
written when the file is new or empty:

```csv
timestamp,method,path,status,model,prompt_tokens,completion_tokens,total_tokens,tokens_per_second,tps_estimated,ttft_seconds,duration_seconds,complete,ttft_measured
2026-10-17T03:28:30.366+03:00,POST,/api/v1/chat,200,qwen2.5-coder-0.5b-instruct,32,10,42,271.22,false,0.005,0.054,true,false
2026-10-17T03:28:30.447+03:00,POST,/v1/chat/completions,200,qwen2.5-coder-0.5b-instruct,32,10,42,234.45,true,0.021,0.043,true,true
```

- Unknown tokens, TPS and TTFT are left empty. `tps_estimated` and
  `ttft_measured` tell estimated and measured values from reported ones;
  `complete` is `false` for cut-off responses. Running averages are not saved.
- An existing file's header is not checked; start a new file when the
  columns change.
- Method, path and model cells that a spreadsheet could read as a formula
  (starting with `=`, `+`, `-` or `@`) get a leading apostrophe. Text after a
  `;` gets one right after the `;` (`/x;'=1+1`), since Excel in locales that
  use `;` as list separator splits cells there. Otherwise text is written as
  received, control characters included, so view the file with `cat -v`
  rather than printing it raw.
- A new file is readable by its owner only on Linux and macOS (mode `0600`);
  on Windows it gets its folder's access rules.
- If a row can't be written (e.g. Excel has the file locked), an
  `[ERROR] writing CSV` line is printed and later rows are written once the
  file accepts writes again.

### Stopping

Press Ctrl+C (or send `SIGTERM`). The proxy stops accepting connections and
waits up to 60 seconds for running requests, streams included, to finish and
be recorded. Press Ctrl+C again to stop at once; cut-off requests are logged
as `incomplete`. Open WebSocket connections are not waited for.

## Configuration

`config.yaml` (or the file given with `--config`); every key is optional and
an empty file works for LM Studio on `localhost:1234`:

```yaml
proxy_port: 11436         # port the proxy listens on

server:                   # upstream LLM server
  host: "localhost"
  port: 1234

api_endpoints:            # path prefixes to meter
  - "/api/v1/*"           # LM Studio native REST API
  - "/v1/*"               # OpenAI-compatible API

forward_unmatched: true   # forward other paths unmetered (false: answer 404)
forward_management: false # forward model management and WebSocket requests (false: answer 403)
allowed_hosts: []         # further host names clients use, see "Host names"

metrics:
  history_points: 10      # recent queries to average tokens/sec over

logging:
  debug: false            # see "Debug output"
```

To front Ollama, set the server to `localhost:11434` and add `"/api/*"` to
`api_endpoints`.

Requests outside `api_endpoints` (such as `/api/tags`) are forwarded unchanged
and logged without metrics, e.g. `[Proxy] GET /models 404 (0.01s, not
metered)`, where the time is until the response headers arrived.

The config is checked strictly. Unknown keys, duplicate keys and a second
YAML document are errors naming the line, e.g.
`line 3: field histroy_points not found in type main.MetricsConfig`. Values
that can't work are errors naming the setting, e.g. ports outside 1–65535, a
`server.host` with a scheme or port, `api_endpoints` not starting with `/` or
with `*` anywhere but at the end, malformed `allowed_hosts` entries, or a
`history_points` below 1.

### Management requests

The proxy listens on all network interfaces and has no authentication: anyone
who can reach `proxy_port` can use the server, even when the server itself
only accepts connections from localhost. Restrict access with a firewall if
needed.

With `forward_management: false` (the default), requests that manage the
server are answered with `403 Forbidden` without forwarding, even when they
match `api_endpoints`:

- Ollama: `/api/pull`, `/api/push`, `/api/create`, `/api/copy`, `/api/delete`,
  `/api/blobs`, and the ollama.com account endpoints `/api/me`,
  `/api/signout`, `/api/user/keys`. Queries that use the account (cloud
  models, `/api/web_search`, `/api/web_fetch`) are forwarded; sign the server
  out of ollama.com if clients shouldn't use it.
- LM Studio: `/api/v1/models/load`, `/api/v1/models/unload`,
  `/api/v1/models/download`.
- WebSocket and other protocol upgrades on any path. LM Studio's SDK
  (`lmstudio-js`, `lmstudio-python`) uses WebSocket, so it needs
  `forward_management: true`. An `Upgrade: h2c` offer (sent by Java's
  `HttpClient`) is dropped and the request forwarded as plain HTTP/1.1.

```
[Proxy] Management request not forwarded: DELETE /api/delete (forward_management: false)
```

In every configuration, paths with a `..` segment (also percent-encoded or
with `\`) and request targets that aren't a path (such as `*`) get
`400 Bad Request`, since the server could resolve them to a management
endpoint. Clients never send these.

### Host names

To stop DNS rebinding (a web page pointing its own domain at the proxy), only
these `Host` names are accepted:

- IP addresses, e.g. `http://192.0.2.10:11436` or `http://[::1]:11436`
- `localhost` and names ending in `.localhost`
- this machine's hostname (as printed by `hostname`)
- names ending in `.local`, `.internal` or `.home.arpa`

Other names get `403 Forbidden`:

```
[Proxy] Rejected GET /api/tags: Host "homelab.lan:11436" not allowed (add "homelab.lan" to allowed_hosts if clients use this name)
```

List further names clients use, without port:

```yaml
allowed_hosts:
  - "llm.example.com"     # this name only (any port)
  - "*.lan"               # all names ending in .lan
```

`"*"` allows any host and turns the check off. This check is not
authentication: it doesn't stop clients that reach the port directly.

### Debug output

Set `logging.debug: true` to see how each query's numbers were derived, e.g.
when a value is missing or looks wrong:

```
[DEBUG] req 2: POST /v1/chat/completions from [::1]:37316 (metered: true)
[DEBUG] req 2: upstream http://192.0.2.20:1234/v1/chat/completions -> 200 after 0.00s, Content-Type "text/event-stream"
[DEBUG] req 2: body ended at EOF, stream time 0.03s from the first output after 2797 bytes
[DEBUG] req 2: parsed as SSE: 10 JSON document(s), 0 invalid, 0 skipped as larger than 16 MiB
[DEBUG] req 2: stream has no token usage; OpenAI-compatible clients must send "stream_options": {"include_usage": true}
[03:52:45] #2 POST /v1/chat/completions 200 | Model: qwen2.5-coder-0.5b-instruct | Time: 0.04s | ...
```

It also prints the effective configuration and the backend probe at startup.
Bodies, query strings and credentials in URLs are never logged.

## Limitations

- **Streamed OpenAI-compatible responses** include token counts only when the
  client sends `"stream_options": {"include_usage": true}`; otherwise only the
  model and duration are recorded.
- **Estimated TPS** (`(est.)`) is completion tokens divided by generation
  time. For non-streamed responses that time includes prompt processing and
  overhead, so short replies are badly understated. For streams the time from
  the first output to the last chunk is used, unless the stream arrived all
  at once or faster than about 1000 tokens/s. LM Studio's OpenAI-compatible
  API never reports TPS.
- **TTFT** for non-streamed responses is reported only by LM Studio's native
  API. Measured stream TTFT includes model loading and network time, and is
  missing when the output is in fields the proxy doesn't know (e.g. LM
  Studio's native tool call events).
- **Inspection limits:** 16 MiB per body, SSE event or NDJSON line, eight
  `result`/`response` wrapper levels and 32 MiB of JSON decoding per
  document. Larger input is forwarded but not metered. Implausible reported
  numbers (over 2^29 tokens, a million tokens/s or 24 h TTFT) are ignored.
- **One plain-HTTP upstream**; no HTTPS upstream or API key injection.
- **No client authentication**: queries can also load and unload models
  (see [Management requests](#management-requests)).
- **Totals are in memory only**; use `--csv` to keep per-query statistics.
- **Not yet tested on Windows**; see [TESTING.md](TESTING.md#windows-11).

## Development

```bash
go test -race ./...
go vet ./...
gofmt -s -l .
go mod verify
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go test -run '^$' -fuzz='^FuzzCaptureSplit$' -fuzztime=30s .  # fuzz one target
```

GitHub Actions runs these checks on pushes to `main`, pull requests, weekly
and on demand. [AGENTS.md](AGENTS.md) describes the architecture and how to
maintain the code, [TESTING.md](TESTING.md) the manual tests, and
[SECURITY.md](SECURITY.md) how to report vulnerabilities.

## License

MIT, see [LICENSE](LICENSE).
