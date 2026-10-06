# Tech stack

- Go; `go.mod` declares `go 1.21`, module name `metrics_proxy`. Don't use stdlib APIs newer than 1.21 without bumping `go.mod` (e.g. `http.ServeMux` method/wildcard patterns need 1.22).
- Only external dep: `go.yaml.in/yaml/v4 v4.0.0-rc.1` (pre-release; YAML config parsing). Import path is `go.yaml.in/yaml/v4`, not `gopkg.in/yaml.v*`.
- Everything else stdlib: `net/http`, `sync`, `time`, `os/signal`. Preferred additions: `net/http/httputil` (ReverseProxy), `encoding/json`, `net/http/httptest`.
- Upstream target: LM Studio HTTP server (default `localhost:1234`), OpenAI-compatible `/v1/*` plus LM Studio native REST API; responses may be SSE (`text/event-stream`).
