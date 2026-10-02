# Task completion checklist

1. `gofmt -s -w .` then `gofmt -l .` prints nothing.
2. `go vet ./...` clean.
3. `go build -o /dev/null .` succeeds (plain `go build ./...` writes a `metrics_proxy` binary into the repo root).
4. `go test -race -v ./...` passes; new behavior covered by tests.
5. If behavior, config schema, or known limitations changed: update `AGENTS.md` (architecture §5, limitations §6, config reference §4), `config.yaml` comments, and any affected `.serena/memories/` (they are committed and must not go stale).
6. No build artifacts staged; `.serena/cache/` and `.serena/project.local.yml` must stay untracked.
