# Testing on Windows 11

A checklist for checking that the proxy works on Windows. The binary and the
tests cross-compile for Windows (`GOOS=windows go build`, `go vet` and
`go test -c` all pass on Linux), so this list covers behavior that only
shows up on a real Windows machine. The most likely problems are listed
first.

## Quick run

If there is only time for one pass, these steps cover the likely problems
(Ctrl+C handling, Windows paths, line endings, Excel's file lock):

1. Clone the repository fresh, with Git for Windows' default settings.
2. Run `go test ./...`.
3. Run `go build -o metrics_proxy.exe .`.
4. Start the proxy with a config path containing spaces and a CSV file:
   `.\metrics_proxy.exe --config "C:\Users\<you>\metrics proxy\ollama.yaml" --csv stats.csv`
5. Send one streamed request through it.
6. Press Ctrl+C in PowerShell while the request is still running.
7. Open `stats.csv` in Excel while the proxy is still running.

## 1. Stopping the proxy

- [ ] **Ctrl+C in Windows Terminal (PowerShell and cmd):** prints
  `Shutting down…` and lets a running generation finish. A second Ctrl+C
  forces the stop and logs the request as `incomplete`.
- [ ] **Ctrl+C in Git Bash (mintty):** native Windows programs are often
  killed there without receiving the signal. If so, no graceful shutdown
  happens; that's a terminal limitation, not a proxy bug.
- [ ] **Closing the terminal window during a generation:** Windows only
  allows a few seconds before killing the process, so the 60 s wait doesn't
  apply. Check that CSV rows for finished queries are still in the file; each
  row is flushed when the query finishes.
- [ ] **`taskkill /PID <pid>` without `/F`:** probably can't stop a console
  program. `taskkill /F` kills it at once, with no shutdown.

## 2. CSV file and Excel

- [ ] **Formula protection:** new method/path/model cells that could start a
  spreadsheet formula have a protective leading apostrophe (possibly visible
  depending on import settings). Confirm that a model cell such as `'=1+1`
  remains text. Existing CSV rows are not rewritten; use a new file after
  upgrading or import old text columns explicitly as text.
- [ ] **Open `stats.csv` in Excel while the proxy is running:** Excel locks
  the file, so new rows may fail with `[ERROR] writing CSV`. Check whether
  rows are written again after Excel is closed.
- [ ] **Start the proxy while Excel has the file open:** startup will
  probably fail with "file in use".
- [ ] **Separators:** in locales that use a comma as the decimal separator
  (e.g. Finnish), Excel expects `;` between columns. The file may then open
  as a single column, and values like `48.12` may not be read as numbers.
  Data → From Text/CSV lets you choose the separator. If this is a problem,
  the fix would be a `--csv-separator` option or a semicolon mode.

## 3. Paths for `--config` and `--csv`

- [ ] Backslashes and drive letters: `--config C:\Users\...\ollama.yaml`.
- [ ] Paths with spaces, in quotes.
- [ ] Relative paths, started from a different directory.
- [ ] `~` isn't expanded the way it is on Linux, so the README example
  `--config ~/metrics_proxy/ollama.yaml` may not work. If it doesn't, add a
  Windows example to the README.

## 4. Network

- [ ] **Firewall prompt:** the proxy listens on all network interfaces
  (`:11436` by default), so Windows Firewall asks on first start. Check that clicking
  Cancel still allows connections from the same machine. If it's only used
  locally, a listen-address option for `127.0.0.1` would avoid the prompt.
- [ ] **Reserved ports:** Hyper-V and WSL reserve port ranges, and the
  configured port can fall in one of them. Startup then fails with an
  "access permissions" error.
  `netsh int ipv4 show excludedportrange protocol=tcp` lists the reserved
  ranges.
- [ ] **Upstream offline at startup:** the banner should say
  `(server type not detected)`. Windows takes about 2 s to report a refused
  connection, so startup is slower than on Linux.
- [ ] **`localhost` vs `127.0.0.1`** for an LM Studio or Ollama server on the
  same Windows machine.

## 5. Running the tests

- [ ] **`go test ./...`:** `-race` needs cgo and a C compiler (gcc) on
  Windows. Without one, run the tests without `-race`.
- [ ] **Line endings:** Git for Windows converts line endings to CRLF on
  checkout by default. `.gitattributes` marks `testdata/**` as `-text`, so
  the captured responses stay byte-for-byte; check that a fresh clone's
  `testdata/` files contain no CR characters (`TestCaptureFixtures` and
  `TestProxyOllamaNativeNDJSON` should pass). `config.yaml` may still get
  CRLF, which the YAML parser accepts.
- [ ] **Possibly slow or flaky tests:** two tests connect to a port that has
  just been closed. Windows reports these failures slowly, which could hit
  the tests' 2 s time limits:
  - `TestGracefulShutdownWaitsForInFlightStream` (waits for the listener to
    close)
  - the "unreachable" case of `TestDetectBackend`

## 6. Terminal output

- [ ] **Non-ASCII model names:** may show as garbage in the old console
  window (conhost). Windows Terminal handles them.
- [ ] **`--help`:** shows `Usage: metrics_proxy.exe`.
