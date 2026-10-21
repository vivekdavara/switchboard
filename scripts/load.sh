#!/usr/bin/env bash
# Day 4's load test (README "Results (day 4)"): the real binaries on loopback. Two mock providers with no latency
# (8118, 8119), the gateway (8108; metrics on 9108) restarted for each case with its own config in run/load/, and
# loadgen. Each case runs RUNS times (3 by default; each run has its own warmup); every run's summary is a JSON line in
# run/load/results.jsonl, and scripts/load_table.py prints the medians. Cases:
#
#   direct-c1, direct-c32       straight to the mock: the baseline
#   gateway-c1, gateway-c32     through the gateway, no Redis (day 2's path), a log line per request
#   quiet-c32                   the same with the log at warn: no line per request
#   redis-c1, redis-c32         with a client key, a rate limit (never reached), and the usage totals in Redis
#   slow-direct, slow-gateway   mocks answering in ~360 ms (200 ms to the first token, 5 ms a token), an open loop at
#                               600 requests a second (about 216 in flight). A closed loop of 256 workers sends in
#                               waves of 256 at once (every answer takes the same time), and the median then measures
#                               the queue within a wave (+5 ms through the gateway in a trial run), not the gateway.
#
# Needs a quiet machine: nothing else running, or the numbers measure that instead. Redis on 6382 must be there for
# the redis cases (make redis starts it).
set -euo pipefail
cd "$(dirname "$0")/.."
RUNS=${RUNS:-3}
out=run/load
mkdir -p "$out" bin
go build -o bin/ ./cmd/...
: >"$out/results.jsonl"

pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  pids=()
}
trap cleanup EXIT

wait_for() {
  for _ in $(seq 100); do curl -sf "$1" >/dev/null 2>&1 && return; sleep 0.05; done
  echo "$1 never answered" >&2
  exit 1
}

mocks() { # mocks TTFT PER_TOKEN
  bin/mockprovider -addr 127.0.0.1:8118 -name mock-a -ttft "$1" -per-token "$2" 2>"$out/mock-a.log" &
  pids+=($!)
  bin/mockprovider -addr 127.0.0.1:8119 -name mock-b -ttft "$1" -per-token "$2" 2>"$out/mock-b.log" &
  pids+=($!)
  wait_for http://127.0.0.1:8118/livez
  wait_for http://127.0.0.1:8119/livez
}

# gateway NAME EXTRA: writes run/load/NAME.json (EXTRA spliced into its top level) and starts the gateway with it.
gateway() {
  cat >"$out/$1.json" <<JSON
{
  "listen": "127.0.0.1:8108",
  "metrics": {"listen": "127.0.0.1:9108"},
  $2
  "providers": [
    {"name": "mock-a", "base_url": "http://127.0.0.1:8118/v1", "timeout": "10s"},
    {"name": "mock-b", "base_url": "http://127.0.0.1:8119/v1", "timeout": "10s"}
  ],
  "models": [{"name": "fast", "provider": "mock-a", "upstream_model": "mock-small"}],
  "prices": [{"provider": "mock-a", "upstream_model": "mock-small", "input_per_million": 0.15, "output_per_million": 0.6}]
}
JSON
  bin/switchboard serve -config "$out/$1.json" 2>"$out/$1.log" &
  gw=$!
  pids+=($gw)
  wait_for http://127.0.0.1:8108/readyz
}

stop_gateway() { kill "$gw"; wait "$gw" 2>/dev/null || true; }

# measure CASE URL LOADGEN_ARGS...: RUNS runs, each a JSON line tagged with the case.
measure() {
  local name=$1 url=$2
  shift 2
  for run in $(seq "$RUNS"); do
    bin/loadgen -url "$url" -json "$@" | python3 -c "import json,sys; d=json.load(sys.stdin); d['case']='$name'; \
d['run']=$run; print(json.dumps(d))" | tee -a "$out/results.jsonl"
  done
}

DIRECT=http://127.0.0.1:8118/v1/chat/completions
GATEWAY=http://127.0.0.1:8108/v1/chat/completions

mocks 0s 0s
measure direct-c1 "$DIRECT" -model mock-small -c 1 -n 5000 -warmup 500
measure direct-c32 "$DIRECT" -model mock-small -c 32 -n 30000 -warmup 2000

gateway plain ""
measure gateway-c1 "$GATEWAY" -c 1 -n 5000 -warmup 500
measure gateway-c32 "$GATEWAY" -c 32 -n 30000 -warmup 2000
curl -s http://127.0.0.1:9108/metrics >"$out/plain.metrics"
stop_gateway

gateway quiet '"log": {"level": "warn"},'
measure quiet-c32 "$GATEWAY" -c 32 -n 30000 -warmup 2000
stop_gateway

# A key made for this run, never checked in; its hash goes in the config.
key="sb-load-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
hash=$(printf '%s' "$key" | shasum -a 256 | cut -d' ' -f1)
prefix="switchboard-load-$(date +%s)-$$:"
redis-cli -p 6382 ping >/dev/null
gateway redis "\"redis\": {\"addr\": \"127.0.0.1:6382\", \"prefix\": \"$prefix\"},
  \"clients\": [{\"name\": \"load\", \"key_sha256\": \"$hash\",
    \"rate_limit\": {\"requests_per_minute\": 1000000, \"burst\": 100000}}],"
export LOAD_KEY=$key
measure redis-c1 "$GATEWAY" -c 1 -n 5000 -warmup 500 -key-env LOAD_KEY
measure redis-c32 "$GATEWAY" -c 32 -n 30000 -warmup 2000 -key-env LOAD_KEY
bin/switchboard usage -config "$out/redis.json" >"$out/redis-usage.txt"
redis-cli -p 6382 --scan --pattern "$prefix*" | xargs -n 100 redis-cli -p 6382 del >/dev/null
cleanup

mocks 200ms 5ms
gateway slow ""
measure slow-direct "$DIRECT" -model mock-small -rate 600 -c 512 -n 9000 -warmup 512
measure slow-gateway "$GATEWAY" -rate 600 -c 512 -n 9000 -warmup 512
cleanup
python3 scripts/load_table.py "$out/results.jsonl"
