# Switchboard: walkthrough

The questions an interviewer is likely to ask about this project, with short answers grounded in the code. Read
README.md first; DESIGN.md has the full reasoning. Each day adds a section.

**Reading order for day 1:** DESIGN.md "A chat completion, step by step" and "Mapping the provider's answer";
`internal/gateway/gateway.go` (`ServeHTTP` and `hostAllowed`, `chat`, `forward`, `failUpstream`);
`internal/jsonstrict/jsonstrict.go`; `internal/openai/request.go` (`ParseChatRequest`, `caseVariant`,
`WithModel`); `cmd/switchboard/main.go` (`main`, `serve`); `internal/mock/mock.go` (`draw`, `chatCompletions`).
Then the tests named below, and the `review_*_test.go` files, each of which starts with the bug it found.

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

**What's DNS rebinding, and why does the gateway check `Host`?**
A page on `attacker.example` gets its DNS answer changed to 127.0.0.1 after it loads. The browser still thinks it is
talking to `attacker.example`, so requests to `http://attacker.example:8108` are same-origin: no CORS, no preflight,
the content-type rule doesn't stop them, and they land on the gateway. The one thing that gives them away is the
`Host` header, which still says `attacker.example`. So `ServeHTTP` answers only `allowed_hosts` (by default
`localhost`, `127.0.0.1`, `::1`) and everything else gets 403. The review suspected this; I confirmed a request with
that Host got a 200 and reached the provider, before adding the check (`TestOnlyAllowedHostsAreAnswered`).

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

**Can Go's HTTP client send your POST twice?**
Yes, by itself, in one case: a reused connection breaks after the request was written, and the request is
"replayable", which for a POST means it carries an `Idempotency-Key` or `X-Idempotency-Key` header (and a body it can
rewind). A resent completion is billed twice. The gateway's upstream POSTs carry neither header.
`TestAForwardedRequestIsNeverResentByTheTransport` has a provider answer once, then read the next request on the same
connection and drop it: two requests in all, and a 502. With an `Idempotency-Key` added upstream the provider sees
three. (If nothing was written before the connection broke, Go retries any request with a rewindable body; that's
safe, the provider never saw it.)

**Does the gateway speak HTTP/2 to providers?**
To https ones, yes: `NewTransport` clones Go's default transport, and its `ForceAttemptHTTP2` keeps HTTP/2 on even
with a TLS configuration of its own. `TestAnHTTPSProviderIsReachedOverHTTP2` checks the provider sees `HTTP/2.0`,
and fails with that flag off. Over HTTP/2 one connection carries many requests, so the idle-pool numbers in README
"Results" (taken over plain HTTP/1.1 to the mock) matter less for a real https provider.

**What happens when the client disconnects?**
Its request context is cancelled; the upstream request derives from it, so the provider's request is cancelled too
(`TestAClientThatGoesAwayCancelsTheProvidersRequest` waits for the provider to see it). The log line says 499, and
the handler drops the connection with `panic(http.ErrAbortHandler)` rather than returning without writing.

**Why panic instead of just returning?**
Because a handler that returns without writing gets `200 OK` with an empty body from net/http. The first version did
exactly that, assuming nobody was listening, and the review showed two ways somebody was: a malformed chunked body
(the read fails, the client is still there) and a client that half-closes its side after sending the request
(net/http reports that as "gone", but it is still reading). Both got an empty 200, which an SDK would try to parse
as a completion. Now an unreadable body is a 400 `invalid_body`, and a cancelled request drops the connection; the
deferred log line still runs (`review_empty200_test.go`).

## Running it

**How does shutdown work?**
In steps. `serve` turns `/readyz` to 503 and keeps serving for `drain_delay` (0 by default; next question). Then
`http.Server.Shutdown` closes the listener and waits up to the grace for requests in flight. Shutdown returns at its
deadline *with handlers still running*, so then the base context every request derives from is cancelled with the
cause `gateway.ErrShutdown`; each running request cancels its provider request, sees the cause, and answers 503
`shutting_down` (one still reading a slow body is woken by a read deadline); they get a second to write it, and the
server closes. `TestARequestStillRunningAfterTheGraceGetsA503` checks the client gets that 503; removing any step
of the sequence (the cancel, the cause, the second to write) fails it in 3 of 3 runs. A second signal, of the other
kind or the same one after the first was taken, ends the process at once.

**What is `drain_delay` for?**
Telling a load balancer to stop sending before the port closes. On a stop, `/readyz` turns 503, but `Shutdown`
closes the listener in the same instant, so the first version's 503 could never be seen: a probe got "connection
refused" (found in review). With `drain_delay`, the gateway keeps serving everything for that long with `/readyz` at
503, then closes. It defaults to 0 because there's no balancer in front of a laptop. `/livez` and `/readyz` answer
any `Host`, since a balancer probes by address. A second Ctrl-C ends the process at once, mid-drain or mid-grace:
`main` keeps receiving signals after the first and raises the second again with the default handling, so the
process dies by it (two tests run the real binary).

