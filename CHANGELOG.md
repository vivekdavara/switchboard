# Changelog

## v1.0.0 (2026-10-22)

The first release: an OpenAI-compatible gateway for `/v1/chat/completions`, built over five days (2026-10-18 to
2026-10-22). README.md has the measurements and the limitations; DESIGN.md the reasons.

**The request path (day 1).**

- `POST /v1/chat/completions`, `GET /v1/models`, `GET /v1/models/{model}`, `GET /livez`, `GET /readyz`; every error in
  OpenAI's error shape, so SDKs raise their usual exceptions.
- Strict reading: a body any two JSON parsers could read differently (a key given twice at any depth, invalid UTF-8,
  data after the object, a key equal to one the gateway reads only when case is ignored) is a 400. Everything else in
  the request passes byte for byte; only `model` is rewritten.
- A fixed mapping of every provider answer and failure to what the client gets (the provider's 401 is a 502
  `upstream_auth`, a timeout a 504, a cut-off a 502 `upstream_unavailable`).
- Header allowlists both ways; a `Host` allowlist (loopback names by default) against DNS rebinding;
  `application/json` only; request and response size limits.
- A stop in steps: `/readyz` at 503 for `drain_delay`, then a grace for running requests, then their cancellation
  with a 503; a second signal ends the process at once.
- Provider keys only from environment variables, never from a file.

**Routing, fallback and streaming (day 2).**

- Models with aliases and weighted targets (weight 0: a standby, tried last); `max_attempts`.
- Three outcomes per attempt (answered, failed, unanswered): a provider that may have done the work is never sent the
  request again.
- A retry budget (by default 20% of requests plus 1 a second, over 10 s) and a circuit breaker per provider (5
  failures in a row, 10 s, one probe), with results kept to their generation.
- Server-sent events relayed one at a time, falling back until the first event, and an error event (which the OpenAI
  SDK raises) after it.

**Clients, limits, cache and accounting (day 3).**

- Client keys, kept in the file as their SHA-256 (`switchboard keygen` makes one); a 401 for a missing or unknown key.
- A token bucket per client in Redis (GCRA in one Lua script, on Redis's clock), with OpenAI's `x-ratelimit-*`
  headers and a 429 the OpenAI SDK waits out by itself.
- A response cache per model, per client unless `shared`: answers only, `Cache-Control: no-cache` and `no-store`.
- Token and cost accounting from the provider's own `usage` (for a stream whose client didn't ask, the gateway asks and
  takes it out again), priced in integer picodollars, with daily totals per client and model (`switchboard usage`).
- Redis failures bounded by `redis.timeout`, and open by default (`on_error: refuse` to fail closed).

**Observing and measuring (day 4).**

- Prometheus metrics on a port of their own, the log line's facts as series, the cost exact; the log's level and
  format.
- `loadgen`, with an open loop that measures from each request's due time (no coordinated omission);
  `scripts/load.sh` and `scripts/chaos.sh` (a provider killed, a provider hung, Redis hung and shut down).

**The release (day 5).**

- `scripts/release_check.sh` (`make release-check`): what CI runs, the tests on a clean export of the commit, the
  binary's version against the release tag, the demo, the benchmarks run once, the race detector on two cores, the
  fuzz tests and the coverage.
- `loadgen`: the first byte's time is kept atomically; a request whose deadline fired as its answer began was a data
  race (found by the release check's first full run).

**What a 1.x release keeps.** The API's paths, status codes and error codes; the response headers
(`X-Request-Id`, `X-Switchboard-Attempts`, `X-Switchboard-Provider`, `X-Switchboard-Cache`, the `x-ratelimit-*`
ones); the configuration's keys and their meaning (a 1.x may add keys, never change what one means; the file stays
strict, so a key a later version adds is an error to an earlier one); the metric names and labels; the log line's
fields; the Redis keys (`<prefix>ratelimit:<client>`, `<prefix>cache:<sha256>`,
`<prefix>usage:<day>:<client>:<model>`); the commands, their flags and exit codes. A change to any of these is a 2.0.
