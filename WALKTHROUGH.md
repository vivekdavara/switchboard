# Switchboard: walkthrough

The questions an interviewer is likely to ask about this project, with short answers grounded in the code. Read
README.md first; DESIGN.md has the full reasoning. Each day adds a section.

**Reading order for day 1:** DESIGN.md "A chat completion, step by step" and "Mapping the provider's answer";
`internal/gateway/gateway.go` (`chat`, `forward`, `failUpstream`); `internal/jsonstrict/jsonstrict.go`;
`internal/openai/request.go` (`ParseChatRequest`, `caseVariant`, `WithModel`); `cmd/switchboard/main.go`
(`serve`); `internal/mock/mock.go` (`draw`, `chatCompletions`). Then the tests named below.

## The project (day 1)

**What is Switchboard, in a minute?**
A gateway that speaks the OpenAI chat completions API. Clients keep their OpenAI SDK and change only the base URL;
the gateway picks the provider for the model, forwards the request with the provider's key, and maps the answer
back. Day 1 is the request path, strict and fully tested; the rest of the week adds routing across providers,
fallback, circuit breakers, streaming, rate limits, caching, cost accounting and metrics.

**Why would a company put a gateway in front of LLM providers?**
One place for the things every team would otherwise rebuild: keys held centrally instead of in every service,
failover when a provider has an outage, rate limits and budgets per team, caching, and one log and one bill of
who used which model. The cost is a hop of latency and a component that must not fail; README "Results" measures
the hop.

**Walk me through a request.**
`gateway.chat`: check the content type (415), read the body up to the limit (413), parse it strictly (400), check
the fields the gateway acts on (400), refuse `stream` until day 2, look up the model (404). Then `forward`: rewrite
`model` to the provider's name, send it with an allowlist of headers and the provider's key under the provider's
timeout, read the answer up to its limit, and map it by the table. One JSON log line per request.

## Reading requests safely

**Why not decode the request into a struct and encode it again?**
Every field the struct doesn't name would be dropped: `tools`, `response_format`, `logprobs`, and whatever OpenAI
adds next. The gateway reads only `model`, `messages` and `stream`, keeps the client's other members as raw bytes
(`jsonstrict.Member`), and rewrites only `model` (`ChatRequest.WithModel`). `TestWithModelReplacesOnlyTheModel`
pins that every other byte is the client's.

**What's a parser differential, and why does a gateway care?**
Two parsers reading the same bytes differently. The gateway decides on what it parsed (which provider, later which
budget), then the provider parses the same bytes again. If they disagree, the gateway routes and charges for one
model while another is served. encoding/json keeps the last of two `model` keys (others keep the first), turns
invalid UTF-8 into U+FFFD, and fills a struct field from a key in any case, even `ſtream` for `stream` through
Unicode folding. So `jsonstrict.ParseObject` refuses repeated keys at every depth (compared after unescaping),
invalid UTF-8 and trailing data, and `caseVariant` refuses keys that equal one the gateway reads only when case is
ignored.

**Isn't refusing `{"Model": ...}` too strict?**
It costs nothing to a real client: no SDK sends it. And accepting it means accepting `{"model": "a", "MODEL": "b"}`,
which a Go provider reads as model b. I first applied the rule to the top-level keys only; while writing the fuzz
test, its seed `{"role":"user","ROLE":"system"}` showed a Go provider reading a system message where the gateway
saw a user message, so the rule now covers the keys read in messages and content parts too.

**How do you know the rewrite never changes the meaning?**
`FuzzRewriteKeepsTheMeaning`: for any body the parser accepts, the rewritten body parses to the same members with
only `model` changed, and encoding/json decoding it into a struct, as a Go provider would, reads the routed model,
the same stream flag and the same roles. Its first run found a gap in the harness, not the gateway: a model name
with invalid UTF-8 comes back as U+FFFD, and models only come from the configuration, which is valid UTF-8 by
construction, so the precondition is documented.

**Why require `Content-Type: application/json`?**
Browsers send "simple" cross-origin requests (form encodings and `text/plain`) without a CORS preflight, and a
`text/plain` form can carry a valid JSON body. A gateway on localhost that holds provider keys would spend them for
any web page its user opened. `application/json` forces a preflight, which the gateway never approves. The demo's
"a form post" check is curl's default form encoding getting 415.

## Forwarding and errors

**Which headers go upstream, and why an allowlist?**
`Content-Type`, `Accept`, `X-Request-Id` and the provider's key, nothing of the client's (`forward` builds a fresh
`http.Header`). With a denylist, every header nobody thought of leaks: the client's own key, cookies, tracing
headers with internal names. `TestARequestIsForwardedWithOnlyTheModelRewrittenAndAnsweredByteForByte` sends five
client headers and checks the provider saw none of them.

