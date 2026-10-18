.PHONY: build check fuzz lint test

# What CI runs.
check: lint test

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test:
	go test -race -count=1 -timeout 3m ./...

build:
	go build -o bin/ ./cmd/...

# A minute of fuzzing the strict JSON reader (its seeds run in every make test).
fuzz:
	go test -run '^$$' -fuzz FuzzParseObject -fuzztime 60s ./internal/jsonstrict
