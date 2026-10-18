# Switchboard

An OpenAI-compatible LLM gateway in Go. Clients point an OpenAI SDK (or curl) at Switchboard instead of at a
provider; it decides which provider serves the requested model, forwards the request, and returns the answer in
the shape the OpenAI API uses. Over five days it gains what a gateway in front of several providers is for:
routing by weights and aliases, fallback with retry budgets and circuit breakers, streaming, per-key rate limits,
response caching, and token and cost accounting, each measured against local mock providers that simulate latency
and errors.

**Status: day 1 of 5.** The request path works end to end for non-streamed chat completions: strict request
reading, routing by model, forwarding with the provider's key, and a fixed mapping of every provider answer and
failure to what the client gets. There is a deterministic mock provider, a graceful shutdown, a demo that runs the
real binaries, and a check with the official OpenAI Python SDK. Day 2 adds routing across several targets, fallback,
circuit breakers and streaming. See [DESIGN.md](DESIGN.md) for the specification and the plan.

## Architecture

```mermaid
flowchart LR
    C["client<br/>(OpenAI SDK, curl)"] -->|"POST /v1/chat/completions"| G
    subgraph G["switchboard (127.0.0.1:8108)"]
        direction LR
        P["read and check<br/>the body"] --> R["route: model →<br/>provider, upstream model"]
        R --> F["forward: rewrite model,<br/>set the provider's key"]
        F --> M["map the answer<br/>or the failure"]
    end
    F -->|HTTP| A["mockprovider mock-a :8118"]
    F -->|HTTP| B["mockprovider mock-b :8119"]
    F -.->|"opt-in, a key from the environment"| L["a live provider"]
```

| Package | What it does |
|---|---|
| `internal/jsonstrict` | Accepts a body only if every JSON parser reads it the same way; returns its top-level members, values byte for byte. |
| `internal/openai` | The request fields the gateway acts on, the rewrite of `model`, OpenAI's error shape, JSON 404s and 405s. |
| `internal/config` | The JSON configuration, read strictly; provider keys only from environment variables. |
| `internal/gateway` | The handler: check, route, forward, map. |
| `internal/mock` | A mock provider: latency, errors and hangs, deterministic given a seed. |
| `cmd/switchboard` | `serve`, `check`, `version`; graceful shutdown. |
| `cmd/mockprovider` | The mock provider as a server. |

## Quick start

Go 1.27 and curl. `make demo` builds everything, starts two mock providers and the gateway, sends eight requests
through it with curl (and eight more with the OpenAI Python SDK, if it is installed), checks each answer, and stops
everything:

```bash
make demo
```

```
ok    the models                         200
ok    fast, through mock-a               200
ok    smart, through mock-b              200
ok    an unknown model                   404
ok    streaming (day 2)                  400
ok    a model given twice                400
ok    a form post                        415
...
ok    smart, with mock-b down            502
all checks passed
```

To run it by hand, in three terminals:

```bash
go run ./cmd/mockprovider -addr 127.0.0.1:8118 -name mock-a
```

```bash
go run ./cmd/mockprovider -addr 127.0.0.1:8119 -name mock-b
```

```bash
go run ./cmd/switchboard serve -config configs/local.json
```

Then:

```bash
curl -s http://127.0.0.1:8108/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"fast","messages":[{"role":"user","content":"What does a gateway do?"}]}'
```

