.PHONY: build test integration check
build:
	go build -trimpath -o bin/agent-context ./cmd/agent-context
test:
	go test ./...
integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo 'TEST_DATABASE_URL is required'; exit 1)
	go test -race -count=1 ./...
check:
	@test -z "$$(gofmt -l cmd internal)"
	go vet ./...
