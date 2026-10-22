#!/usr/bin/env bash
# Day 4's chaos runs (README "Results (day 4)"): steady load while a dependency goes down and comes back. Each phase
# is an open loop of 200 requests a second for 40 s (loadgen -rate 200 -n 8000) with a 1 s timeline, the gateway's
# metrics scraped before and after it (scripts/metrics_delta.py prints what changed). Phases:
#
#   provider-killed  model "smart": mock-b first, mock-a its fallback (weight 0). mock-b is killed at 10 s
#                    (connections refused) and started again at 25 s.
#   provider-hung    the same, but mock-b is stopped (SIGSTOP) at 10 s: the kernel still accepts connections, nothing
#                    answers, and each attempt waits out the provider's 2 s timeout. Continued (SIGCONT) at 25 s.
#   redis-hung       model "fast" with a client key, a rate limit (never reached), a cache (each request different, so
#                    every lookup misses) and the totals. Redis is stopped (SIGSTOP) at 10 s and continued at 20 s,
#                    then shut down at 25 s (connections refused) and started again at 32 s.
#
# PHASES picks some (default all three, in that order); PROVIDER_TIMEOUT sets the providers' timeout (default 2s), so
# the hung phase can be run with another: PHASES=provider-hung PROVIDER_TIMEOUT=500ms scripts/chaos.sh.
#
# The mocks answer in about 52 ms (20 ms to the first token, 1 ms a token). The breaker and the retry budget are the
# defaults (5 failures in a row open it for 10 s; retries up to 20% of requests plus 1 a second). Redis runs on 6382,
# started here (one already there, such as make test's, is shut down first). Output in run/chaos/.
set -euo pipefail
cd "$(dirname "$0")/.."
out=run/chaos
mkdir -p "$out" bin
go build -o bin/ ./cmd/...
METRICS=http://127.0.0.1:9108/metrics
PHASES=${PHASES:-provider-killed provider-hung redis-hung}
PROVIDER_TIMEOUT=${PROVIDER_TIMEOUT:-2s}

# Every port must be free first: wait_for only curls a port, so a gateway or mock left from another run (or another
# session) would answer in place of the one just started, and be measured (a hypothesis of the day-4 review).
for port in 8108 8118 8119 9108; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "port $port is already in use; stop whatever listens there first" >&2
    exit 1
  fi
done

pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill -CONT "$pid" 2>/dev/null || true; kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

wait_for() {
  for _ in $(seq 100); do curl -sf "$1" >/dev/null 2>&1 && return; sleep 0.05; done
  echo "$1 never answered" >&2
  exit 1
}

mock() { # mock NAME PORT
  bin/mockprovider -addr "127.0.0.1:$2" -name "$1" -ttft 20ms -per-token 1ms 2>>"$out/$1.log" &
  pids+=($!)
  wait_for "http://127.0.0.1:$2/livez"
}

redis() {
  redis-server --port 6382 --bind 127.0.0.1 --save "" --appendonly no --dir "$PWD/$out" \
    --logfile "$PWD/$out/redis.log" &
  redis_pid=$!
  pids+=($redis_pid)
  for _ in $(seq 100); do redis-cli -p 6382 ping >/dev/null 2>&1 && return; sleep 0.05; done
  echo "redis never answered" >&2
  exit 1
}

redis-cli -p 6382 shutdown nosave >/dev/null 2>&1 || true
key="sb-chaos-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
hash=$(printf '%s' "$key" | shasum -a 256 | cut -d' ' -f1)
export CHAOS_KEY=$key
cat >"$out/chaos.json" <<JSON
{
  "listen": "127.0.0.1:8108",
  "metrics": {"listen": "127.0.0.1:9108"},
  "log": {"level": "warn"},
  "redis": {"addr": "127.0.0.1:6382", "prefix": "switchboard-chaos:"},
  "clients": [{"name": "chaos", "key_sha256": "$hash",
    "rate_limit": {"requests_per_minute": 1000000, "burst": 100000}}],
  "providers": [
    {"name": "mock-a", "base_url": "http://127.0.0.1:8118/v1", "timeout": "$PROVIDER_TIMEOUT"},
    {"name": "mock-b", "base_url": "http://127.0.0.1:8119/v1", "timeout": "$PROVIDER_TIMEOUT"}
  ],
  "models": [
    {"name": "smart", "targets": [
      {"provider": "mock-b", "upstream_model": "mock-large", "weight": 1},
      {"provider": "mock-a", "upstream_model": "mock-large", "weight": 0}
    ]},
    {"name": "fast", "provider": "mock-a", "upstream_model": "mock-small", "cache": {"ttl": "1m"}}
  ]
}
JSON

redis
mock mock-a 8118
mock mock-b 8119
mock_b=${pids[-1]}
bin/switchboard serve -config "$out/chaos.json" 2>"$out/switchboard.log" &
pids+=($!)
wait_for http://127.0.0.1:8108/readyz

# phase NAME MODEL ACTIONS: the load in the background, ACTIONS (a function) meanwhile, then the timeline and the
# metrics' changes.
phase() {
  local name=$1 model=$2 actions=$3
  curl -s "$METRICS" >"$out/$name.before"
  bin/loadgen -model "$model" -rate 200 -n 8000 -c 1024 -warmup 200 -unique -key-env CHAOS_KEY -timeout 10s \
    -timeline 1s >"$out/$name.txt" &
  local load=$!
  $actions
  wait "$load"
  curl -s "$METRICS" >"$out/$name.after"
  echo "== $name"
  cat "$out/$name.txt"
  python3 scripts/metrics_delta.py "$out/$name.before" "$out/$name.after" | tee "$out/$name.delta"
}

# The warmup takes about a second at 200 a second; the timeline starts after it.
killed() {
  sleep 11
  kill "$mock_b"; wait "$mock_b" 2>/dev/null || true
  echo "$(date +%T) mock-b killed" >>"$out/events.txt"
  sleep 15
  mock mock-b 8119
  mock_b=${pids[-1]}
  echo "$(date +%T) mock-b started" >>"$out/events.txt"
}
hung() {
  sleep 11
  kill -STOP "$mock_b"
  echo "$(date +%T) mock-b stopped" >>"$out/events.txt"
  sleep 15
  kill -CONT "$mock_b"
  echo "$(date +%T) mock-b continued" >>"$out/events.txt"
}
redis_down() {
  sleep 11
  kill -STOP "$redis_pid"
  echo "$(date +%T) redis stopped" >>"$out/events.txt"
  sleep 10
  kill -CONT "$redis_pid"
  echo "$(date +%T) redis continued" >>"$out/events.txt"
  sleep 5
  redis-cli -p 6382 shutdown nosave >/dev/null 2>&1 || true
  wait "$redis_pid" 2>/dev/null || true
  echo "$(date +%T) redis shut down" >>"$out/events.txt"
  sleep 7
  redis
  echo "$(date +%T) redis started" >>"$out/events.txt"
}

: >"$out/events.txt"
first=1
for name in $PHASES; do
  # After a phase, the breaker's cooldown (10 s) and a probe's time, so the next one starts with it closed.
  [[ -n $first ]] || sleep 12
  first=
  case $name in
  provider-killed) phase provider-killed smart killed ;;
  provider-hung) phase provider-hung smart hung ;;
  redis-hung) phase redis-hung fast redis_down ;;
  *) echo "no phase $name" >&2; exit 2 ;;
  esac
done
cat "$out/events.txt"