Or with the OpenAI Python SDK (`scripts/sdk_check.py` does more of this, and `make demo` runs it when `.venv` has
the SDK: `python3.11 -m venv .venv && .venv/bin/pip install -r scripts/requirements-sdk.txt`):

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8108/v1", api_key="placeholder")  # client keys come on day 3
reply = client.chat.completions.create(model="fast", messages=[{"role": "user", "content": "Hello"}])
print(reply.choices[0].message.content)  # This is mock-a answering as mock-small. You said: "Hello". ...
```

`make check` runs what CI runs (gofmt, vet, `go test -race`). `go run ./cmd/switchboard check -config FILE`
loads a configuration and prints its routes.

## The API (day 1)

| Method and path | What it does |
|---|---|
| `POST /v1/chat/completions` | Forwards a chat completion to the model's provider. `stream: true` is refused with 400 until day 2. |
| `GET /v1/models` | The models this gateway serves, in OpenAI's list shape. |
| `GET /v1/models/{model}` | One of them (404 `model_not_found` otherwise); SDKs call it to check a model exists. Names may hold slashes. |
| `GET /livez` | 200 while the process runs, whatever the `Host` (load balancers probe by address). |
| `GET /readyz` | 200 while serving; 503 during a stop's `drain_delay`, while everything else is still served. Any `Host`. |

Every error from the API's endpoints, the gateway's own included, has OpenAI's error shape, so SDKs raise their
usual exceptions (the health endpoints answer in plain text):

```json
{"error": {"message": "the model \"gpt-9\" is not served here; GET /v1/models lists the ones that are", "type": "invalid_request_error", "param": "model", "code": "model_not_found"}}
```

Before forwarding, the gateway refuses: a `Host` header that isn't one of `allowed_hosts` (403; by default only
the loopback names, against DNS rebinding), a content type other than `application/json` (415), a body over
`max_request_bytes` (413), a body with more than one meaning (a key given twice, invalid UTF-8, data after the
object: 400), a missing or malformed `model`, `messages` or `stream` (400), and an unknown model (404). Everything
else in the request (`temperature`, `tools`, `response_format`, parameters that don't exist yet) passes through
byte for byte (top-level keys are written again from what they decode to). Only `model` is rewritten, to the
provider's name for the model.

What the client gets for each provider outcome:

| The provider... | The client gets |
|---|---|
| answers 200 with a JSON object | 200 and the body, byte for byte |
| answers 400, 413, 422 or 429 with a JSON object | the same status and body (429 keeps `Retry-After`) |
| answers 401 or 403 | 502 `upstream_auth`: the gateway's key was refused, not the client's |
| answers anything else (5xx, 404, a redirect, a 200 that isn't JSON) or too much | 502 `upstream_error` |
| can't be reached, or breaks off | 502 `upstream_unavailable` |
| is slower than its `timeout` | 504 `upstream_timeout` |

If the client goes away, the provider's request is cancelled too. Each response carries `X-Request-Id` (also on
the request to the provider and in the log line) and `X-Switchboard-Provider`.

## Results (day 1)

Measured on an Apple M3 (8 cores), macOS 26.6.2, Go 1.27.1, **on battery** (99%, Low Power Mode off), other
applications open, load average 3.3 to 4.1, at commit `92f8214`. Medians of 6 runs, ranges in brackets. The
provider is the mock with no simulated latency, on loopback, and the requests are synthetic, so these are the cost
of the HTTP hops and of the gateway's own work, not of any model.

```bash
make bench | tee run/bench.txt
python3 scripts/bench_medians.py < run/bench.txt
```

**What the gateway adds to a request** (`BenchmarkOverhead`, one request at a time, 20,000 a run):

| Request | Straight to the mock: p50 / p99 / mean | Through the gateway: p50 / p99 / mean | Added at p50 |
|---|---|---|---|
| one message, 79 bytes | 39 / 94 / 40.9 µs | 81 / 183 / 85.0 µs | 42 µs |
| 20 messages, 3,997 bytes | 86 / 214.5 / 90.0 µs | 170 / 338.5 / 179.7 µs | 84 µs |

The gateway's own work on the body, parsing, checking and rewriting it (`BenchmarkParseAndRewrite`), is 2.1 µs and 53
allocations for the short request, and 35.0 µs and 397 allocations (38.9 KB) for the conversation. So of the 84 µs
added to the conversation, about 35 are parsing; the rest, and nearly all of the short request's 42 µs, is the
second HTTP hop. Against a real provider, which takes hundreds of milliseconds to seconds, both are under a tenth of
a millisecond. The allocation-heavy parse is the part worth making faster; day 4's load test will show whether it
matters under concurrency.

**Keeping connections to the provider** (`BenchmarkPool`, 32 requests in flight at once, 3,000 a run):

| The gateway's transport | Provider connections opened per request | Time per request | Requests a second |
|---|---|---|---|
| Go's default: 2 idle per host | 0.475 [0.466, 0.505] | 33.9 µs [32.3, 39.3] | about 29,500 |
| Switchboard's: 256 idle per host | 0.0102 [0.0087, 0.0107] | 23.3 µs [22.8, 23.6] | about 42,800 |

With Go's default, about every other request opens a new connection (and leaves a socket in TIME_WAIT for 30 s on
macOS, out of 16,384 ephemeral ports); with 256 kept idle, the gateway opens about 31 connections in 3,000 requests,
roughly one per request in flight, once. That is 45% more requests a second. `TestTheGatewayKeepsItsProviderConnections`
pins the behaviour: after 32 requests at once, the next 32 open 0 connections (30 with Go's default). Over HTTP/2,
which the gateway uses for https providers, one connection carries many requests and the difference shrinks.

**Fuzzing:** `FuzzParseObjectAgreesWithItsOwnOutput` ran 2,355,061 inputs in 21 s with no failure (`go test -run
'^$' -fuzz FuzzParseObject -fuzztime 20s ./internal/jsonstrict`, at `7016e51`); `FuzzRewriteKeepsTheMeaning` ran
1,044,560 inputs in 30 s with no failure (`-fuzztime 30s`, at `60c3a8e`, after a 60 s run whose count wasn't kept).

**Compatibility:** `make demo` passes its eight curl checks and the eight checks of the official OpenAI Python SDK
(openai 3.35.1) here and in CI.

## Configuration

`configs/local.json` routes three models to the two mocks:

```json
{
  "listen": "127.0.0.1:8108",
  "shutdown_grace": "10s",
  "providers": [
    {"name": "mock-a", "base_url": "http://127.0.0.1:8118/v1", "timeout": "10s"},
    {"name": "mock-b", "base_url": "http://127.0.0.1:8119/v1", "timeout": "10s"}
  ],
  "models": [
    {"name": "fast", "provider": "mock-a", "upstream_model": "mock-small"},
    {"name": "smart", "provider": "mock-b", "upstream_model": "mock-large"},
    {"name": "mock-small", "provider": "mock-a"}
  ]
}
```

Other settings: `max_request_bytes` (default 4 MiB), `max_response_bytes` (16 MiB; both at most 1 GiB),
`drain_delay` (default 0: how long a stopping gateway keeps serving with `/readyz` at 503), `allowed_hosts` (default
the loopback names; `["*"]` for any), and per provider `timeout` (default 60 s) and `api_key_env`. **No key is ever in a file:** `api_key_env` names the environment variable that
holds it, and the gateway refuses to start if it is unset. A live provider is opt-in:
`configs/openai.example.json` routes `gpt-4o-mini` to OpenAI with the key from `OPENAI_API_KEY`. Tests and CI use
the mocks only and need no key.

The file is read strictly: a key given twice, a key in the wrong case, an unknown key or a null is an error that
names it, not a silent default.

## The mock provider

`mockprovider` answers like OpenAI's API with a sentence that names the provider and model and quotes the last
user message, padded to `-completion-tokens` words (it counts a word as a token). Its latency is `-ttft` plus
`-per-token` for each word, scaled by `-jitter`. It fails a fraction `-error-rate` of requests with
`-error-status`, and never answers a fraction `-hang-rate`. Every fate comes from a PCG seeded with `-seed`, four
draws per request whatever the outcome, so a run is reproducible, and a higher error or hang rate only turns answers
into failures or hangs.

## Tests

`make check` runs gofmt, `go vet` and `go test -race` over every package (80 test and fuzz functions, about 6 s);
CI runs the same on Ubuntu 24.04, then `make demo`'s script with the SDK checks as a second job.

| Package | What the tests pin down |
|---|---|
| `jsonstrict` | members in order and byte for byte; repeated keys at any depth (also after unescaping); invalid UTF-8; anything but one object; the depth limit; a fuzz test that every accepted body rebuilds to itself |
| `openai` | the acted-on fields and every way they can be wrong; keys equal to them only when case is ignored (including `ſ` for `s`), at the top level, in messages and in parts; the rewrite leaves every other byte alone; a fuzz test that a Go provider reads the forwarded body as the gateway did; the error envelope |
| `config` | the checked-in configs load; defaults; 52 kinds of mistake, each refused with its name; no key, nothing of a `base_url`, and no `api_key_env` that may be a pasted key, in any error |
| `mock` | exact answers and usage; `max_tokens` and `n`; validation; the key; latency lower bounds; jitter bounds; the same seed gives the same run, and a higher error or hang rate only adds failures or hangs; `Close` drops held and later requests; an unreadable body is a 400 |
| `gateway` | a forward with only `model` rewritten and only the allowlisted headers; the `Host` allowlist; every refusal before forwarding, the request limit at exactly its boundary, a 408 for a slow body, a 503 for a body still arriving at the stop; every row of the mapping table with a misbehaving provider; the response limit at exactly its boundary; timeouts (no answer, a trickled body); unreachable and cut-off providers; a client that leaves or half-closes (never an empty 200); the shutdown 503; a forwarded request never resent; HTTP/2 to an https provider; the pool keeping its connections; the log line's facts and absences; 64 concurrent requests |
| `cmd/...` | serving, readiness and a clean stop; the drain (`/readyz` 503 while still answering); a request finishing within the grace; one still running after it getting a 503; a second signal ending the real binary at once; the server's timeouts; usage errors and exit codes |

Mutation checks, run by hand: 36 mechanisms removed or broken one at a time, and each made a test fail: 22 in the
gateway (the header allowlist, the redirect rule, the 401 rule, the content type, the `Host` check, both limits at
both edges, the timeouts, the pool, never resending, waking a slow body at the stop...), 7 in the stop sequence and
the signals, 3 in parsing and the case rules, 4 in the mock. Two more didn't count: one didn't build, and reading one byte more than the response
limit changes nothing (the size check still catches it). The commit messages list them.

## Design decisions

- **Forward the client's bytes, not re-encoded structs.** Decoding into Go structs and encoding again drops every
  field the structs don't name; OpenAI adds parameters every few months. The gateway reads only what it acts on
  and rewrites only `model`.
- **Refuse what parsers disagree on.** The gateway routes on what it parsed, then the provider parses the same
  bytes. encoding/json keeps the last of two `model` keys and fills fields from keys in any case; another parser
  may keep the first or ignore case. So the gateway refuses a key given twice anywhere in the body, invalid UTF-8,
  and any key that equals one it reads (`model`, `messages`, `stream`; a message's `role` and `content`; a part's
  `type` and `text`) only when case is ignored. A fuzz test checks that, for every body accepted, a Go provider
  reads from the forwarded bytes the model, stream flag and roles the gateway acted on.
- **An allowlist, both ways.** Only `Content-Type`, `Accept`, `X-Request-Id` and the provider's key go upstream.
  Only the body, its status and `Retry-After` (on 429) come back. The client's key never reaches a provider, and the
  provider's rate-limit headers (about the gateway's account) never reach the client.
- **The provider's 401 is the gateway's problem.** Passing it on would tell an SDK user that their key is wrong. It
  becomes a 502.
- **`application/json` only, and only known hosts.** A web page can make a browser post a form or `text/plain`
  cross-origin without a preflight; a gateway on localhost that holds provider keys would spend them for any page
  its user visits. And a page whose domain is made to resolve to 127.0.0.1 (DNS rebinding) is same-origin, so it
  needs no preflight at all; its `Host` header is what gives it away, so only `allowed_hosts` are answered.
- **Loopback by default.** Until client keys exist (day 3), anyone who can reach the port can use the providers'
  keys.
- **Stop in steps.** `/readyz` turns 503 and the gateway keeps serving for `drain_delay` (so a load balancer can
  stop sending first); then requests get the grace to finish; the ones still running are then cancelled through the
  server's base context with a shutdown cause, which cancels their provider requests and lets each answer 503.
  `http.Server.Shutdown` alone returns at its deadline with handlers still running. A second Ctrl-C ends it at once.
- **No dependencies.** The standard library covers day 1; Redis and Prometheus come with days 3 and 4.

## Limitations (day 1)

- One provider per model: no fallback, retries or circuit breakers yet (day 2).
- No streaming: `stream: true` is refused (day 2).
- No client authentication, rate limits, caching or cost accounting (day 3); no metrics (day 4).
- `/v1/chat/completions` only: no embeddings, legacy completions, files or assistants.
- The mock counts words, not tokens.
- A request arriving while the gateway stops can be reset rather than answered: a connection still waiting in the
  listener's queue when it closes is never served (seen in the mock's stop test). `drain_delay` with a load balancer
  in front is the cure.
- A provider's error answer larger than `max_response_bytes` becomes a 502 and loses its status (the log keeps it).

## License

MIT
