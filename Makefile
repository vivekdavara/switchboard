.PHONY: bench build check cross demo fuzz lint redis redis-stop test

# The Redis the tests use (day 3): a project-local one on 6382, started by `make test` if none answers there, keeping
# nothing on disk. Tests that need Redis are skipped when SWITCHBOARD_TEST_REDIS is unset (a plain `go test ./...`),
# and fail when it is set and nothing answers. `make redis-stop` stops it.
REDIS_PORT ?= 6382
export SWITCHBOARD_TEST_REDIS ?= 127.0.0.1:$(REDIS_PORT)

# What CI runs.
check: lint test

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

# The real binaries: two mock providers and the gateway on 8108/8118/8119, eight requests checked (CI's smoke job).
demo:
	scripts/demo.sh

# README, "Results": the gateway's overhead (sequential, two request shapes), its connection pool (32 at once), and
# its own work on a body (parse, check, rewrite).
# The pool runs are short because Go's default of 2 idle connections leaves about one socket in TIME_WAIT per two
# requests, for 30 s on macOS, out of 16,384 ephemeral ports. Needs a quiet machine.
# Day 2 adds a streamed answer straight and through the gateway, a model whose first provider is dead (refusing or
# hanging until its 10 ms timeout) with breakers off and on, and the router's, budget's and breaker's cost per call.
bench:
	go test -run '^$$' -bench Overhead -benchtime 20000x -count 6 ./internal/gateway
	go test -run '^$$' -bench Pool -benchtime 3000x -count 6 ./internal/gateway
	go test -run '^$$' -bench ParseAndRewrite -benchtime 20000x -count 6 ./internal/openai
	go test -run '^$$' -bench Stream -benchtime 5000x -count 6 ./internal/gateway
	go test -run '^$$' -bench DeadProvider -benchtime 500x -count 6 ./internal/gateway
	go test -run '^$$' -bench . -benchtime 1000000x -count 6 ./internal/route ./internal/budget ./internal/breaker

# Builds for the other platforms, as CI does: a Unix-only call (syscall.Kill) once broke the Windows build unnoticed.
cross:
	GOOS=windows GOARCH=amd64 go build -o /dev/null ./...
	GOOS=linux GOARCH=arm64 go build -o /dev/null ./...
	GOOS=darwin GOARCH=arm64 go build -o /dev/null ./...
