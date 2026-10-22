#!/usr/bin/env bash
# Every measurement in README "Results (v1.0.0)", run again at one commit and kept: the load test (RUNS=3
# scripts/load.sh), the chaos runs (scripts/chaos.sh), and the in-process benchmarks of the gateway's overhead, day 3's
# Redis steps and day 4's metrics and log line (medians of 6 by scripts/bench_medians.py). Their raw outputs and the
# machine's conditions at the start and end go to results/NAME/, which is meant to be committed with the README's
# numbers (each a few kilobytes).
#
#   scripts/results.sh v1.0.0      about 6 minutes on an M3
#
# Needs a quiet machine (nothing else using the CPU: no build, test or review running), the ports of load.sh and
# chaos.sh free (8108, 8118, 8119, 9108), and Redis on 6382 free to be stopped and started (chaos.sh shuts down the one
# there and starts its own; make redis starts the benchmarks' afterwards).
set -euo pipefail

cd "$(dirname "$0")/.."
name="${1:?usage: scripts/results.sh NAME (results/NAME/ gets the outputs)}"
dir="results/$name"
mkdir -p "$dir/chaos"
# BenchmarkDay3's Redis cases need it (unset, they would be skipped and the table would just lack them).
export SWITCHBOARD_TEST_REDIS="${SWITCHBOARD_TEST_REDIS:-127.0.0.1:6382}"

conditions() {
  date
  echo "commit $(git rev-parse HEAD) ($(git describe --tags --always --dirty))"
  go version
  redis-server --version 2>/dev/null || true
  if sysctl -n machdep.cpu.brand_string >/dev/null 2>&1; then
    sysctl -n machdep.cpu.brand_string
    sw_vers -productVersion
    pmset -g batt | sed -n 1,2p # sed reads it all: head could end the pipe early, which pipefail would fail
    pmset -g | grep -i lowpowermode || true
  else
    grep -m1 'model name' /proc/cpuinfo || true
    uname -sr
  fi
  uptime
}

echo "results of $(git rev-parse --short=12 HEAD) into $dir, $(date)"
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "warning: tracked files have changes; the numbers are the working tree's, not the commit's"
fi
conditions >"$dir/conditions-start.txt" 2>&1

make redis >/dev/null
echo "== load (RUNS=3 scripts/load.sh)"
RUNS=3 scripts/load.sh >"$dir/load.txt" 2>&1
cp run/load/results.jsonl "$dir/load.jsonl"
cp run/load/redis-usage.txt "$dir/load-redis-usage.txt"
python3 scripts/load_table.py "$dir/load.jsonl" | tee "$dir/load-table.md"

echo "== chaos (scripts/chaos.sh)"
scripts/chaos.sh >"$dir/chaos.txt" 2>&1
cp run/chaos/events.txt run/chaos/*.delta "$dir/chaos/"
for phase in provider-killed provider-hung redis-hung; do cp "run/chaos/$phase.txt" "$dir/chaos/$phase-timeline.txt"; done
cat "$dir/chaos/events.txt"

echo "== benchmarks"
make redis >/dev/null
{
  go test -run '^$' -bench Overhead -benchtime 20000x -count 6 ./internal/gateway
  go test -run '^$' -bench Day3 -benchtime 5000x -count 6 ./internal/gateway
  go test -run '^$' -bench 'Observe|LogLine' -benchtime 200000x -count 6 ./internal/gateway
} >"$dir/bench.txt" 2>&1
python3 scripts/bench_medians.py <"$dir/bench.txt" | tee "$dir/bench-medians.txt"

conditions >"$dir/conditions-end.txt" 2>&1
echo "done; the outputs are in $dir"
