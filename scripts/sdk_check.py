"""Checks Switchboard with the official OpenAI Python SDK, as a client would use it.

scripts/demo.sh runs this while the gateway (configs/local.json) and both mocks are up, if SDK_PYTHON names a Python
with the packages in scripts/requirements-sdk.txt. The SDK gets a placeholder key: the gateway doesn't check client
keys until day 3, and it never forwards them.
"""

import os
import sys

from openai import BadRequestError, NotFoundError, OpenAI

base_url = os.environ.get("SWITCHBOARD_URL", "http://127.0.0.1:8108/v1")
client = OpenAI(base_url=base_url, api_key="placeholder-not-a-key", max_retries=0, timeout=10)
hello = [{"role": "user", "content": "Hello from the OpenAI SDK"}]
failures = 0


def check(name, ok, detail):
    global failures
    print(f"{'ok  ' if ok else 'FAIL'}  sdk: {name:30} {detail}")
    failures += not ok


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

try:
    client.chat.completions.create(model="fast", messages=hello, stream=True)
    check("streaming (day 2)", False, "no error")
except BadRequestError as e:
    check("streaming (day 2)", e.status_code == 400 and e.param == "stream", f"{e.status_code} {e.param}")

sys.exit(1 if failures else 0)
