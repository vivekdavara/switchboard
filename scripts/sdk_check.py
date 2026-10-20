"""Checks Switchboard with the official OpenAI Python SDK, as a client would use it.

scripts/demo.sh runs this while the gateway (configs/local.json) and both mocks are up, if SDK_PYTHON names a Python
with the packages in scripts/requirements-sdk.txt. The SDK gets a placeholder key there: configs/local.json names no
clients, so no key is checked (and none is ever forwarded). With "day3" it checks the day-3 gateway (run/day3.json,
which demo.sh writes with keys made for the run): client keys and a rate limit the SDK waits out.
"""

import os
import sys
import time

from openai import APIError, AuthenticationError, BadRequestError, NotFoundError, OpenAI, RateLimitError

base_url = os.environ.get("SWITCHBOARD_URL", "http://127.0.0.1:8108/v1")
client = OpenAI(base_url=base_url, api_key="placeholder-not-a-key", max_retries=0, timeout=10)
hello = [{"role": "user", "content": "Hello from the OpenAI SDK"}]
failures = 0


def check(name, ok, detail):
    global failures
    print(f"{'ok  ' if ok else 'FAIL'}  sdk: {name:30} {detail}")
    failures += not ok


if sys.argv[1:] == ["day3"]:
    # The day-3 gateway (run/day3.json): carol has a burst of 1 and 60 requests a minute.
    carol = OpenAI(base_url=base_url, api_key=os.environ["SB_KEY_CAROL"], max_retries=2, timeout=10)
    carol.chat.completions.create(model="plain", messages=hello, max_tokens=3)  # her one request at once
    began = time.monotonic()
    r = carol.chat.completions.create(model="plain", messages=hello, max_tokens=4)
    waited = time.monotonic() - began
    check(
        "a 429 the SDK waits out",
        0.5 < waited < 3 and r.usage.completion_tokens == 4,
        f"answered {waited:.2f} s later: the SDK read Retry-After and retried",
    )
    try:
        OpenAI(base_url=base_url, api_key=os.environ["SB_KEY_CAROL"], max_retries=0).chat.completions.create(
            model="plain", messages=hello
        )
        check("a 429 without retries", False, "no error")
    except RateLimitError as e:
        ms = e.response.headers.get("retry-after-ms")
        check("a 429 without retries", e.code == "rate_limit_exceeded" and ms is not None, f"retry-after-ms {ms}")
    try:
        OpenAI(base_url=base_url, api_key="sb-not-a-key", max_retries=0).models.list()
        check("a wrong key", False, "no error")
    except AuthenticationError as e:
        check("a wrong key", e.status_code == 401 and e.code == "invalid_api_key", f"{e.status_code} {e.code}")
    sys.exit(1 if failures else 0)

if sys.argv[1:] == ["cut"]:
    # scripts/demo.sh has restarted mock-b to break every answer off part-way: the stream starts, then breaks, and
    # the gateway's last event is an error, which the SDK raises.
    seen = 0
    try:
        for chunk in client.chat.completions.create(model="smart", messages=hello, stream=True):
            seen += 1
        check("a stream that breaks", False, f"{seen} chunks and no error")
    except APIError as e:
        check("a stream that breaks", e.code == "upstream_unavailable", f"APIError {e.code} after {seen} chunks")
    sys.exit(1 if failures else 0)

ids = [m.id for m in client.models.list()]
check("the models", ids == ["fast", "gpt-4o-mini", "smart", "mock-small"], ids)

m = client.models.retrieve("smart")
check("one model", m.id == "smart" and m.owned_by == "switchboard", f"{m.id}, owned by {m.owned_by}")
m = client.models.retrieve("gpt-4o-mini")
check("an alias", m.id == "gpt-4o-mini" and m.owned_by == "switchboard", f"{m.id}, owned by {m.owned_by}")
try:
    client.models.retrieve("gpt-9")
    check("an unknown model's details", False, "no error")
except NotFoundError as e:
    check("an unknown model's details", e.status_code == 404 and e.code == "model_not_found", f"{e.status_code}")

raw = client.chat.completions.with_raw_response.create(model="fast", messages=hello, max_tokens=12)
r = raw.parse()
check(
    "a completion",
    r.choices[0].message.content.startswith("This is mock-a answering as mock-small")
    and r.choices[0].finish_reason == "length"
    and r.usage.completion_tokens == 12
    and r.usage.prompt_tokens == 5,
    f"{r.model} {r.usage.prompt_tokens}+{r.usage.completion_tokens} tokens, {r.choices[0].finish_reason}",
)
check(
    "the gateway's headers",
    raw.headers.get("x-switchboard-provider") == "mock-a" and len(raw.headers.get("x-request-id", "")) == 16,
    f"provider {raw.headers.get('x-switchboard-provider')}, request id {raw.headers.get('x-request-id')}",
)

r = client.chat.completions.create(
    model="smart",
    messages=[{"role": "system", "content": "Be brief."}, *hello],
    temperature=0.2,
    n=2,
    extra_body={"a_parameter_from_next_year": {"nested": [1, 2]}},
)
check("n, and an unknown parameter", len(r.choices) == 2 and r.model == "mock-large", f"{len(r.choices)} choices")

try:
    client.chat.completions.create(model="gpt-9", messages=hello)
    check("an unknown model", False, "no error")
except NotFoundError as e:
    check("an unknown model", e.status_code == 404 and e.code == "model_not_found", f"{e.status_code} {e.code}")

text, finish, usage, chunks = "", None, None, 0
for chunk in client.chat.completions.create(
    model="fast", messages=hello, stream=True, stream_options={"include_usage": True}, max_tokens=12
):
    chunks += 1
    usage = chunk.usage or usage
    for c in chunk.choices:
        text += c.delta.content or ""
        finish = c.finish_reason or finish
check(
    "streaming",
    text.startswith("This is mock-a answering as mock-small")
    and len(text.split()) == 12
    and finish == "length"
    and usage is not None
    and usage.completion_tokens == 12,
    f"{chunks} chunks, {len(text.split())} words, {finish}",
)

try:
    client.chat.completions.create(model="fast", messages=hello, stream=True, n=0)
    check("a bad stream request", False, "no error")
except BadRequestError as e:
    check("a bad stream request", e.status_code == 400 and e.param == "n", f"{e.status_code} {e.param}")

sys.exit(1 if failures else 0)
