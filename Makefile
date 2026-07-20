# orderbook — developer tasks
# Run `make help` for the list.

GO      ?= go
PKGS    := ./...
BIN_DIR := bin

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Sync go.mod / go.sum
	$(GO) mod tidy

.PHONY: build
build: ## Build all packages
	$(GO) build $(PKGS)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: test
test: ## Run unit tests
	$(GO) test $(PKGS)

.PHONY: race
race: ## Run tests with the race detector
	$(GO) test -race $(PKGS)

.PHONY: cover
cover: ## Run tests with coverage and write coverage.out/html
	$(GO) test -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "coverage.html written"

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -run '^$$' -bench=. -benchmem $(PKGS)

.PHONY: demo
demo: ## Run the CLI demo (cmd/obdemo)
	$(GO) run ./cmd/obdemo

.PHONY: check
check: tidy vet test race ## Full local gate: tidy + vet + test + race

.PHONY: clean
clean: ## Remove build/coverage artifacts
	rm -rf $(BIN_DIR) coverage.out coverage.html *.prof
