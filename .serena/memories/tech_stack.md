# Tech stack

- Go; `go.mod` declares `go 1.26` (raised from 1.21 on 2026-10-26), module name `metrics_proxy`. Don't use stdlib APIs or language features newer than 1.26 without bumping `go.mod`; bump policy and steps in AGENTS.md §6 item 7.
- Only external dep: `go.yaml.in/yaml/v4 v4.0.0-rc.6` (pre-release; YAML config parsing). Import path is `go.yaml.in/yaml/v4`, not `gopkg.in/yaml.v*`. Use the v4 API (`yaml.NewLoader` + `With*` options), not the legacy `NewDecoder`/`Unmarshal` shims (they ignore trailing documents).
- Everything else stdlib (`net/http`, `net/http/httputil` ReverseProxy, `encoding/json`, `encoding/csv`, `flag`, `mime`, `os/signal`, `sync/atomic`, …; tests: `net/http/httptest`).
- Dev tools only (not in go.mod): `gocognit` via `go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3`; golangci-lint v2.15.0 and govulncheck v1.9.0 (pinned in `.github/workflows/ci.yml`; Dependabot doesn't bump them, so update by hand together with AGENTS.md, README.md, `mem:suggested_commands` and `mem:task_completion`).
- Dependabot (`.github/dependabot.yml`) proposes updates to the yaml module and the SHA-pinned actions. yaml RCs have changed error wording and internals between releases; `TestParseConfigYAMLRules` pins the behavior config loading relies on, and README.md quotes the error formats.
- Upstream targets: LM Studio (default `localhost:1234`; OpenAI-compatible `/v1/*` + native `/api/v1/*`) and Ollama (`localhost:11434`; `/v1/*` + native `/api/*`). Response bodies: JSON, SSE (`text/event-stream`), NDJSON (`application/x-ndjson`, Ollama native streaming).
- Newer idioms in use since the bump: `min`/`max` builtins, `for i := range n`, `strings.SplitSeq`, `slices.Contains`, `sync.WaitGroup.Go` (1.25).
