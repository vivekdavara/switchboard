# Switchboard: walkthrough

The questions an interviewer is likely to ask about this project, with short answers grounded in the code. Read
README.md first; DESIGN.md has the full reasoning. Each day adds a section.

**Reading order for day 1:** DESIGN.md "A chat completion, step by step" and "Mapping the provider's answer";
`internal/gateway/gateway.go` (`ServeHTTP` and `hostAllowed`, `chat`, `forward`, `failUpstream`);
`internal/jsonstrict/jsonstrict.go`; `internal/openai/request.go` (`ParseChatRequest`, `caseVariant`,
`WithModel`); `cmd/switchboard/main.go` (`main`, `serve`); `internal/mock/mock.go` (`draw`, `chatCompletions`).
Then the tests named below, and the `review_*_test.go` files, each of which starts with the bug it found.

**Reading order for day 2:** DESIGN.md "Routing", "Fallback", "The retry budget", "Circuit breakers" and
"Streaming"; `internal/gateway/gateway.go` (`chat`'s loop, the `outcome` and `health` types, `try`, `unreached`);
`internal/gateway/stream.go` (`relay`, `endStream`, `eventReader.next`); `internal/breaker/breaker.go` (`Allow`,
`report`); `internal/budget/budget.go` (`Retry`, `totals`); `internal/route/route.go`; `internal/mock/mock.go`
(`draw`, `stream`). Then `TestEachKindOfFailureFallsBackOrNot`, `TestAnUnansweredProviderIsNotAskedAgainButAFailedOneIs`,
`TestAProbeTheBudgetRefusesIsHandedBack`, `TestAResultCountsOnlyInTheStateItsAttemptWasAllowedIn`,
`TestAStreamFallsBackUntilItsFirstEvent`, `TestAStreamThatBreaksAfterItsFirstEventEndsWithAnErrorEvent` and
`TestASlowClientIsNotTheProvidersTimeout`.

**Reading order for day 3:** DESIGN.md "Client keys", "Rate limits", "Caching", "Accounting" and "When Redis is
down"; `internal/ratelimit/ratelimit.go` (the script, `Take`); `internal/gateway/clients.go` (`authenticate`,
`rateLimit`); `internal/gateway/caching.go` (`lookup`, `serveHit`, `store`); `internal/cache/cache.go` (`Key`);
`internal/gateway/accounting.go` (`upstreamBody`, `unasked` with its fast path `unaskedNull`, `withData`, `account`,
`afterAnswer`); `untilDone` in `internal/gateway/clients.go`; `internal/openai/request.go`
(`WithModelAskingUsage`); `internal/usage/usage.go` (`Cost`, `USD`, `Totals.Add`); `internal/config/clients.go`
(`microdollars`). Then `TestAFractionalRateAccumulates`, `TestNoTwoRequestsTakeTheSameToken`,
`TestARateLimitedClientGetsA429ItsSDKWaitsFor`, `TestTheLimitCountsRequestsNotAttempts`,
`TestAStreamsUsageIsCountedWhetherOrNotTheClientAskedForIt`, `TestUsageTakenOutOfEveryChunk`,
`TestASecondIdenticalRequestIsAnsweredFromTheCache` and `FuzzAskingForUsageKeepsTheRest`.

**Reading order for day 4:** DESIGN.md "Metrics", "Logs" and "Load and chaos"; `internal/gateway/metrics.go`
(`observe`, `costs`, `breakers_`), `logExchange` in `internal/gateway/gateway.go` (where `observe` is called, and the
WARN rule), `Gateway.Metrics`; `serve` in `cmd/switchboard/main.go` (the metrics listener); `internal/config/observe.go`;
`internal/load/load.go` (`Run`, `send`, `Percentile`, `Timeline`); `scripts/load.sh` and `scripts/chaos.sh`. Then
`TestTheMetricsCountTheLogLinesFacts`, `agreeWithTheLog`, `TestACostIsExactHoweverManyRequestsAddUpToIt`,
`TestTheMetricsAreServedOnTheirOwnPort`, `TestAnOpenLoopCountsTheWaitAStallCausesAndAClosedLoopDoesnt` and
`TestAnAnswerWhoseRedisStepsFailedIsLoggedAtWarn`; and README "Results (day 4)".

