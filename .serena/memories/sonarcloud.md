# SonarCloud

- Project `juusolatva_metrics_proxy` (org `juusolatva`), public; automatic analysis of pushed commits (no `sonar-project.properties` or CI workflow in the repo).
- Read open issues without auth: `curl -s "https://sonarcloud.io/api/issues/search?componentKeys=juusolatva_metrics_proxy&resolved=false&ps=100"`. Line numbers refer to the last analyzed (pushed) commit; compare with `git show HEAD:<file>`.
- Rules hit so far (fixed 2026-10-17):
  - `godre:S8188`: a `context.WithCancel` cancel function must be deferred in the creating function. Signal-driven cancels are created in `main` with `defer cancel()` and passed to `setupSignalHandling`.
  - `godre:S8208`: close an HTTP response body with `defer resp.Body.Close()` right after the error check, in the same function (also in tests; `t.Cleanup` in a helper was not flagged).
  - `go:S3776`: cognitive complexity > 15 per function, also in tests (`TestCaptureFixtures` 17, `TestDebugLogging` 22). Fix by moving loop bodies, handlers and checks into named helpers (`checkFixture`, `parseInChunks`, `checkDebugOutput`, `debugRequestIDs`, `debugTestUpstream`, `postAndDrain`).
- Check complexity locally: `go run github.com/uudashr/gocognit/cmd/gocognit@v1.1.3 -over 15 .` (doesn't touch go.mod). It matches Sonar for plain loops/ifs but scores function literals higher (27 vs Sonar's 22). `TestProxyStreamsSSEWithoutBuffering` (20) and `TestProxyForwardsRequestAndResponse` (16) are over in gocognit only, not flagged by Sonar.
