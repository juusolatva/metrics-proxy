# Manual testing

`go test ./...` covers most behavior (see [AGENTS.md](AGENTS.md#3-testing)).
This file covers what needs a real LLM server or a real Windows machine.

## Capturing server responses

The parser tests replay real response bodies from `testdata/`. To support a
new upstream or endpoint, capture both a JSON and a streamed response:

1. Show the user the server, model, request body and what will be saved, and
   wait for approval before sending anything to a real server.
2. Send the request directly to the server, not through the proxy:
   `curl -sN -o testdata/<file> …`
3. Name the file `<upstream>_<api>[_stream][_variant].<ext>`, e.g.
   `ol_chat_stream.ndjson` (`lm_` = LM Studio, `ol_` = Ollama).
4. Check the file for anything private; captures are committed.
5. Add it to `TestCaptureFixtures` in `main_test.go` and to the table below,
   with the capture date and server version.

`.gitattributes` keeps `testdata/` byte-exact, so line endings don't change on
Windows checkouts.

### Current captures

All taken on 2026-10-17 with the prompt `Count from 1 to 5, separated by
spaces.`; only response bodies were saved. Server versions weren't recorded.

| Files | Server | Model | API |
|---|---|---|---|
| `lm_v1_chat.json`, `lm_v1_chat_stream.sse` | LM Studio | `qwen/qwen2.5-coder-14b` | native `/api/v1/chat` |
| `lm_oai.json`, `lm_oai_stream.sse`, `lm_oai_stream_nousage.sse` | LM Studio | same | `/v1/chat/completions` (stream with and without `include_usage`) |
| `ol_chat.json`, `ol_chat_stream.ndjson` | Ollama | `qwen3:4b-instruct-2507-q8_0` | native `/api/chat` |
| `ol_generate.json`, `ol_generate_stream.ndjson` | Ollama | same | native `/api/generate` |
| `ol_oai.json`, `ol_oai_stream.sse`, `ol_oai_stream_nousage.sse` | Ollama | same | `/v1/chat/completions` (stream with and without `include_usage`) |

The LM Studio captures used speculative decoding (draft model Qwen2.5 Coder
0.5B). Its OpenAI-compatible `stats` therefore hold only the draft counters
(`draft_model`, `*_draft_tokens_count`) and no rate.

## Windows 11

The binary and tests cross-compile for Windows:

```bash
GOOS=windows go build ./... && GOOS=windows go vet ./... && GOOS=windows go test -c -o /dev/null .
```

Nothing has been run on Windows yet. The checks below cover behavior that only
shows up on a real machine, most likely problems first.

### Quick run

1. Clone the repository fresh, with Git for Windows' default settings.
2. Run `go test ./...`.
3. Run `go build -o metrics_proxy.exe .`.
4. Start the proxy with a config path containing spaces and a CSV file:
   `.\metrics_proxy.exe --config "C:\Users\<you>\metrics proxy\ollama.yaml" --csv stats.csv`
5. Send one streamed request through it.
6. Press Ctrl+C in PowerShell while the request is still running. Check that
   it finishes and the proxy stops, then restart the proxy.
7. Open `stats.csv` in Excel while the proxy is still running.

### Stopping the proxy

- [ ] **Ctrl+C in Windows Terminal (PowerShell and cmd):** prints
  `Shutting down…` and lets a running generation finish. A second Ctrl+C
  forces the stop and logs the request as `incomplete`.
- [ ] **Ctrl+C in Git Bash (mintty):** native Windows programs are often
  killed there without receiving the signal. No graceful shutdown then; a
  terminal limitation, not a proxy bug.
- [ ] **Closing the terminal window during a generation:** Windows allows only
  a few seconds before killing the process, so the 60 s wait doesn't apply.
  Rows for finished queries should still be in the CSV file.
- [ ] **`taskkill /PID <pid>` without `/F`:** probably can't stop a console
  program. `taskkill /F` kills it at once, with no shutdown.

### CSV file and Excel

- [ ] **Formula protection:** a model cell such as `'=1+1` stays text.
- [ ] **Open `stats.csv` in Excel while the proxy runs:** Excel locks the
  file, so rows may fail with `[ERROR] writing CSV`. Rows are written again
  after Excel is closed (rows from while it was open are lost).
- [ ] **Start the proxy while Excel has the file open:** startup probably
  fails with "file in use".
- [ ] **Separators:** in locales with a decimal comma (e.g. Finnish), Excel
  expects `;` between columns, so the file may open as a single column and
  `48.12` may not be read as a number (Data → From Text/CSV lets you choose
  the separator). If this is a problem, add a `--csv-separator` option. Opened
  directly, Excel splits cells at `;` even inside quoted fields: a path or
  model with `;=1+1` should show `'=1+1` as text in its own cell.

### Paths for `--config` and `--csv`

- [ ] Backslashes and drive letters: `--config C:\Users\...\ollama.yaml`.
- [ ] Paths with spaces, in quotes.
- [ ] Relative paths, started from a different directory.
- [ ] `~` isn't expanded as on Linux, so the README example
  `--config ~/metrics_proxy/ollama.yaml` may not work. If not, add a Windows
  example to the README.

### Network

- [ ] **Firewall prompt:** the proxy listens on all interfaces, so Windows
  Firewall asks on first start. Clicking Cancel should still allow
  connections from the same machine.
- [ ] **Reserved ports:** Hyper-V and WSL reserve port ranges; a port in one
  fails with an "access permissions" error.
  `netsh int ipv4 show excludedportrange protocol=tcp` lists them.
- [ ] **Upstream offline at startup:** the banner says
  `(server type not detected)`. Windows takes about 2 s to report a refused
  connection, so startup is slower than on Linux.
- [ ] **`localhost` vs `127.0.0.1`** for a server on the same machine.

### Running the tests

- [ ] **`go test ./...`:** `-race` needs cgo and a C compiler (gcc); without
  one, run the tests without `-race`.
- [ ] **Line endings:** a fresh clone leaves `git diff --exit-code -- testdata`
  clean, and `TestCaptureFixtures` and `TestProxyOllamaNativeNDJSON` pass.
  `config.yaml` may get CRLF, which the YAML parser accepts.
- [ ] **Slow refused connections:** Windows reports a refused connection
  after about 2 s, so tests that connect to a closed port
  (`TestUpstreamUnavailable`, `TestLogEscapesControlCharacters`, the
  unreachable case of `TestDetectBackend`) should pass, only more slowly.

### Terminal output

- [ ] **Non-ASCII model names:** may show as garbage in the old console
  window (conhost); Windows Terminal handles them.
- [ ] **`--help`:** shows `Usage: metrics_proxy.exe`.