**Reading order for day 5:** CHANGELOG.md; DESIGN.md "The release (day 5)"; `scripts/release_check.sh` (`step`,
`versions`, `clean_export`); `version` in `cmd/switchboard/main.go`; `send` in `internal/load/load.go` (the atomic
first-byte time); `internal/gateway/docs_test.go`. Then `TestAFirstByteAtTheDeadlineIsNoRace`,
`TestDesignsMetricsTableIsTheGatewaysMetrics`, `TestEveryTestTheDocsNameExists`, the `review_d5_*` files, and README
"Results at a glance", "Results (v1.0.0)" and "Reviews and mutation checks".

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
the fields the gateway acts on (400), look up the model (404). Then (day 1's `forward`, day 2's `try`): rewrite
`model` to the provider's name, send it with an allowlist of headers and the provider's key under the provider's
timeout, read the answer up to its limit, and map it by the table. From day 2 that last part runs once per target
until one answers (below). One JSON log line per request.

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

## Routing and fallback (day 2)

**How does the gateway pick a provider?**
A model has targets, each a provider, its name for the model and a weight (`config.Target`). `route.Order` picks the
first at random in proportion to the weights, each next in proportion to the weights left, and puts weight-0
standbys last, so `[{a, 3}, {b, 1}]` goes to `a` three times in four and falls back to `b`. The randomness comes in as
an `IntN` function, so `TestEveryPointOfTheWeightsPicksItsTarget` can say where each draw lands, and 40,000 seeded
draws check the proportions.

