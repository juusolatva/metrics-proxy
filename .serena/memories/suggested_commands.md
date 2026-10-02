# Commands (run from repo root)

- Run: `go run .` — must run from repo root; `config.yaml` is loaded from CWD.
- Build: `go build -o metrics_proxy .` — `.gitignore` only lists `metrics_proxy.exe`; don't commit the Linux binary (or build to a temp dir).
- Test: `go test -v ./...`; race: `go test -race -v ./...`
- Format: `gofmt -s -w .`; check: `gofmt -l .`
- Vet: `go vet ./...`
- Deps: `go mod tidy`, `go mod verify`
- Manual smoke test needs LM Studio on the configured host/port, or a stub upstream; proxy listens on `proxy_port` (default 8080).