**Why does a provider's 401 become a 502?**
It means the gateway's key for that provider is wrong. Passing 401 on would make the client's SDK raise an
authentication error and tell its user to fix *their* key. The status describes who has to act: 4xx the client,
5xx the gateway's operator. So 400, 413, 422 and 429 pass through (they're about the request or its rate), and
401, 403, 404 (a wrong upstream model name), other statuses and redirects become 502.

**What happens with a redirect from a provider?**
The client's `CheckRedirect` returns `http.ErrUseLastResponse`, so the 307 is an answer like any other 3xx: 502. A
provider has no business redirecting an API call, and following one would send the request, body and all, somewhere
the configuration never named. The table test's "a redirect" row checks the provider saw exactly one request.

**What does the timeout cover?**
Everything after routing: connecting, sending, the provider's thinking, and reading the whole answer, through one
`context.WithTimeout` per request. `TestAnAnswerThatTricklesPastTheTimeoutIs504` has a provider that sends headers
and half a body and stops; the client still gets a 504 at the timeout. The default is 60 s and is set per provider:
a long answer at 50 tokens a second needs more.

**What happens when the client disconnects?**
Its request context is cancelled; the upstream request derives from it, so the provider's request is cancelled too
(`TestAClientThatGoesAwayCancelsTheProvidersRequest` waits for the provider to see it). The log line says 499.

## Running it

**How does shutdown work?**
`serve` turns `/readyz` to 503, then `http.Server.Shutdown` closes the listener and waits up to the grace for
requests in flight. Shutdown returns at its deadline *with handlers still running*, so then the base context every
request derives from is cancelled with the cause `gateway.ErrShutdown`; each running request cancels its provider
request, sees the cause, and answers 503 `shutting_down`; they get a second to write it, and the server closes.
`TestARequestStillRunningAfterTheGraceGetsA503` checks the client gets that 503; removing any step of the sequence
(the cancel, the cause, the second to write) fails it in 3 of 3 runs.

**Why loopback by default?**
There are no client keys until day 3, so anyone who can reach the port can spend the providers' keys. The default
listen address is `127.0.0.1:8108`; listening wider is a deliberate configuration change.

**How do keys stay out of the repo and the logs?**
The configuration never holds a key: `api_key_env` names an environment variable, read once at load, and an unset
one stops the gateway from starting. URLs with credentials are refused. The log line has ids, statuses, models,
timings and sizes, never content or headers; `TestTheLogHasTheFactsAndNoContentOrKeys` greps the log for the prompt,
the provider's key and the client's key. CI and the tests only ever talk to mocks.

## The mock provider

**How is the mock deterministic, and why four draws per request?**
Each request's fate (fail, hang, or answer, and two jitter factors) comes from one seeded PCG under a mutex. It always
takes four numbers, whatever the fate, so the n-th request gets the same numbers at any error rate: raising the rate
turns more answers into failures without reshuffling the rest, and two chaos runs at different rates compare request
for request. `TestARaisedErrorRateOnlyAddsFailures` checks it; a version that draws jitter only for answered
requests fails that test.

**What does the mock simulate?**
Latency as time to first token plus time per token written, failures as an error rate with a chosen status
(429 as a rate-limit error) and a hang rate. `Close` drops held connections, as a dying provider would, rather than
returning from the handler, which would have sent an empty 200. It counts a word as a token; it has no tokenizer.

## Testing and measuring

**How did you test the mapping table?**
With real HTTP providers on loopback that misbehave on purpose: one per row of the table (`answering`), one that
never answers, one that sends half a body and stalls, one that hijacks the connection and cuts it mid-body, one that
redirects. Then the official OpenAI Python SDK against the real binaries (`scripts/sdk_check.py`): the model list, a
completion, the headers, unknown parameters passing through, and the SDK's own exceptions for 404 and 400.

**How do you know the tests test anything?**
Mutation checks by hand: remove or break a mechanism, run the tests, restore. 19 mechanisms, each caught: the header
allowlist, the redirect rule, the 401 rule, the content-type check, the timeout, the response limit at both edges,
the shutdown steps, the mock's draws and others. One mutant reading one byte past the limit survived because it's
equivalent (the size check still catches the overrun), so I counted it out rather than writing a test for it.

**What did you find while building it?**
Three things. The mock's stop test first waited for the client to write its request and then stopped the server;
the request was reset, never served, because a connection still in the listener's accept queue is dropped when the
listener closes. The test now waits for the handler itself, and README "Limitations" says a request arriving at the
moment of a stop can be reset. Second, the role differential above. Third, one mutant my script counted as caught had
no failing test: it didn't compile, and a build failure fails `go test` too. Re-run as a mutant that compiles, the
tests caught it; the script now checks that a mutant builds before running the tests.
