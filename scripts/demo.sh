#!/usr/bin/env bash
# Runs the real binaries: two mock providers (8118, 8119) and the gateway (8108, configs/local.json), sends a few
# requests through the gateway, checks each answer, and stops everything. CI runs it as a smoke test; `make demo`
# runs it here. Logs go to run/ (gitignored). Needs curl.
#
# Day 3's part runs the gateway again with client keys made for this run (run/day3.json, never checked in), a rate
# limit, a cache and prices, against the Redis on 127.0.0.1:6382: the one already running there (CI's service
# container, or make test's), or one it starts itself (redis-server) and stops at the end.
#
# With the official OpenAI Python SDK it also runs scripts/sdk_check.py: set SDK_PYTHON to a Python that has
# scripts/requirements-sdk.txt installed (CI does), or have it in .venv (python3.11 -m venv .venv &&
# .venv/bin/pip install -r scripts/requirements-sdk.txt).
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=http://127.0.0.1:8108
REDIS_PORT=6382
mkdir -p run bin
go build -o bin/ ./cmd/...

pids=()
started_redis=
prefix="switchboard-demo-$(date +%s)-$$:"
cleanup() {
  # Every step may fail here (a process already gone); none may stop the rest.
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  if command -v redis-cli >/dev/null; then
    redis-cli -p "$REDIS_PORT" --scan --pattern "$prefix*" 2>/dev/null |
      xargs -n 100 redis-cli -p "$REDIS_PORT" del >/dev/null 2>&1 || true
  fi
  if [[ -n $started_redis ]]; then redis-cli -p "$REDIS_PORT" shutdown nosave >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

bin/mockprovider -addr 127.0.0.1:8118 -name mock-a -ttft 50ms -per-token 1ms 2>run/mock-a.log &
pids+=($!)
bin/mockprovider -addr 127.0.0.1:8119 -name mock-b -ttft 80ms -per-token 2ms 2>run/mock-b.log &
mock_b=$!
pids+=($mock_b)
bin/switchboard serve -config configs/local.json 2>run/switchboard.log &
pids+=($!)

# Wait for all three, not just the gateway: it gets ready without asking its providers, and a request sent before a
# mock listens gets a 502 (it happened: the gateway listened 200 ms before a freshly built mock did).
for url in "$GATEWAY/readyz" http://127.0.0.1:8118/livez http://127.0.0.1:8119/livez; do
  for _ in $(seq 100); do
    curl -sf "$url" >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -sf "$url" >/dev/null || { echo "$url never answered"; cat run/*.log; exit 1; }
done

failures=0
# check NAME WANT_STATUS WANT_TEXT CURL_ARGS...: one request, its status and a piece of its body.
check() {
  local name=$1 want=$2 text=$3
  shift 3
  local status
  status=$(curl -s -o run/body -w '%{http_code}' "$@")
  if [[ $status == "$want" ]] && grep -qF -- "$text" run/body; then
    printf 'ok    %-34s %s\n' "$name" "$status"
  else
    printf 'FAIL  %-34s %s (want %s with %s): %s\n' "$name" "$status" "$want" "$text" "$(head -c 300 run/body)"
    failures=$((failures + 1))
  fi
}
chat() { # chat BODY: curl arguments for a chat completion
  printf '%s\0' -X POST "$GATEWAY/v1/chat/completions" -H 'Content-Type: application/json' -d "$1"
}
ask() { printf '{"model":"%s","messages":[{"role":"user","content":"What does a gateway do?"}]%s}' "$1" "${2:-}"; }

check "the models"                  200 '"id":"smart"'            "$GATEWAY/v1/models"
mapfile -d '' args < <(chat "$(ask fast)")
check "fast, through mock-a"        200 'This is mock-a answering as mock-small' "${args[@]}"
mapfile -d '' args < <(chat "$(ask smart ',"max_tokens":5')")
check "smart, through mock-b"       200 '"finish_reason":"length"' "${args[@]}"
mapfile -d '' args < <(chat "$(ask gpt-9)")
check "an unknown model"            404 '"code":"model_not_found"' "${args[@]}"
mapfile -d '' args < <(chat "$(ask fast ',"stream":true')")
check "streaming"                   200 'data: [DONE]'            -N "${args[@]}"
mapfile -d '' args < <(chat '{"model":"fast","model":"smart","messages":[]}')
check "a model given twice"         400 '"code":"invalid_json"'   "${args[@]}"
check "a form post"                 415 '"code":"unsupported_media_type"' \
  -X POST "$GATEWAY/v1/chat/completions" -d "$(ask fast)"

sdk_python=${SDK_PYTHON:-}
if [[ -z $sdk_python && -x .venv/bin/python ]] && .venv/bin/python -c 'import openai' 2>/dev/null; then
  sdk_python=.venv/bin/python
fi
if [[ -n $sdk_python ]]; then
  "$sdk_python" scripts/sdk_check.py || failures=$((failures + 1))
else
  echo "skip  the OpenAI SDK checks: no SDK_PYTHON and no .venv with openai"
fi

# smart's standby is mock-a: with mock-b gone, the gateway falls back to it.
kill "$mock_b"
wait "$mock_b" 2>/dev/null || true
mapfile -d '' args < <(chat "$(ask smart)")
check "smart, with mock-b down"     200 'This is mock-a answering as mock-large' "${args[@]}"

# mock-b again, now breaking every answer off part-way. A stream through it has started when it breaks, so the
# client can't be given a 502 or another provider's answer: it gets an error event, and the transfer is cut (curl's
# exit status 18, "transfer closed with outstanding read data remaining").
bin/mockprovider -addr 127.0.0.1:8119 -name mock-b -ttft 10ms -per-token 1ms -cut-rate 1 2>run/mock-b-cut.log &
pids+=($!)
for _ in $(seq 100); do curl -sf http://127.0.0.1:8119/livez >/dev/null 2>&1 && break; sleep 0.1; done
mapfile -d '' args < <(chat "$(ask smart ',"stream":true')")
curl_exit=0
status=$(curl -s -N -o run/body -w '%{http_code}' "${args[@]}") || curl_exit=$?
if [[ $status == 200 && $curl_exit == 18 ]] && grep -qF '"code":"upstream_unavailable"' run/body &&
  ! grep -qF '[DONE]' run/body; then
  printf 'ok    %-34s %s, curl exit %s\n' "a stream that breaks" "$status" "$curl_exit"
else
  printf 'FAIL  %-34s %s, curl exit %s: %s\n' "a stream that breaks" "$status" "$curl_exit" "$(tail -c 300 run/body)"
  failures=$((failures + 1))
fi
if [[ -n $sdk_python ]]; then
  "$sdk_python" scripts/sdk_check.py cut || failures=$((failures + 1))
fi

echo
echo "The gateway's log (run/switchboard.log), one line per request:"
grep '"chat completion"' run/switchboard.log | sed 's/^{"time":"[^"]*",//' | cut -c1-200

# Day 3: client keys, a rate limit, a cache, accounting. The gateway again, with keys made for this run.
echo
kill "${pids[2]}"
wait "${pids[2]}" 2>/dev/null || true
if ! (exec 3<>"/dev/tcp/127.0.0.1/$REDIS_PORT") 2>/dev/null; then
  redis-server --port "$REDIS_PORT" --bind 127.0.0.1 --daemonize yes --save "" --appendonly no --dir "$PWD/run" \
    --logfile "$PWD/run/redis.log"
  started_redis=1
  for _ in $(seq 50); do (exec 3<>"/dev/tcp/127.0.0.1/$REDIS_PORT") 2>/dev/null && break; sleep 0.1; done
fi
newkey() { # newkey NAME: sets NAME_key and NAME_hash
  local out
  out=$(bin/switchboard keygen)
  printf -v "$1_key" '%s' "$(awk '$1 == "key" { print $2 }' <<<"$out")"
  printf -v "$1_hash" '%s' "$(awk '$1 == "key_sha256" { print $2 }' <<<"$out")"
}
newkey alice
newkey bob
newkey carol
cat >run/day3.json <<JSON
{
  "listen": "127.0.0.1:8108",
  "redis": {"addr": "127.0.0.1:$REDIS_PORT", "prefix": "$prefix"},
  "clients": [
    {"name": "alice", "key_sha256": "$alice_hash", "rate_limit": {"requests_per_minute": 60, "burst": 2}},
    {"name": "bob", "key_sha256": "$bob_hash"},
    {"name": "carol", "key_sha256": "$carol_hash", "rate_limit": {"requests_per_minute": 60, "burst": 1}}
  ],
  "providers": [{"name": "mock-a", "base_url": "http://127.0.0.1:8118/v1", "timeout": "10s"}],
  "models": [
    {"name": "fast", "provider": "mock-a", "upstream_model": "mock-small", "cache": {"ttl": "1m"}},
    {"name": "plain", "provider": "mock-a", "upstream_model": "mock-small"}
  ],
  "prices": [
    {"provider": "mock-a", "upstream_model": "mock-small", "input_per_million": 0.15, "output_per_million": 0.6}
  ]
}
JSON
bin/switchboard serve -config run/day3.json 2>run/switchboard-day3.log &
pids+=($!)
for _ in $(seq 100); do curl -sf "$GATEWAY/readyz" >/dev/null 2>&1 && break; sleep 0.1; done

as() { printf '%s\0' -H "Authorization: Bearer $1"; }
mapfile -d '' args < <(chat "$(ask fast)")
check "no key"                      401 '"code":"missing_api_key"' "${args[@]}"
mapfile -d '' auth < <(as "sb-not-a-key")
check "a key it doesn't know"       401 '"code":"invalid_api_key"' "${auth[@]}" "${args[@]}"
mapfile -d '' auth < <(as "$alice_key")
check "alice"                       200 'This is mock-a answering' -D run/headers "${auth[@]}" "${args[@]}"
grep -qi '^x-ratelimit-remaining-requests: 1' run/headers || { echo "FAIL  alice's remaining requests"; failures=$((failures + 1)); }
check "alice again, from the cache" 200 'This is mock-a answering' "${auth[@]}" "${args[@]}"
check "alice's third: over her limit" 429 '"code":"rate_limit_exceeded"' -D run/headers "${auth[@]}" "${args[@]}"
grep -qi '^retry-after: 1' run/headers || { echo "FAIL  alice's Retry-After"; failures=$((failures + 1)); }
mapfile -d '' auth < <(as "$bob_key")
mapfile -d '' args < <(chat "$(ask fast ',"max_tokens":7')")
check "bob, a miss"                 200 '"completion_tokens":7' -D run/headers "${auth[@]}" "${args[@]}"
grep -qi '^x-switchboard-cache: miss' run/headers || { echo "FAIL  bob's miss"; failures=$((failures + 1)); }
check "bob again, a hit"            200 '"completion_tokens":7' -D run/headers "${auth[@]}" "${args[@]}"
grep -qi '^x-switchboard-cache: hit' run/headers || { echo "FAIL  bob's hit"; failures=$((failures + 1)); }
mapfile -d '' args < <(chat "$(ask plain ',"stream":true')")
check "a stream, usage not asked"   200 'data: [DONE]' -N "${auth[@]}" "${args[@]}"
if grep -qF '"usage"' run/body; then echo "FAIL  the stream had a usage the client didn't ask for"; failures=$((failures + 1)); fi
if [[ -n $sdk_python ]]; then
  SB_KEY_CAROL=$carol_key "$sdk_python" scripts/sdk_check.py day3 || failures=$((failures + 1))
fi
echo
bin/switchboard usage -config run/day3.json | tee run/usage.txt
grep -qE '^ +bob +fast +1 +1 ' run/usage.txt || { echo "FAIL  bob's totals"; failures=$((failures + 1)); }
echo
echo "The day-3 gateway's log (run/switchboard-day3.log):"
grep '"chat completion"' run/switchboard-day3.log | sed 's/^{"time":"[^"]*",//' | cut -c1-200
if ((failures > 0)); then
  echo "$failures checks failed"
  exit 1
fi
echo "all checks passed"
