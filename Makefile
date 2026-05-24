BIN     := tmux-mcp
PKG     := ./cmd/tmux-mcp
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath -ldflags='$(LDFLAGS)'

.PHONY: help build test test-fast test-race cover lint fmt vet tidy clean version release-snapshot

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the binary into ./$(BIN)
	go build $(GOFLAGS) -o $(BIN) $(PKG)

test: ## Run all tests with the race detector
	go test ./... -count=1 -race

test-fast: ## Quick test loop — no race detector, no coverage
	go test ./... -count=1

test-race: ## Run tests under the race detector
	go test ./... -count=1 -race

cover: ## Run tests with coverage and print summary
	go test ./... -count=1 -covermode=atomic -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -n 20

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Reformat code with gofmt -s
	gofmt -s -w .

vet: ## Run go vet
	go vet ./...

tidy: ## Reconcile go.mod / go.sum
	go mod tidy

version: ## Print the version that would be embedded in the binary
	@echo $(VERSION)

clean: ## Remove build artifacts
	rm -f $(BIN) coverage.out
	rm -rf dist

release-snapshot: ## Build a local goreleaser snapshot (reproducible)
	goreleaser release --snapshot --clean