**When does it fall back, and when not?**
Every attempt ends in one of three outcomes (`outcome` in gateway.go). *Answered*: a completion or an answer about
the request (400, 413, 422) goes to the client; another provider would refuse the same bad request. *Failed*: the
provider said it didn't do the work (429, 500, 503, 401/403, 404, a redirect), or never got the whole request; any
target may be next. *Unanswered*: it got the request and gave nothing usable (a timeout, a cut-off answer, a 200 that
isn't JSON, or a 502, 504 or 524 from its own front end, whose back end may have done the work); other providers may
be tried, this one isn't asked again. `TestEachKindOfFailureFallsBackOrNot` has 21 rows.

**Why the difference between failed and unanswered?**
Money. An error answer is the provider saying it didn't do it. A timeout isn't proof of anything: the provider may
have generated the whole answer and billed it, and may still be working. Sending it the request again could pay
twice for one answer; sending it to another provider pays at most once more, which is what you buy fallback for.
`TestAnUnansweredProviderIsNotAskedAgainButAFailedOneIs` gives one provider two upstream models: after a timeout the
second is skipped (`unanswered_before` in the log); after a 500 it's tried.

**How do you know whether the provider got the whole request?**
`httptrace.ClientTrace.WroteRequest`, which net/http calls once the request and its body are written. If it never
reported success (the dial was refused, DNS failed, the write broke), the provider can't have a complete JSON body
to act on, so the failure is *failed*, not *unanswered*. The "nothing listening" row tests that side.

**Why never retry the same target?**
The client's SDK already retries (the OpenAI SDK twice, with backoff), and a gateway retrying the same target too
would multiply the attempts: 3 × 3 = 9 upstream calls for one user request. The gateway's value is the other targets.

**What does the client see when everything fails?**
The last attempt's answer, mapped by day 1's table, with only that answer's headers: a first attempt's 429 must not
leave its `Retry-After` on a later 502 (`TestWhenEveryTargetFailsTheClientGetsTheLastAnswer`). The response says
which provider answered and how many targets were tried (`X-Switchboard-Provider`, `X-Switchboard-Attempts`), and
the log line lists each attempt and each skipped target with its reason.

## Retry budget and circuit breakers (day 2)

**What's a retry storm, and what does the budget do about it?**
When a provider fails, every request falls back to the others, just when they are busiest; with a fixed retry
count, an outage of one provider adds its whole traffic as retries on the rest. `budget.Budget` caps retries at a
share of requests: in any 10 seconds at most `ratio` (0.2) per request received, plus `min_per_second` (1) so a quiet
gateway can still fall back. Past that, the client gets the failure at once. `TestMaxAttemptsAndTheRetryBudgetLimitFallback`
shows a ratio of 0.5 letting exactly every second request fall back.

**Why a window of slots and not a token bucket?**
"20% of requests over the last 10 seconds" is the policy itself, and the window says it directly: ten one-second
slots of (requests, retries), each cleared when its second comes round again. A token bucket would need a capacity,
a second knob for how big a burst of retries after a quiet spell may be. Finagle's `RetryBudget` makes the same
choice of ratio plus floor.

**Walk me through the breaker.**
Closed: count failures in a row; 5 open it. Open: refuse for the 10 s cooldown; the gateway skips the provider
without an attempt. Half-open: let one request through as a probe; its success closes the breaker, its failure opens
it for another cooldown. If every target is skipped, the client gets 503 `circuit_open` with `Retry-After` until the
earliest probe, at once, instead of waiting on a provider known to be failing.

**What are the generations for?**
A request allowed while the breaker was closed can take its whole timeout to fail. Without generations it would
then count against a breaker that has since opened (restarting the cooldown) or, if it succeeded during a probe,
close the breaker on old evidence. Every change of state starts a generation, a ticket remembers its own, and a
result from an older one is ignored (`TestAResultCountsOnlyInTheStateItsAttemptWasAllowedIn`). gobreaker does the same.

**What's the easiest bug to write in a half-open breaker?**
A probe that's never reported: the breaker stays "probing" for ever and every request skips the provider. Here the
budget refusing a retry after the breaker had allowed the probe was that path; the ticket is handed back, and
`TestAProbeTheBudgetRefusesIsHandedBack` fails if that line is removed. A ticket reports once, so a deferred hand-back
after a real report is harmless.

**Why doesn't a 404 trip the breaker, while a 429 does?**
A 404 is about one upstream model (a typo in the config), not the provider: the provider's other models may be fine,
and opening its breaker would take them down too (`TestA404DoesNotTripTheProvidersBreaker`). A 429 means the provider
is rate-limiting the gateway's key; sending it every request only collects more 429s, so skipping it for a cooldown
is right.

**What does the breaker buy, in numbers?**
`BenchmarkDeadProvider` (README "Results (day 2)"): a model whose first provider hangs until its 10 ms timeout, with
a standby that answers. With breakers off, every request waits the timeout first: 13.3 ms at p50. With them on, the
first five requests pay it and the rest skip the dead provider: 88 µs at p50, the healthy path's, and the mean falls
from 13.2 ms to 0.21 ms. Against a provider that refuses connections the saving is only the refused dial, about
40 µs. With real timeouts of 30 to 60 s, the hanging case is the one that matters.

**And what does all of day 2 cost a request that needs none of it?**
About 2 µs at p50 (day 1's overhead benchmark: 44 µs added for a small request, against 42); the router, the budget
and the breaker together are about 0.2 µs of that, per their microbenchmarks.

## Streaming (day 2)

**How do you stream through a gateway that also falls back?**
The status of a stream is out with its first byte, so fallback is only possible before it. The gateway reads the
provider's first data event before writing anything (`relay`): a failure until then is an outcome like any other,
and the request falls back (`TestAStreamFallsBackUntilItsFirstEvent` has eight ways to fail first). The first event
commits: 200, the headers and that event, then every event as it arrives, byte for byte, each flushed.

**What happens when a stream breaks halfway?**
It can't become a 502: the client already has a 200 and part of an answer. The gateway writes one last event in
OpenAI's error shape (`endStream`), which the OpenAI SDK raises as an `APIError` (the demo checks this with the real
SDK), and then drops the connection with `http.ErrAbortHandler`, so even a client that ignores the event sees a broken
transfer (curl exits 18) rather than a stream that ended as if complete. A stream that ends cleanly ends with
`data: [DONE]`; one that ends without it counts as broken.

**How do you know events are flushed one at a time, not buffered?**
`TestAStreamIsRelayedEventByEventAsItArrives`: the mock writes 32 words 20 ms apart; through the gateway the events
must arrive spread over more than 500 ms, and match the mock's own stream byte for byte.

**What does the provider's timeout mean for a stream?**
The longest wait for its next sign of progress: the first event, then each next one. A whole-answer timeout would cut
long answers; none would let a stalled provider hold a connection for ever. It's a `time.AfterFunc` that cancels the
attempt's context with its own cause (`errProviderTimeout`), so a timeout is told apart from a client leaving.

**Tell me about a bug you found in your own streaming code.**
The progress timer kept running while the gateway was blocked writing to the client. A client that paused longer
than the timeout got a healthy stream cut as `upstream_timeout`, and the provider's breaker counted the failure: in
the test, 8 MB streamed at once, a client that read one event and then nothing for 600 ms, a 200 ms timeout, and the
stream died after 1.3 MB with the breaker open. I found it writing DESIGN.md's paragraph on backpressure, reproduced
it, and fixed it by stopping the timer around each write (`TestASlowClientIsNotTheProvidersTimeout`).

**And one a fuzzer found?**
`FuzzEventReaderAgreesWithTheReference` compares the streaming event reader (a 16-byte buffer, input arriving a byte
at a time) with a splitter that has the whole input in memory. Its first run found that `data: ` with an empty value
came back as an event with no data, because `bytes.Join` of one empty slice returns nil; by the SSE spec it's an
event whose data is the empty string. After the fix it ran 5.4 million inputs in a minute without a difference.

**What did the review find in day 2's code?**
One agent reviewed `b3b5e1f` for 21 minutes and reproduced four bugs as failing tests: the slow-client timeout (which
I had found and fixed meanwhile, at `f514304`); a streamed probe holding its half-open breaker for the whole stream,
so a healthy provider was skipped as `circuit_open` for as long as one client kept reading slowly (now the success is
reported at the first event, if it is a probe); a stream of nothing but `data: [DONE]`
committing an empty 200 and a success; and the mock checking `stream_options` after its random draw, so a refusal
could become an injected 500 and shifted every later fate. Its suspicions gave seven more changes (a first event
that isn't a JSON chunk falls back; a provider's own 502, 504 and 524 count as unanswered; no budget spent once the
client has left; pings aren't progress; broken streams log at WARN; the budget counts only requests that made an
attempt; a misleading comment), and its list of 24
one-line changes no test caught gave the tests in `health_test.go` and five more: 23 of them fail a test now, and the
24th (a cleanly ended stream's verdict) does too since the second review's fix.

**And the second review?**
It reviewed only the fixes, and found that my fix for the streamed probe was too broad: I had made every stream
report a success at its first event, which resets a breaker's failures in a row, so a provider that broke every
stream after its first chunk never opened its breaker. My test had used `failures: 1`, which a single break trips,
so it couldn't see that. Now only a half-open breaker's probe reports early (`Ticket.Answered`); every other stream
reports when it ends. Reviewing the fixes paid off again, as on day 1.

**How did you make the mock stream, and fail mid-stream, deterministically?**
It writes OpenAI's chunks a token's time apart and `data: [DONE]` (`mock.stream`). `-cut-rate` breaks answers off;
the decision uses the fourth random draw that day 1 reserved, so every request still takes four draws, the failures
don't move when the cut rate changes, and a higher cut rate only turns answers into cuts
(`TestARaisedCutRateOnlyAddsCuts`).

## Client keys and rate limits (day 3)

**How does a client authenticate?**
With the key it would give the OpenAI SDK: `Authorization: Bearer <key>`. The file holds only each client's SHA-256
of its key (`key_sha256`), so the configuration can be checked in; `switchboard keygen` makes a key (32 random
bytes) and prints it once with its hash. The gateway hashes what it's given and looks it up (`authenticate`). Wrong
or missing is a 401 in OpenAI's shape, which the SDK raises as `AuthenticationError`, and so is any path that doesn't
exist, so a caller without a key can't map what's served. The key is never logged and never goes upstream.

**SHA-256, not bcrypt?**
bcrypt is slow on purpose, to protect passwords a person chose, which are guessable. A key here is 256 random bits:
nobody can guess it, fast hash or slow, and a slow hash would cost every request milliseconds. A fast hash of a
random key is the usual way to store issued API keys.

**Walk me through the rate limiter.**
A token bucket per client: `burst` requests at once, refilled at `requests_per_minute`. It lives in Redis so every
gateway shares it, and one Lua script reads and updates it (`internal/ratelimit`), so it's atomic: from Go, a GET and
a SET would let two gateways both see the last token and both take it. `TestNoTwoRequestsTakeTheSameToken` sends 200
requests at once at a burst of 50, and exactly 50 pass.

**What's GCRA, and why use it for a token bucket?**
The same limit, stored differently. Instead of "tokens left, counted at time t", it stores one time: when the bucket
will be full again. Each request allowed pushes that time one interval (60 s / rate) later; a request is allowed if,
pushed, it's at most `burst` intervals ahead of now. I came to it from a bug: my first version stored tokens as a
fraction and added a fraction per call, and `TestAFractionalRateAccumulates` (0.5 a minute, checked every second)
found 120 refills of 1/120 adding up to less than one token, so a request due at exactly that moment was refused
(and told to retry in 1 ms). A time in whole microseconds has nothing to add up.

**Whose clock does the limiter use?**
Redis's (`TIME`, inside the script). Gateways on several machines disagree about the time by milliseconds or more;
one clock in the one place the bucket lives can't disagree with itself. Tests pass their own clock instead.

**Does a fallback count against the client's limit?**
No. The limit counts the client's requests; the gateway's retries are the gateway's cost, and day 2's budget bounds
those. `TestTheLimitCountsRequestsNotAttempts`: two requests that each needed two attempts leave a burst of 2 empty,
not overdrawn. A cache hit does count: it is a request.

**Why check the limit before reading the body?**
So a refused request costs one Redis round trip and nothing more: not reading up to 4 MiB, not parsing it.
`TestARefusedRequestsBodyIsNeverRead` sends a refused request over the size limit and gets 429, not 413.

**What does a client see when it's limited?**
429 `rate_limit_exceeded` with `Retry-After` (seconds) and `retry-after-ms`, plus OpenAI's
`x-ratelimit-{limit,remaining,reset}-requests` on every answer. The OpenAI Python SDK reads `retry-after-ms` and
waits that long before its own retry: in the demo, carol (a burst of 1) sends two requests through the SDK and the
second is answered 1.02 s later, with no retry code of hers.

**And when Redis is down?**
Every command has a timeout (100 ms by default), no retries and one dial attempt. A rate-limited request then goes on
unlimited, logged as `rate_limit: unavailable` (or gets a 503 with `on_error: refuse`). The default is to allow: the
limit protects the providers' budget, and an outage of Redis shouldn't become an outage of the gateway. Tests point
the gateway at a closed port and at a listener that never answers (`TestARedisThatNeverAnswersCostsAtMostItsTimeout`).

## Caching (day 3)

**What's in the cache key?**
The SHA-256 of a version, the scope (the client, or `*` for a shared cache), the model's own name (so an alias shares
its model's entries) and the request body with that name as its model, compacted. Whitespace doesn't matter; any
other difference, even key order, is another entry. That errs on one side only: a miss costs a provider call, a
wrong hit gives someone another request's answer. The parts are separated by NUL bytes, so `alic` + `efast` isn't
`alice` + `fast`.

**Why per client by default?**
A hit tells a client that someone sent exactly that request recently. Between tenants that's a leak (did anyone ask
about X?), so a cache is the client's own unless the model says `shared`.

**What's never cached?**
Anything but an answer: errors, 400s, broken streams (day 2's rule), answers over 1 MiB. A stream is kept only once
its `[DONE]` has arrived, as the bytes the client got, and replayed all at once. A hit is no attempt: no budget, no
breaker; `X-Switchboard-Attempts: 0` and the original's bytes, `id` included.

**How does a client get a fresh answer?**
`Cache-Control: no-cache` skips the lookup and keeps the new answer; `no-store` skips both
(`TestAClientCanAskForAFreshAnswer`).

**Isn't caching wrong with temperature above 0?**
For a model whose answers should vary, yes, and that's why caching is per model and off by default. It suits
deterministic calls (temperature 0, classification, extraction, evals run twice).

## Accounting (day 3)

**Where do the token counts come from?**
The provider's own `usage` object: that's what it bills. The gateway has no tokenizer for every provider, and an
estimate would be wrong in both directions. A plain answer always has one.

**And a stream, whose usage comes only if the client asked for it?**
The gateway asks on the client's behalf: it sets `stream_options.include_usage: true` upstream (merged into the
client's own `stream_options`; `FuzzAskingForUsageKeepsTheRest` checks that nothing else changes), reads the usage as
the chunks pass, and takes it out again (`unasked`): the usage-only last chunk is dropped, and the `"usage": null`
that OpenAI then puts on every chunk is removed. Everything else goes byte for byte, and
`TestAStreamsUsageIsCountedWhetherOrNotTheClientAskedForIt` compares the client's stream with the mock's own stream
for the request as the client sent it, event for event. A provider that refuses `stream_options` gets
`stream_usage: false`.

**Why did asking for usage make you refuse `Include_Usage`?**
Day 1's rule: the gateway refuses keys equal to one it reads only when case is ignored, because a Go provider fills
`include_usage` from `Include_Usage` too. If a client sent `{"Include_Usage": true}`, the gateway would think it
hadn't asked, add `include_usage`, and strip the usage the client had in fact asked for.

**How do you represent money?**
Whole picodollars in an int64. A price is dollars per million tokens, a plain decimal of at most six places,
converted exactly (string arithmetic, no float) to microdollars per million tokens, which is the same number as
picodollars per token: $0.15 per million is 150,000. A cost is two multiplications and an addition. The config caps
prices at $1,000 per million and the gateway believes at most a billion tokens, so each product is at most 10^18 and
the sum fits under 9.2 × 10^18.

**Where do the totals go, and when?**
A hash per UTC day, client and model in Redis, incremented in one MULTI (so a report never sees half a request),
kept 90 days; `switchboard usage` prints a day. They're written before the handler returns, which costs the client a
Redis round trip but lets each request's log line say whether its totals were written. Batching them in the
background is the obvious next step if day 4's load test says the round trip matters.

**What did the review find in day 3's code?**
One agent reviewed the day's core (`4134835`) for 17 minutes and reproduced four bugs, all in streams. A stream with
the provider's own error event in the middle (vLLM sends one, then `[DONE]`, when its engine fails) was cached, so
every identical request got the error from the cache for the ttl; now such a stream is a failure (not kept, not
counted, the breaker told). Taking the usage out dropped any chunk with a `usage` member and no choices, even
`"usage": null`, which loses Azure's first chunk; and it wrote each rewritten event as one data line, losing `id:`
and `event:` lines. And a usage chunk as the first data event committed a 200 of nothing but `[DONE]`.

**And from its suspicions?**
Testing them against a Redis that accepts connections and never answers showed that go-redis ends a command at its
deadline but not on a cancel: a stop waited out the whole Redis timeout, both before the answer and in the writes
after it, while the answer sat in net/http's buffer. Every Redis call now returns when the request's context ends
(`untilDone`), and the writes after an answer ignore a client that left but not the stop (`afterAnswer`). Also a rate
so low it overflowed (a burst may now take at most 30 days to refill), a 200 whose body is an error being cached, and
SCAN returning a key twice. Of its 14 one-line changes no test caught, 13 fail a test now; the 14th touched a field
that was written and never read, which is gone.

**And the second review?**
It looked only at the fixes, and found two bugs in them, as on days 1 and 2. My new rule "drop a chunk with a usage
object and no choices" also dropped an error event that carried a usage, so a provider's refusal read as no answer
and a mid-stream error never reached the client; and the usage I now read before the commit stayed on the exchange
when that attempt failed, so the next provider's answer was billed with the first one's tokens. An error now loses
only its usage, and each attempt starts with no usage.

**What does day 3 cost a request, in numbers?**
Its Redis round trips, each 93 to 99 µs on this Mac's loopback (README "Results (day 3)"): a rate-limited, accounted
request is 285 µs at p50 against 87.5 µs without day 3, and a cache hit 227.5 µs. The key itself is 0.5 µs. Taking
`"usage": null` out of a chunk is 0.41 µs on the fast path for OpenAI's own shape (2.7 µs by parsing the chunk, which
other shapes still take). The round trips are sequential; batching the totals is the obvious cut.

**What isn't counted?**
Errors, broken streams (the provider may still bill the tokens it sent; the log line records the break), and
attempts that went unanswered before a fallback, which may have cost something nobody can know. The log lines are
the record to reconcile against a provider's invoice.

## Metrics, load and chaos (day 4)

**What do you measure, and where does it come from?**
Every fact the per-request log line has, as a Prometheus counter: requests by model, status and the gateway's error
code; duration; attempts and their duration by provider and outcome; skipped targets; budget refusals; rate-limit
results; cache results; Redis failures per step; tokens and cost per client and provider model; breaker states.
They're all filled in one place, `observe`, called right after the log line from the same `exchange` struct, so the
two can't disagree. `agreeWithTheLog` in the tests reads the log lines back and checks the metrics against them.

**Why not increment counters where things happen?**
Then the metrics are a second account of the request, written in a dozen places, and nothing keeps them equal to
the log. The exchange already records every decision for the log line; reading it once is less code and one truth.

**How do you keep cardinality bounded?**
No label value comes from the request. The model label is the configured name an alias resolves to, not what the
client typed; an unknown model is `model=""`. Providers, upstream models and client names come from the config;
statuses, codes, outcomes and reasons are fixed sets. A client sending a million made-up model names makes one series.

**Why is the cost a custom collector?**
Prometheus counters are float64. Adding each request's cost (around 10^-7 dollars) to a float rounds at every
addition: a million $0.00000015 requests sum to $0.15000000000209981. The collector keeps exact int64 picodollars per
label set and converts once, at the scrape, so it reads exactly $0.15. The invoice-grade totals are still Redis's.

**Why a separate port for `/metrics`?**
The API's port wants a client key on every path; a scraper shouldn't hold one, and the metrics port can be
firewalled to the monitoring network. It still checks `Host` like the API, because metrics include client names and
spend, and a DNS-rebinding page could otherwise read them. It keeps answering through the drain, so a scrape sees
the stop, and closes last.

**What's coordinated omission, and how does loadgen avoid it?**
A closed-loop load generator sends its next request only after the last answer, so when the server stalls, the
generator stalls too: it records one slow request, while real clients arriving at their own pace would all have
waited. loadgen's open loop (`-rate`) gives every request a due time and measures from it, not from when a worker
got around to sending it. `TestAnOpenLoopCountsTheWaitAStallCausesAndAClosedLoopDoesnt` shows it: one 300 ms stall at
100 requests a second is about 25 slow requests open-loop, 1 closed-loop.

**What does the gateway cost?**
54 µs a request on loopback (0.097 against 0.043 ms straight to a no-latency mock), and +0.2 ms at the median against
a provider that takes 360 ms at 600 requests a second. With a client key, a rate limit and the totals, two Redis round
trips add 197 µs. Throughput at 32 in flight was 31,247 requests a second through the gateway on a laptop that also
ran the load generator and the mock. The per-request log line costs about 2 µs (`BenchmarkLogLine`, with the
metrics' 0.44 µs in it); under load, runs with and without it overlap.

**So did you batch the totals?**
No. Day 3 left it open; the load test says it's 98 µs, a fraction of a percent of a real completion, and writing
before the handler returns means every log line says whether its totals were written, and nothing is lost when a
gateway dies. I'd batch if a provider answered in a millisecond, or if Redis were across a slow network.

**What happens when a provider dies?**
Killed (connections refused): nothing visible. Of 8,000 requests at 200 a second, all got a 200, p99 57.9 ms. The
12 attempts that hit the dead provider fell back at once, its breaker opened after five failures in a row, and every
later request skipped it (4,010 skips) until probes: one 10 s later failed, one 20 s later found it back. Traffic
returned 5 s after the provider did, because recovery waits for the next probe.

**And when it hangs instead?**
That's the expensive one. A stopped process still has its connections accepted by the kernel; nothing answers until
the gateway's 2 s timeout. Every request sent before the breaker had heard of five timeouts (about 400 in 2 s) waited
2 s and then fell back. The retry budget allowed 394 of those fallbacks and refused 11, which became 504s (6 in the first run). That's the
budget's purpose: a dying provider shouldn't double the load on the healthy one. The levers are a shorter timeout per
provider, or a breaker that counts slow attempts before they finish. I measured the first: at 500 ms instead of 2 s,
105 requests waited (half a second each) instead of about 400 (two seconds), all fell back, and none got a 504. The
catch is that a plain request's timeout bounds its whole answer, so it only suits fast models or streams.

**And Redis?**
Hung, it cost 100 ms per Redis step for about a second, then almost nothing, which surprised me. Reading go-redis's
pool explained it: after `PoolSize` (80) dial errors in a row it fails every dial at once with the last error and
re-dials once a second in the background. Dials started failing once the kernel's accept queue for the stopped Redis
(128 on macOS) filled. With the default `on_error: allow`, all 8,000 requests were answered; about 3,510 ran without a rate
limit, a cache or totals, and the metrics counted each.

**What did the chaos run find?**
A logging gap. With the log level at warn, those 3,500-odd requests all answered 200, so they logged nothing: the log
showed no Redis outage at all. A failed Redis step now makes the line WARN whatever the status.

**What did the review find in day 4's code?**
One agent reviewed it at `2b5b865` for 11 minutes and reproduced seven bugs, each kept as a `review_d4_*` test. Two
broke the "metrics are the log's facts" claim: a cache write that worked erased the lookup's failure (the line said
`unavailable` with no error, at INFO, and the metric missed it), and a stream that broke after its usage chunk was
logged with tokens nothing counted (now `uncounted: true` in the log and `switchboard_uncounted_tokens_total`, since
the provider may still bill them). Three were in loadgen and changed my numbers: p99.9 one rank too high, because
`99.9/100*1000` is `999.0000000000001` in floating point (the rank is integer arithmetic now); only 100 idle
connections kept in all, so it re-dialled above 100 workers; and the open loop's warmup sent as one burst, which put
every chaos fault 0.85 s late on the timeline. One was macOS-specific: `:8108` and `127.0.0.1:8108` both bind, so a
metrics listener on the API's port took the API's local traffic; the config now refuses the same port whatever the
hosts. And a run stopped during the warmup reported a length of minus 292 years. I re-ran every measurement after the
fixes.

**And the second review?**
It checked only the fixes, and found bugs in two of them, as on days 1 to 3. The port check compared strings, but
`net.Listen` reads numbers, so `08108` or `+8108` for the metrics still shadowed the API's `:8108` (and `127.0.0.1:`,
port 0, was wrongly refused); ports go through `net.LookupPort` now. And the uncounted tokens covered only the last
attempt: an attempt that reported its usage and then failed lost it when another provider answered, so each attempt's
entry in the log's `tried` list now carries its usage, and the metric counts it under that attempt's provider. Four
more mutants survived (the metrics through the grace, an open loop's warmup sent twice or with one body, truncating
instead of rounding p); each has a test now. A third review of those two fixes reproduced no bug; its five surviving
mutants (where a failed attempt's tokens land, and under which labels) are caught by one table test now, and a port
past 65535 is refused at load instead of first at serve.

**And the tests it showed were missing?**
Five mutants survived: the WARN rule's rate-limit and cache conditions (only the totals' was pinned), the in-flight
gauge (tests only read 0 at the end), the metrics staying up through the drain, loadgen's per-host connection limit,
and the filter that drops unsent samples from a stopped run. Each has a test now that fails with the mutant.

