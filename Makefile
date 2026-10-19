.PHONY: bench build check cross demo fuzz lint test

# What CI runs.
check: lint test

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test:
	go test -race -count=1 -timeout 3m ./...

build:
	go build -o bin/ ./cmd/...

# A minute of each fuzz test: the strict JSON reader, the rewrite read back as a Go provider would, and the stream's
# event reader against a whole-input reference (their seeds run in every make test). go test fuzzes one target at a
# time.
fuzz:
	go test -run '^$$' -fuzz FuzzParseObject -fuzztime 60s ./internal/jsonstrict
	go test -run '^$$' -fuzz FuzzRewriteKeepsTheMeaning -fuzztime 60s ./internal/openai
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
