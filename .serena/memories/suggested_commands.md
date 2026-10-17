# Commands (run from repo root)

- Run: `go run .` (loads `config.yaml` from CWD) or `go run . --config <file>`; add `--csv <file>` to save per-query stats. `go run . --help` lists flags.
- Build: `go build -o metrics_proxy .` — output (`/metrics_proxy`, `metrics_proxy.exe`) is gitignored.
- Test: `go test -v ./...`; race: `go test -race -v ./...`
- Format: `gofmt -s -w .`; check: `gofmt -l .`
- Vet: `go vet ./...`
- Deps: `go mod tidy`, `go mod verify`
- Manual smoke test needs LM Studio on the configured host/port, or a stub upstream; proxy listens on `proxy_port` (default 8080).