## The release (day 5)

**What does v1.0.0 promise?**
That a 1.x won't change what a client, an operator or a dashboard depends on: the API's paths, statuses, error codes
and headers; the configuration's keys and what each means; the metric names and labels; the log line's fields; the
Redis keys; the commands and their exit codes (CHANGELOG.md). Adding is allowed, changing isn't. The catch I wrote
down: the config reader is strict, so a key that v1.2 adds is an unknown key, an error, to v1.0. I'd keep the strict
reader anyway; a misspelt `max_atempts` silently becoming the default is worse than a refused file.

**Where does the version come from?**
From the git tag, through the go command: since Go 1.24, `go build` stamps the module version from version control,
so a build at `v1.0.0` with nothing changed reports `v1.0.0`, a build after it a pseudo-version, a modified tree
`+dirty`. `version` in `cmd/switchboard/main.go` reads it with `debug.ReadBuildInfo`. No constant to forget to bump,
and the release check fails if a binary built at a tag doesn't report that tag.

**What does the release check do that CI doesn't?**
CI runs on every push: gofmt, vet, the cross builds, `go test -race`, the benchmarks once, and the demo with the
OpenAI SDK. The release check runs that here and adds: the tests on a `git archive` of the commit (a file I forgot to
add passes on my machine and fails there), the version against the tag, the race detector again with two cores (an
interleaving eight cores hide), 15 s of each fuzz test, and the coverage. It measures nothing; numbers need a quiet
machine and their own runs.

