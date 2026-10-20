BINARY  := lcars-api
PKG     := github.com/asilluron/lcars-api
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/server.version=$(VERSION)
REDOCLY := redocly/cli:2.59.0

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

.PHONY: run
run: ## Run the server locally with human-readable logs
	LCARS_LOG_FORMAT=text go run ./cmd/$(BINARY)

.PHONY: build
build: ## Build a static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: test
test: ## Run tests with the race detector
	go test -race ./...

.PHONY: e2e
e2e: ## Run end-to-end tests against the built binary and Docker image
	go test -tags e2e -count=1 -v ./e2e/...

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: api-lint
api-lint: ## Validate the OpenAPI document with Redocly CLI (needs Docker)
	docker run --rm -v "$(CURDIR):/spec:ro" $(REDOCLY) lint

.PHONY: fmt
fmt: ## Format code (gofumpt + goimports)
	golangci-lint fmt

.PHONY: vuln
vuln: ## Scan for known vulnerabilities
	go tool govulncheck ./...

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: check
check: tidy fmt lint api-lint test vuln e2e ## Run everything CI runs

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
