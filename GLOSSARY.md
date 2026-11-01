# Inference metrics

This context describes observations of LLM requests passing through the proxy
and the performance statistics associated with those observations.

## Language

### Requests and observations

**Upstream**:
The LLM server to which the proxy forwards requests, such as LM Studio or Ollama.
_Avoid_: Backend (for the forwarding target).

**Metered request**:
A forwarded request selected for metrics inspection.
Being metered alone does not mean the request contributes to query statistics.
_Avoid_: API request (when referring specifically to metering eligibility).

**Unmetered request**:
A forwarded request outside the selection for metrics inspection, excluded from
query statistics even if its response contains inference data.
_Avoid_: Non-API request (an unmetered request may still use an upstream API).

**Query**:
A metered request recorded because its response provides a model identity, a
positive token count, or a positive generation rate. This includes embeddings
and interrupted generations when that evidence is available.
_Avoid_: Request (for the recorded unit), prompt, successful generation.

**Management request**:
A request to a dedicated endpoint for model transfers, model lifecycle changes,
or upstream account inspection and changes. Model listings and generation
requests that also load or unload a model fall outside this category.
_Avoid_: Any request that loads a model.

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
sum of available prompt and completion counts.
_Avoid_: Cumulative tokens (for a single query's total).

**Generation rate (TPS)**:
The completion-token rate for one query, expressed in tokens per second, either
reported by the upstream or estimated by the proxy.
_Avoid_: Total tokens per second (which suggests including prompt tokens).

**Reported TPS**:
A generation rate based on the upstream's own measurements, supplied either as a
rate or as completion tokens and their evaluation duration.
_Avoid_: Estimated TPS (for a rate calculated from upstream evaluation timing).

**Estimated TPS**:
A generation rate derived from completion tokens and time observed by the proxy
when upstream rate measurements are unavailable. Its timing basis can include
prompt processing and request overhead.
_Avoid_: Reported TPS, model speed (without an estimate qualifier).

**Query duration**:
The elapsed time from the proxy starting to forward a metered request until it
finishes observing the response body, including an interrupted response.
_Avoid_: Generation time, time to response headers.

**Stream time**:
The elapsed time from the proxy observing the first byte of a streamed response
body until it finishes observing that body.
_Avoid_: Query duration, time to first token.

**Time to first token (TTFT)**:
The upstream's reported time until the first generated token for a query.
_Avoid_: Time to first response byte (as an equivalent measurement).

**Query completeness**:
Whether the proxy observed the response body through its normal end.
A complete response may contain an error, and an interrupted response may still
provide query measurements.
_Avoid_: Success, failure (as synonyms for complete and incomplete).

### Aggregates

**Query history**:
The most recently recorded queries, limited to a configured number and ordered
by when they are recorded. Queries without a generation rate still occupy places
in this history.
_Avoid_: Last successful queries, last queries started.

**Average TPS**:
The arithmetic mean of available generation rates in the query history,
including both reported and estimated rates. Queries without a rate contribute
no value to this mean.
_Avoid_: Aggregate throughput, lifetime average TPS.

**Average query duration**:
The mean query duration across all recorded queries since the proxy started,
including incomplete queries.
_Avoid_: Recent average duration, average generation time.

**Cumulative tokens**:
The sum of available per-query token totals across all recorded queries since
the proxy started, including incomplete queries.
_Avoid_: Total tokens (without specifying a query or the accumulated total).
