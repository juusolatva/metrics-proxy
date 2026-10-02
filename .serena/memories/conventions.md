# Conventions

- Stdlib first; justify any new dependency.
- Single-file layout today (`main.go`); splitting into more files within `package main` is fine, keep it one module.
- Exported identifiers with short `// Name does …` doc comments; YAML fields mapped via `yaml:"snake_case"` struct tags.
- Logging is plain `fmt.Printf` to stdout with bracketed prefixes: `[Proxy]`, `[LM Studio]`, `[ERROR]`. No logger package yet.
- Concurrency: write-lock `metrics.mu` for updates, read-lock for display; copy data out rather than holding locks during I/O.
- Tests: every bug fix/feature gets `httptest`-based tests (fake upstream server + `httptest.NewRecorder`/`NewServer`); must pass under `-race`. Tests touching globals (`cfg`, `metrics`) must reset them.
- Proxy must not buffer whole responses before forwarding (breaks SSE streaming); inspect bodies via tee while streaming to client.
- Keep `config.yaml` backward-compatible; new options need sane zero-value defaults.