**Tell me about the bug the release check found.**
A data race in the load generator, caught once under `-race` in a test that stops a run part-way. loadgen records each
answer's first byte through httptrace's `GotFirstResponseByte`, which the transport calls from its own read loop. When
a request's deadline fires just as the answer begins, `client.Do` returns (the context is done) while the read loop
may still be about to call the hook, so the hook's write and `send`'s read weren't ordered. It showed 0 times in 200
runs of that test alone, so I wrote one aimed at the moment: 4,000 requests with a 2 ms timeout against a server
answering in 2 ms. The race detector flagged it in 5 of 5 runs, and in none after making the time an
`atomic.Pointer`. The gateway's own hook (whether the provider got the whole request) was already an `atomic.Bool` for
this reason; the load generator, written later, missed it.

**Did it change any published number?**
No: the race was on the first-byte time, and the README's tables report the whole answer's latency, read in the same
goroutine after `Do`. A torn read could only have touched the `FirstByte` median `loadgen` prints, which no table
uses.

**Why test the docs?**
Because v1.0.0 promises the metric names and labels, and DESIGN's table is where an operator reads them. A test now
holds the table to the registry both ways (every metric a row with its labels in order, every row a metric), the
README's configuration example to `configs/local.json`, the README's error example to the gateway's real answer, and
every test name the docs cite to a declared function. Each was checked by breaking the docs in a copy. The last one
mattered at once: a name cut short by a letter passed my first version, which accepted prefixes.
