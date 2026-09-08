VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: help run run-worker test check build docker-api docker-worker tidy

help:
	@echo "make run          start the API locally (needs DATABASE_URL)"
	@echo "make run-worker   start the worker locally"
	@echo "make check        fmt + vet + test, same as CI"
	@echo "make test         go test -race ./..."
	@echo "make build        build both binaries into ./bin"
	@echo "make docker-api   build the API image"

run:
	go run $(LDFLAGS) ./cmd/api

run-worker:
	go run $(LDFLAGS) ./cmd/worker

test:
	go test -race ./...

# Mirrors the CI 'check' job exactly, so a green local run means a green CI run.
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...

build:
	go build $(LDFLAGS) -o bin/api ./cmd/api
	go build $(LDFLAGS) -o bin/worker ./cmd/worker

docker-api:
	docker build -f deployments/Dockerfile --build-arg CMD=api --build-arg VERSION=$(VERSION) -t ayna-api .

docker-worker:
	docker build -f deployments/Dockerfile --build-arg CMD=worker --build-arg VERSION=$(VERSION) -t ayna-worker .

tidy:
	go mod tidy