**Tell me about a test that passed locally and failed in CI.**
That one, and it took three tries. My first version handed signals back in a goroutine woken by the same cancelled
context as the stop. It passed 20 of 20 runs on my laptop and failed once on a 2-CPU CI runner: the drain started and
logged, the test sent its second signal, and the hand-back hadn't run yet, so the signal was swallowed. A 100 ms delay
at that point showed the window: 3 of 3 failures. My second version reset the signals first, then cancelled; it
survived that delay, but the second review sent the two signals back to back and it swallowed the second 10 times in
10 (3 in 10 after I buffered two): a signal still pending inside the Go runtime when `signal.Reset` runs is dropped.
The third keeps receiving after the first signal and, when a second comes, resets and raises it again; 20 of 20 runs
of both tests, with 8 CPUs and with 2. A third review then sent the *same* signal twice back to back and the stop went
on: the kernel and Go's runtime keep one pending flag per signal number, so the two merge before any code runs.
Nothing can fix that, so the docs say it, and the test checks the same signal again once the first was taken, which
is what a person's second Ctrl-C is. Lessons: "after X happens" across two goroutines is not an order; turning a
mechanism off is itself an event that something can arrive in the middle of; and signals aren't a queue.

**Why loopback by default?**
There are no client keys until day 3, so anyone who can reach the port can spend the providers' keys. The default
listen address is `127.0.0.1:8108`; listening wider is a deliberate configuration change.

**How do keys stay out of the repo and the logs?**
The configuration never holds a key: `api_key_env` names an environment variable, read once at load, and an unset
one stops the gateway from starting. URLs with credentials are refused, and no configuration error repeats a
`base_url` at all: the first version quoted it, through `url.Redacted()` at best, which hides a password but not a
key pasted as the user name or into the query (found in review). A key with a space or control character (a line
ending from a `.env` file) is refused too; before that, net/http refused to send it and every request looked like
an unreachable provider. The log line has ids, statuses, models,
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

**What does the gateway cost?**
At p50, 42 µs on a 79-byte request and 84 µs on a 4 KB conversation, against a mock answering on loopback with no
latency (README "Results"). The parse and rewrite are 2.1 and 35 µs of that (53 and 397 allocations); the rest is
the second HTTP hop. A real model takes hundreds of milliseconds, so it's under 0.1% there. If anything, the parse
is what I'd optimize first: it goes over the body several times and allocates for every token.

**Why keep 256 idle connections per provider?**
Go's default keeps 2 per host. With 32 requests in flight, every other request then opened a new connection (0.475 a
request) and left a socket in TIME_WAIT; with 256 it was 0.0102, roughly one connection per request in flight,
opened once, and 45% more requests a second (`BenchmarkPool`). `TestTheGatewayKeepsItsProviderConnections` pins
it: the second burst of 32 opens 0 connections, 30 with the default.

**What did the review find?**
One agent reviewed the code at `07b8b14` for 16 minutes and reproduced eight defects as failing tests: the empty
200s (gateway and mock), `base_url` secrets in error messages, `max_response_bytes` at `math.MaxInt64` overflowing
`limit+1` (every answer a 502), the mock's `Close` comment promising what it didn't do and its hang band moving with
the error rate, and `/readyz`'s 503 being unobservable. Its suspicions gave three more fixes (DNS rebinding, keys
with control characters, the swallowed second Ctrl-C) and its list of ten mechanisms it could break without a
test failing gave five new tests and five new cases or checks in existing ones. Each fix is a commit that says "found in review".
A second, narrower review of only the code written since found that my fixes had gaps of their own: the new `Host`
check refused HTTP/1.0 health probes (no `Host` at all) and IPv6 written out in full, `allowed_hosts` refused Docker
Compose's underscore names, and `api_key_env`'s errors printed a key pasted into it, the very thing the `base_url`
fix had just ruled out elsewhere. Reviewing the fixes paid off again.

**How did you test the mapping table?**
With real HTTP providers on loopback that misbehave on purpose: one per row of the table (`answering`), one that
never answers, one that sends half a body and stalls, one that hijacks the connection and cuts it mid-body, one that
redirects. Then the official OpenAI Python SDK against the real binaries (`scripts/sdk_check.py`): the model list, a
completion, the headers, unknown parameters passing through, and the SDK's own exceptions for 404 and 400.

**How do you know the tests test anything?**
Mutation checks by hand: remove or break a mechanism, run the tests, restore. 46 mechanisms, each caught: the header
allowlist, the redirect rule, the 401 rule, the content-type and `Host` checks, the timeouts, both limits at both
edges, the shutdown steps and the signals, the case rules, the mock's draws and others. One mutant reading one byte
past the limit survived because it's equivalent (the size check still catches the overrun), so I counted it out
rather than writing a test for it. The reviews found the gaps I'd missed: ten mechanisms the first could break with
every test still passing, five the second could and four the third could; all but one are covered now, and that
survivor is named in README "Tests".

**What did you find while building it?**
Three things. The mock's stop test first waited for the client to write its request and then stopped the server;
the request was reset, never served, because a connection still in the listener's accept queue is dropped when the
listener closes. The test now waits for the handler itself, and README "Limitations" says a request arriving at the
moment of a stop can be reset. Second, the role differential above. Third, one mutant my script counted as caught had
no failing test: it didn't compile, and a build failure fails `go test` too. Re-run as a mutant that compiles, the
tests caught it; the script now checks that a mutant builds before running the tests.
