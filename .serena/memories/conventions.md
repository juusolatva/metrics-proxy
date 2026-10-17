# Conventions

- Stdlib first; justify any new dependency.
- Single-file layout today (`main.go`); splitting into more files within `package main` is fine, keep it one module.
- Exported identifiers with short `// Name does …` doc comments; YAML fields mapped via `yaml:"snake_case"` struct tags.
- Logging is plain `fmt.Printf` to stdout with bracketed prefixes: `[Proxy]`, `[LM Studio]`, `[ERROR]`. No logger package yet.
- Concurrency: write-lock `metrics.mu` for updates, read-lock for display; copy data out rather than holding locks during I/O.
- Tests: every bug fix/feature gets `httptest`-based tests (fake upstream server + `httptest.NewRecorder`/`NewServer`); must pass under `-race`. Tests touching globals (`cfg`, `metrics`) must reset them.
- Parser tests: prefer real captures in `testdata/` (`<upstream>_<api>[_stream][_variant].<ext>`, e.g. `ol_oai_stream_nousage.sse`) over hand-written JSON. They are read with `readFixture()`, fed whole and in 1-byte chunks (`TestCaptureFixtures`), and committed. New upstream/endpoint support → capture JSON + streamed fixtures first (directly from the server, body only). Before capturing, show the user every request (server, model, body) and what gets saved, and wait for approval (user requirement); check captures for private data.
- Proxy must not buffer whole responses before forwarding (breaks SSE streaming); inspect bodies via tee while streaming to client.
- New config options need sane zero-value defaults so existing files keep working. Parsing is strict, so removing or renaming a key breaks files that use it (acceptable for unused settings; note it in README.md).
