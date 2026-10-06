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

# The floor the library must not fall through, over the packages a consumer
# imports. cmd/ and examples/ are main() wiring and runnable demonstrations;
# counting them measures how much demo code has tests, not how well the library
# is covered. Coverage is a floor here and not a goal -- see docs/TESTING.md for
# why "coverage went up" is explicitly not this project's standard.
COVER_MIN ?= 80.0

.PHONY: cover-check
cover-check: ## Gate: coverage over pkg/ and internal/ must be >= COVER_MIN
	@# -p 1: one package binary at a time. This target measures coverage; it is not
	@# a scheduling stress test, and running the whole suite in parallel WITH
	@# coverage instrumentation on a 4-vCPU runner starves the drills in
	@# examples/replication that hold real TCP listeners -- they pass alone and
	@# under -race at full parallelism, and failed only here. Serial is slower and
	@# says the same thing about coverage.
	@$(GO) test -p 1 -coverprofile=coverage.out -covermode=atomic $(PKGS)
	@head -1 coverage.out > coverage.lib.out
	@grep -E '/(pkg|internal)/' coverage.out >> coverage.lib.out
	@total=$$($(GO) tool cover -func=coverage.lib.out | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
		awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { \
			if (t+0 < m+0) { printf "FAIL: coverage %.1f%% over pkg/ + internal/ is below the %.1f%% floor\n", t, m; exit 1 } \
			printf "coverage %.1f%% over pkg/ + internal/ (floor %.1f%%)\n", t, m }'

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test -run '^$$' -bench=. -benchmem $(PKGS)

# The benchmark gate, run locally before a push (docs/BENCH-GATE.md §5.4). Changes
# land on main by direct push, so this is the only comparison that happens before
# one does. The judge is built from the BASE tree, as in CI, so the change under
# test cannot loosen the rules that judge it. BENCH_BASE picks the comparison point
# (default: the merge base with origin/main); ENFORCE=1 makes a regression fail;
# BENCH_ONLY=<regexp> narrows the run while iterating.
BENCH_BASE ?= origin/main

.PHONY: bench-check
bench-check: ## Gate: benchmarks of the working tree against the merge base (ENFORCE=1 to fail)
	@base=$$(git merge-base HEAD $(BENCH_BASE)) || exit 2; \
		tmp=$$(mktemp -d); \
		git worktree add --detach -q "$$tmp/base" "$$base" || exit 2; \
		if [ -d "$$tmp/base/cmd/benchgate" ]; then \
			(cd "$$tmp/base" && $(GO) build -o "$$tmp/benchgate" ./cmd/benchgate) && \
			"$$tmp/benchgate" compare -base-dir "$$tmp/base" -head-dir . $(if $(ENFORCE),-enforce) $(if $(BENCH_ONLY),-only '$(value BENCH_ONLY)') -out bench-result.json; \
			status=$$?; \
		else \
			echo "benchgate: not compared: no base gate at $$base"; status=0; \
		fi; \
		git worktree remove --force "$$tmp/base"; rm -rf "$$tmp"; exit $$status

.PHONY: demo
demo: ## Run the CLI demo (cmd/obdemo)
	$(GO) run ./cmd/obdemo

.PHONY: check
check: tidy vet test race cover-check ## Full local gate: tidy + vet + test + race + coverage floor

.PHONY: clean
clean: ## Remove build/coverage artifacts
	rm -rf $(BIN_DIR) coverage.out coverage.lib.out coverage.html *.prof bench-result.json
