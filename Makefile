.PHONY: bench bench-smoke build chaos check cross demo fuzz lint load redis redis-stop release-check test

# The Redis the tests use (day 3): a project-local one on 6382, started by `make test` if none answers there, keeping
# nothing on disk. Tests that need Redis are skipped when SWITCHBOARD_TEST_REDIS is unset (a plain `go test ./...`),
# and fail when it is set and nothing answers. `make redis-stop` stops it.
REDIS_PORT ?= 6382
export SWITCHBOARD_TEST_REDIS ?= 127.0.0.1:$(REDIS_PORT)

# What CI's test job runs: gofmt and vet, the cross builds, the tests, and every benchmark once (the cross builds and
# the benchmarks were CI's alone until the day-5 review, so a Windows-only break could pass make check).
check: lint cross test bench-smoke

# gofmt skips .scratch/, where review worktrees live (go vet's ./... skips dot-directories by itself): a review's
# half-written file there once failed this check.
lint:
	@unformatted="$$(find . -path ./.scratch -prune -o -name '*.go' -print | xargs gofmt -l)"; if [ -n "$$unformatted" ]; then echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test: redis
	go test -race -count=1 -timeout 3m ./...

redis:
	@redis-cli -p $(REDIS_PORT) ping >/dev/null 2>&1 || { mkdir -p run && \
		redis-server --port $(REDIS_PORT) --bind 127.0.0.1 --daemonize yes --save "" --appendonly no \
			--dir "$(CURDIR)/run" --logfile "$(CURDIR)/run/redis.log" && \
		for i in 1 2 3 4 5 6 7 8 9 10; do redis-cli -p $(REDIS_PORT) ping >/dev/null 2>&1 && break; sleep 0.1; done; }

redis-stop:
	-redis-cli -p $(REDIS_PORT) shutdown nosave

build:
	go build -o bin/ ./cmd/...

# A minute of each fuzz test: the strict JSON reader, the rewrite read back as a Go provider would (and asking for a
# stream's usage, day 3), and the stream's event reader against a whole-input reference (their seeds run in every make
# test). go test fuzzes one target at a time.
fuzz:
	go test -run '^$$' -fuzz FuzzParseObject -fuzztime 60s ./internal/jsonstrict
	go test -run '^$$' -fuzz FuzzRewriteKeepsTheMeaning -fuzztime 60s ./internal/openai
	go test -run '^$$' -fuzz FuzzAskingForUsageKeepsTheRest -fuzztime 60s ./internal/openai
	go test -run '^$$' -fuzz FuzzEventReaderAgreesWithTheReference -fuzztime 60s ./internal/gateway
	go test -run '^$$' -fuzz FuzzUnaskedsFastPathAgreesWithTheParse -fuzztime 60s ./internal/gateway

# The real binaries: two mock providers and the gateway on 8108/8118/8119, then the day-3 gateway with client keys and
# Redis: curl's checks, the OpenAI SDK's (when installed) and the metrics against the requests (CI's smoke job).
demo:
	scripts/demo.sh

# Every benchmark still runs: 50 iterations each, not a measurement (CI's "Benchmarks run" step).
bench-smoke:
	go test -run '^$$' -bench . -benchtime 50x ./internal/gateway ./internal/openai ./internal/route ./internal/budget ./internal/breaker ./internal/ratelimit

# README, "Results": the gateway's overhead (sequential, two request shapes), its connection pool (32 at once), and
# its own work on a body (parse, check, rewrite).
# The pool runs are short because Go's default of 2 idle connections leaves about one socket in TIME_WAIT per two
# requests, for 30 s on macOS, out of 16,384 ephemeral ports. Needs a quiet machine.
# Day 2 adds a streamed answer straight and through the gateway, a model whose first provider is dead (refusing or
# hanging until its 10 ms timeout) with breakers off and on, and the router's, budget's and breaker's cost per call.
# Day 3 adds a request with a client key, the totals, the rate limit, and a cache miss and hit (the Redis round trips
# are to the local Redis that make test starts), taking the usage out of a chunk, and one rate-limit check alone.
# Day 4 adds the metrics' and the log line's cost at a request's end (observe, logExchange).
bench: redis
	go test -run '^$$' -bench Overhead -benchtime 20000x -count 6 ./internal/gateway
	go test -run '^$$' -bench Pool -benchtime 3000x -count 6 ./internal/gateway
	go test -run '^$$' -bench ParseAndRewrite -benchtime 20000x -count 6 ./internal/openai
	go test -run '^$$' -bench Stream -benchtime 5000x -count 6 ./internal/gateway
	go test -run '^$$' -bench DeadProvider -benchtime 500x -count 6 ./internal/gateway
	go test -run '^$$' -bench . -benchtime 1000000x -count 6 ./internal/route ./internal/budget ./internal/breaker
	go test -run '^$$' -bench Day3 -benchtime 5000x -count 6 ./internal/gateway
	go test -run '^$$' -bench Unasked -benchtime 1000000x -count 6 ./internal/gateway
	go test -run '^$$' -bench Take -benchtime 20000x -count 6 ./internal/ratelimit
	go test -run '^$$' -bench 'Observe|LogLine' -benchtime 200000x -count 6 ./internal/gateway

# Day 4: the real binaries under load against the mocks (README "Results (day 4)"; each case RUNS times, 3 by
# default), and the chaos runs (a provider killed, a provider hung, Redis hung then shut down, under steady load; it
# runs its own Redis on 6382, so it shuts down any other there first). Both need a quiet machine.
load: redis
	scripts/load.sh

chaos:
	scripts/chaos.sh

# Builds for the other platforms, as CI does: a Unix-only call (syscall.Kill) once broke the Windows build unnoticed.
cross:
	GOOS=windows GOARCH=amd64 go build -o /dev/null ./...
	GOOS=linux GOARCH=arm64 go build -o /dev/null ./...
	GOOS=darwin GOARCH=arm64 go build -o /dev/null ./...

# Day 5: the release check (README "Release check"): make check, the tests on a clean export of HEAD, the binary's
# version against HEAD's release tag, the demo, then cross builds, every benchmark run once, the race detector on two
# cores, 15 s of each fuzz test and the coverage. `scripts/release_check.sh quick` stops after the demo.
release-check: redis
	scripts/release_check.sh
