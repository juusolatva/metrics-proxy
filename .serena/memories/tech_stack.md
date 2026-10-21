# Tech stack

- Go; `go.mod` declares `go 1.21`, module name `metrics_proxy`. Don't use stdlib APIs newer than 1.21 without bumping `go.mod` (e.g. `http.ServeMux` method/wildcard patterns need 1.22).
- Only external dep: `go.yaml.in/yaml/v4 v4.0.0-rc.1` (pre-release; YAML config parsing). Import path is `go.yaml.in/yaml/v4`, not `gopkg.in/yaml.v*`.
- Everything else stdlib (`net/http`, `net/http/httputil` ReverseProxy, `encoding/json`, `encoding/csv`, `flag`, `mime`, `os/signal`, `sync/atomic`, …; tests: `net/http/httptest`).
- Dev tools only (not in go.mod): `gocognit` via `go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3`; golangci-lint v2.15.0 and govulncheck v1.9.0 (pinned in `.github/workflows/ci.yml`; Dependabot doesn't bump them, so update by hand together with AGENTS.md, README.md, `mem:suggested_commands` and `mem:task_completion`).
- Dependabot (`.github/dependabot.yml`) proposes updates to the yaml module and the SHA-pinned actions; newer yaml RCs exist (rc.6 as of 2026-10-21).
- Upstream targets: LM Studio (default `localhost:1234`; OpenAI-compatible `/v1/*` + native `/api/v1/*`) and Ollama (`localhost:11434`; `/v1/*` + native `/api/*`). Response bodies: JSON, SSE (`text/event-stream`), NDJSON (`application/x-ndjson`, Ollama native streaming).
- Go 1.21 builtins `min`/`max` are in use; fine under the current `go` directive.
