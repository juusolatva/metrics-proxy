# Commands (run from repo root)

- Run: `go run .` (loads `config.yaml` from CWD) or `go run . --config <file>`; add `--csv <file>` to save per-query stats. `go run . --help` lists flags.
- Build: `go build -o metrics_proxy .` — output (`/metrics_proxy`, `metrics_proxy.exe`) is gitignored.
- Test: `go test -v ./...`; race: `go test -race -v ./...`
- Format: `gofmt -s -w .`; check: `gofmt -l .`
- Vet: `go vet ./...`
- Deps: `go mod tidy`, `go mod verify`
- Cognitive complexity (SonarCloud `go:S3776`): `go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3 -over 15 .`
- Windows build check (from Linux): `GOOS=windows go build ./... && GOOS=windows go vet ./... && GOOS=windows go test -c -o /dev/null .`
- Manual smoke test needs LM Studio on the configured host/port, or a stub upstream; proxy listens on `proxy_port` (default 8080).
