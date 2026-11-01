# Inference metrics

This context describes LLM requests passing through the proxy and the
performance statistics measured from their responses. The terms name concepts;
log lines and code identifiers may word them differently (for example, the
`[Proxy] Non-API request` line reports a rejected request).

## Language

### Requests

**Upstream**:
The LLM server to which the proxy forwards requests, such as LM Studio or Ollama.
_Avoid_: Backend (for the forwarding target; the startup probe's "backend"
names the detected server type).

**Request ID**:
A sequence number the proxy assigns to each forwarded request, metered or not,
shown as `req N` in debug output to tell concurrent requests apart. Rejected
requests receive none. It is independent of the query count.
_Avoid_: Query number, query ID.

**Metered request**:
A forwarded request whose path matches `api_endpoints`, selected for metrics
inspection. Being metered alone does not mean the request contributes to query
statistics.
_Avoid_: API request (when referring specifically to metering eligibility).

**Unmetered request**:
A forwarded request outside the selection for metrics inspection, excluded from
query statistics even if its response contains inference data.
_Avoid_: Non-API request (an unmetered request may still use an upstream API).

**Metrics inspection**:
Parsing a metered request's response body for inference data while forwarding
it to the client. Only JSON (`application/json`, `+json`), SSE
(`text/event-stream`) and NDJSON (`application/x-ndjson`) bodies are inspected.
Other bodies, and upgraded connections, are forwarded without inspection and
cannot yield a query.
_Avoid_: Body logging (bodies are never logged).

**Streamed response**:
A response body inspected incrementally, as SSE events or NDJSON lines. A JSON
body is not a streamed response, even when it arrives in chunks.
_Avoid_: Chunked response (as a synonym).

**Query**:
A metered request recorded because its response provides a model identity, a
positive token count, or a positive generation rate. This includes embeddings
requests and interrupted generations when that evidence is available. An
OpenAI-compatible stream requested without
`"stream_options": {"include_usage": true}` becomes a query with a model
identity only: it has no token counts, so no generation rate can be estimated.
_Avoid_: Request (for the recorded unit), prompt, successful generation.

**Management request**:
A request to a dedicated endpoint that changes which models the upstream has or
runs (transferring, creating, copying, deleting, loading or unloading them), or
that inspects or changes the upstream's account. Refused by default
(`forward_management`). Model listings fall outside this category, as do
generation requests, even when they load or unload a model as a side effect.
_Avoid_: Model request (generation requests load models too).

**Protocol upgrade request**:
A request whose `Upgrade` header offers a protocol other than `h2c` (such as a
WebSocket connection used by client SDKs), on any path. Upgraded connections
tunnel raw bidirectional traffic that bypasses per-request inspection, and are
refused by default (`forward_management`). An `Upgrade` header offering only
`h2c` is removed, and the request is forwarded as plain HTTP/1.1.
_Avoid_: Management request (for the mechanism), streaming request.

**Rejected request**:
A request answered directly by the proxy without contacting the upstream. The
checks run in this order: an unpermitted Host header (403), a `..` path segment
(400), a refused management or protocol upgrade request (403), and an unmatched
path when forwarding is disabled (404).
_Avoid_: Upstream error, failed query (a rejected request is not forwarded).

### Per-query measurements

**Model identity**:
The model name or model instance identifier reported by the upstream for a
query.
_Avoid_: Requested model, draft model (for the query's reported model).

**Prompt tokens**:
The number of input tokens reported by the upstream for a query.
_Avoid_: Input tokens (as a separate metric), prompt length (without a unit).

**Completion tokens**:
The number of output tokens reported by the upstream for a query.
_Avoid_: Output tokens (as a separate metric), response length (without a unit).

**Total tokens**:
The token total attributed to one query, taken from the upstream's total or the
sum of available prompt and completion counts. CSV output writes it as
`total_tokens`.
_Avoid_: Cumulative tokens (for a single query's total).

**Generation rate (TPS)**:
The completion-token rate for one query, expressed in tokens per second, either
reported by the upstream or estimated by the proxy. Terminal output labels it
`TPS`, CSV output `tokens_per_second`.
_Avoid_: Total tokens per second (which suggests including prompt tokens).

**Reported TPS**:
A generation rate based on the upstream's own measurements, supplied either as a
rate or as completion tokens and their evaluation duration.
_Avoid_: Estimated TPS (for a rate calculated from upstream evaluation timing).

**Estimated TPS**:
A generation rate the proxy derives from completion tokens and an elapsed time
when upstream rate measurements are unavailable. The time is the query duration
minus the reported TTFT when available, otherwise the stream time of a streamed
response, otherwise the whole query duration. Bases using the query duration
include request overhead, and without a TTFT also prompt processing. Terminal
output marks it `(est.)`, CSV output with `tps_estimated`.
_Avoid_: Reported TPS, model speed (without an estimate qualifier).

**Query duration**:
The elapsed time from the proxy starting to forward a metered request until it
finishes observing the response body, including an interrupted response.
Terminal output labels it `Time`, CSV output `duration_seconds`.
_Avoid_: Generation time, time to response headers.

**Stream time**:
The elapsed time from the proxy observing the first byte of a streamed response
body until it finishes observing that body. It only serves to estimate TPS and
appears in debug output alone.
_Avoid_: Query duration, time to first token.

**Time to first token (TTFT)**:
The upstream's reported time until the first generated token for a query. The
proxy never measures it; it is unknown when the upstream does not report it.
Terminal output labels it `TTFT`, CSV output `ttft_seconds`.
_Avoid_: Time to first response byte (as an equivalent measurement).

**Query completeness**:
Whether the proxy observed the response body through its normal end.
A complete response may contain an error, and an interrupted response may still
provide query measurements. Terminal output marks incomplete queries
`incomplete`; CSV output has a `complete` column.
_Avoid_: Success, failure (as synonyms for complete and incomplete).

### Aggregates

**Query count**:
The total number of recorded queries since the proxy started. Terminal output
shows it as `#N`, numbering each query in the order queries finish, not the
order they start. Unmetered requests, rejected requests and metered requests
without inference data do not increment this count.
_Avoid_: Total requests, request ID (which numbers forwarded requests).

**Query history**:
The most recently recorded queries, limited to `metrics.history_points` and
ordered by when they are recorded. Queries without a generation rate still
occupy places in this history.
_Avoid_: Last successful queries, last queries started.

**Average TPS**:
The arithmetic mean of available generation rates in the query history,
including both reported and estimated rates. Queries without a rate contribute
no value to this mean. Terminal output shows it as `@ N tok/s` after the
average query duration, although the two cover different sets of queries.
_Avoid_: Aggregate throughput, lifetime average TPS.

**Average query duration**:
The mean query duration across all recorded queries since the proxy started,
including incomplete queries. Terminal output shows it as `Avg: Ns`.
_Avoid_: Recent average duration, average generation time.

**Cumulative tokens**:
The sum of available per-query token totals across all recorded queries since
the proxy started, including incomplete queries. Terminal output displays this
aggregate labeled as "Total tokens", while CSV output uses "total_tokens" for
individual queries.
_Avoid_: Total tokens (without specifying a query or the accumulated total).
