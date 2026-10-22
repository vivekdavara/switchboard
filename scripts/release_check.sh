#!/usr/bin/env bash
# The release check (README, "Release check"): what CI runs, the same tests again on a clean export of the commit (so
# they need nothing that isn't committed), the built binary's version against the commit's release tag, the demo of
# the real binaries, and every benchmark run once; the full check adds the race detector on two cores, a short run of
# every fuzz test and the coverage. Each step's output goes to run/release/<time>/<step>.txt; the check stops at the
# first step that fails.
#
#   scripts/release_check.sh          all of it (about 4 minutes on an M3)
#   scripts/release_check.sh quick    the tests, the clean export, the version and the demo (about a minute)
#
# It measures nothing: README's numbers come from `make bench`, `scripts/load.sh` and `scripts/chaos.sh`. It needs the
# demo's ports free (8108, 8118, 8119, 9108) and a Redis on 6382 or none (make test starts one there; the demo uses
# whichever answers). conditions-start.txt and conditions-end.txt record the machine: the commit, the CPU, the OS, the
# power and the load average.
set -euo pipefail

cd "$(dirname "$0")/.."
dir="run/release/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$dir"
export SWITCHBOARD_TEST_REDIS="${SWITCHBOARD_TEST_REDIS:-127.0.0.1:6382}"

step() {
  local name="$1"
  shift
  local start=$SECONDS
  echo "== $name: $*"
  if ! "$@" >"$dir/$name.txt" 2>&1; then
    echo "   FAILED after $((SECONDS - start)) s; see $dir/$name.txt"
    exit 1
  fi
  echo "   ok in $((SECONDS - start)) s"
}

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

# The binary's `switchboard version` names the module version the go command stamps from git: at a release tag, with
# nothing changed, exactly that tag. Anywhere else it prints the version and says why it isn't checked.
versions() {
  go build -o "$dir/bin/" ./cmd/switchboard
  local line tag
  line="$("$dir/bin/switchboard" version)"
  echo "$line"
  tag="$(git describe --exact-match --tags --match 'v[0-9]*' HEAD 2>/dev/null || true)"
  if [ -z "$tag" ]; then
    echo "HEAD has no release tag: not compared"
  elif [ -n "$(git status --porcelain --untracked-files=no)" ]; then
    echo "HEAD is $tag, but tracked files have changes: not compared"
  elif [[ "$line" != "switchboard $tag ("* ]]; then
    echo "HEAD is $tag, but the binary says: $line"
    return 1
  else
    echo "matches HEAD's tag $tag"
  fi
}

# Tests on a copy of exactly what is committed: a file that was never added, or one gofmt would change, fails here.
clean_export() {
  (
    cd "$export_dir"
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
      echo "not gofmt-formatted:"
      echo "$unformatted"
      exit 1
    fi
    go vet ./...
    go test -race -count=1 -timeout 3m ./...
  )
}

# CI's "Benchmarks run" step: every benchmark still runs (50 iterations: not a measurement).
bench_smoke() {
  go test -run '^$' -bench . -benchtime 50x ./internal/gateway ./internal/openai ./internal/route ./internal/budget \
    ./internal/breaker ./internal/ratelimit
}

# Every fuzz test for 15 s (their seeds already run in make test). go test fuzzes one target at a time.
fuzz() {
  local pkg target
  while read -r pkg target; do
    go test -run '^$' -fuzz "^$target\$" -fuzztime 15s "$pkg"
  done <<'EOF'
./internal/jsonstrict FuzzParseObjectAgreesWithItsOwnOutput
./internal/openai FuzzRewriteKeepsTheMeaning
./internal/openai FuzzAskingForUsageKeepsTheRest
./internal/gateway FuzzEventReaderAgreesWithTheReference
./internal/gateway FuzzUnaskedsFastPathAgreesWithTheParse
EOF
}

coverage() {
  go test -count=1 -coverpkg=./... -coverprofile="$dir/cover.out" ./... >/dev/null
  go tool cover -func="$dir/cover.out" | tail -n 1
}

echo "release check of $(git rev-parse --short=12 HEAD) ($(git describe --tags --always --dirty)), $(date)"
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "warning: tracked files have changes: the clean export checks the commit, every other step the working tree"
fi
conditions >"$dir/conditions-start.txt" 2>&1

export_dir="$(mktemp -d)"
# `|| true`: under set -e a failing command stops a trap, which would skip the removal and fail the script.
trap 'rm -rf "$export_dir" || true' EXIT
git archive HEAD | tar -x -C "$export_dir"

step check make check
step version versions
step clean-export clean_export
step demo scripts/demo.sh
if [ "${1:-}" = quick ]; then
  conditions >"$dir/conditions-end.txt" 2>&1
  echo "quick check passed; the outputs are in $dir"
  exit 0
fi

step cross make cross
step bench-smoke bench_smoke
step race-2-cores env GOMAXPROCS=2 go test -race -count=1 -timeout 5m ./...
step fuzz fuzz
step coverage coverage
conditions >"$dir/conditions-end.txt" 2>&1
echo "coverage: $(tail -n 1 "$dir/coverage.txt")"
echo "all steps passed; the outputs are in $dir"
