# Task completion checklist

1. `gofmt -s -w .` then `gofmt -l .` prints nothing.
2. `go vet ./...` clean.
3. `go build ./...` succeeds.
4. `go test -race -v ./...` passes; new behavior covered by tests.
5. `go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3 -over 15 .` shows no function you added or changed (SonarCloud `go:S3776`).
6. If behavior, config schema, or known limitations changed: update `AGENTS.md` (architecture §5, limitations §6, config reference §4), `README.md`, `config.yaml` comments, and any affected `.serena/memories/` (they are committed and must not go stale).
7. After merges/rebases: `git diff --check` and `grep -rn '^<<<<<<<\|^>>>>>>>' --include='*.go' .` print nothing (conflict markers were once committed to `main`).
8. No build artifacts staged; `.serena/cache/` and `.serena/project.local.yml` must stay untracked.
