.PHONY: bench build check demo fuzz lint test

# What CI runs.
check: lint test

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test:
	go test -race -count=1 -timeout 3m ./...

build:
	go build -o bin/ ./cmd/...

# A minute of each fuzz test: the strict JSON reader, and the rewrite read back as a Go provider would (their seeds
# run in every make test). go test fuzzes one target at a time.
fuzz:
	go test -run '^$$' -fuzz FuzzParseObject -fuzztime 60s ./internal/jsonstrict
	go test -run '^$$' -fuzz FuzzRewriteKeepsTheMeaning -fuzztime 60s ./internal/openai

# The real binaries: two mock providers and the gateway on 8108/8118/8119, eight requests checked (CI's smoke job).
demo:
	scripts/demo.sh

# README, "Results": the gateway's overhead (sequential, two request shapes), its connection pool (32 at once), and
# its own work on a body (parse, check, rewrite).
# The pool runs are short because Go's default of 2 idle connections leaves about one socket in TIME_WAIT per two
# requests, for 30 s on macOS, out of 16,384 ephemeral ports. Needs a quiet machine.
bench:
	go test -run '^$$' -bench Overhead -benchtime 20000x -count 6 ./internal/gateway
	go test -run '^$$' -bench Pool -benchtime 3000x -count 6 ./internal/gateway
	go test -run '^$$' -bench ParseAndRewrite -benchtime 20000x -count 6 ./internal/openai
