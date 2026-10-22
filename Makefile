BINARY  := lcars-api
PKG     := github.com/asilluron/lcars-api
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/server.version=$(VERSION)
REDOCLY := redocly/cli:2.59.0
BUF     := bufbuild/buf:1.74.0
# Also used by the e2e tests, which read it from here.
RUST    := rust:1.99.0-slim
# A bare-metal target, to check the Rust crate builds without std.
NO_STD_TARGET := thumbv7em-none-eabihf

# Same Node as eufy/Dockerfile. @mega-yfue/eufy-sdk requires Node >= 24.5.
NODE    := node:24.22.1-slim

DOCKER_BUF = docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF)
# Cargo in Docker, with the toolchain components, crate downloads and build
# output cached in named volumes.
DOCKER_CARGO = docker run --rm -v "$(CURDIR)/rust:/src" -w /src \
	-v lcars-rustup-$(subst :,-,$(RUST)):/usr/local/rustup \
	-v lcars-cargo-registry:/usr/local/cargo/registry \
	-v lcars-cargo-target:/target -e CARGO_TARGET_DIR=/target \
	$(RUST)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

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
e2e: ## Run end-to-end tests: binary, Docker image, Rust client
	go test -tags e2e -count=1 -v ./e2e/...

# npm for the Eufy proxy. node_modules stays in a volume.
DOCKER_EUFY = docker run --rm -v "$(CURDIR)/eufy:/src/eufy" \
	-v lcars-eufy-node-modules:/src/eufy/node_modules -v lcars-npm-cache:/root/.npm \
	-w /src/eufy $(NODE)

.PHONY: eufy
eufy: ## Type-check and test the Eufy camera proxy (needs Docker)
	$(DOCKER_EUFY) sh -c 'npm ci --no-audit --no-fund && npm run typecheck && npm test'

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

.PHONY: proto
proto: ## Lint proto/ and regenerate the Go and Rust code from it (needs Docker)
	$(DOCKER_BUF) lint
	$(DOCKER_BUF) format --diff --exit-code
	$(DOCKER_BUF) generate

.PHONY: rust
rust: ## Check, lint and test the Rust crate, and build it for no_std (needs Docker)
	$(DOCKER_CARGO) sh -c 'set -e; \
		rustup component add rustfmt clippy; rustup target add $(NO_STD_TARGET); \
		cargo fmt --check; \
		cargo clippy --locked --all-targets -- -D warnings; \
		cargo test --locked; \
		cargo build --locked --no-default-features --target $(NO_STD_TARGET)'

.PHONY: rust-fmt
rust-fmt: ## Format the Rust crate (needs Docker)
	$(DOCKER_CARGO) sh -c 'rustup component add rustfmt && cargo fmt'

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
check: tidy fmt lint api-lint proto test rust eufy vuln e2e ## Run everything CI runs

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
