#!/usr/bin/env bash
# The release check (README, "Release check"): what CI runs (make check: gofmt, vet, the cross builds, the tests and
# every benchmark once), the same tests again on a clean export of the commit (so they need nothing that isn't
# committed), the built binary's version against the commit's release tag, and the demo of the real binaries; the full
# check adds the race detector on two cores, a short run of every fuzz test and the coverage. Each step's output goes to run/release/<time>/<step>.txt; the check stops at the
# first step that fails.
#
#   scripts/release_check.sh          all of it (about 3.5 minutes on an M3)
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

# step NAME COMMAND...: runs the command (often a function below) with its output in NAME.txt, and stops the check if
# any command in it fails. It runs in the background and is waited for, not tested in an if: bash ignores set -e
# inside an if's condition, functions and subshells included, so a step reported only its last command's status (a
# failing fuzz target but the last, a vet failure followed by passing tests: found by the day-5 second review).
step() {
  local name="$1"
  shift
  local start=$SECONDS status=0
  echo "== $name: $*"
  (
    set -e
    "$@"
  ) >"$dir/$name.txt" 2>&1 &
  wait $! || status=$?
  if [ "$status" -ne 0 ]; then
    echo "   FAILED (exit $status) after $((SECONDS - start)) s; see $dir/$name.txt"
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
# nothing changed, exactly that tag. At a tag, the tree must be clean: any change, an untracked file included, makes
# the go command stamp "+dirty" (a stray file once failed this step while the warning about changes said nothing).
# Off a tag it prints the version and says it isn't compared.
versions() {
  go build -o "$dir/bin/" ./cmd/switchboard
  local line tags changes
  line="$("$dir/bin/switchboard" version)"
  echo "$line"
  tags="$(git tag --points-at HEAD --list 'v[0-9]*')" # every one: git describe would name only one of two
  changes="$(git status --porcelain)"
  if [ -z "$tags" ]; then
    echo "HEAD has no release tag: not compared"
    return 0
  fi
  if [ -n "$changes" ]; then
    echo "HEAD is tagged $(echo $tags), but the tree has changes, so the build is +dirty; commit or remove them:"
    echo "$changes"
    return 1
  fi
  local tag
  for tag in $tags; do
    if [[ "$line" == "switchboard $tag ("* ]]; then
      echo "matches HEAD's tag $tag"
      return 0
    fi
  done
  echo "HEAD is tagged $(echo $tags), but the binary says: $line"
  return 1
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

# Every fuzz test for 15 s (their seeds already run in make test). go test fuzzes one target at a time, and a name
# that matches none passes with only a warning, so a renamed target would silently stop being fuzzed: that fails here.
fuzz() {
  local pkg target out
  while read -r pkg target; do
    if ! out="$(go test -run '^$' -fuzz "^$target\$" -fuzztime 15s "$pkg" 2>&1)"; then
      echo "$out" # what it found, before the step fails
      return 1
    fi
    echo "$out"
    if grep -q 'no fuzz tests to fuzz' <<<"$out"; then
      echo "no fuzz test $target in $pkg"
      return 1
    fi
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
  go tool cover -func="$dir/cover.out" | tail -n 1 | tr -s "\t" " "
}

echo "release check of $(git rev-parse --short=12 HEAD) ($(git describe --tags --always --dirty)), $(date)"
if [ -n "$(git status --porcelain)" ]; then
  echo "warning: the tree has changes: the clean export checks the commit, every other step the working tree"
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

step race-2-cores env GOMAXPROCS=2 go test -race -count=1 -timeout 5m ./...
step fuzz fuzz
step coverage coverage
conditions >"$dir/conditions-end.txt" 2>&1
echo "coverage: $(tail -n 1 "$dir/coverage.txt")"
echo "all steps passed; the outputs are in $dir"
