#!/usr/bin/env bash
# Runs the real binaries: two mock providers (8118, 8119) and the gateway (8108, configs/local.json), sends a few
# requests through the gateway, checks each answer, and stops everything. CI runs it as a smoke test; `make demo`
# runs it here. Logs go to run/ (gitignored). Needs curl.
#
# With the official OpenAI Python SDK it also runs scripts/sdk_check.py: set SDK_PYTHON to a Python that has
# scripts/requirements-sdk.txt installed (CI does), or have it in .venv (python3.11 -m venv .venv &&
# .venv/bin/pip install -r scripts/requirements-sdk.txt).
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=http://127.0.0.1:8108
mkdir -p run bin
go build -o bin/ ./cmd/...

pids=()
cleanup() {
  # Every step may fail here (a process already gone); none may stop the rest.
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
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
check "streaming (day 2)"           400 '"param":"stream"'        "${args[@]}"
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

kill "$mock_b"
wait "$mock_b" 2>/dev/null || true
mapfile -d '' args < <(chat "$(ask smart)")
check "smart, with mock-b down"     502 '"code":"upstream_unavailable"' "${args[@]}"

echo
echo "The gateway's log (run/switchboard.log), one line per request:"
grep '"chat completion"' run/switchboard.log | sed 's/^{"time":"[^"]*",//' | cut -c1-200
if ((failures > 0)); then
  echo "$failures checks failed"
  exit 1
fi
echo "all checks passed"
