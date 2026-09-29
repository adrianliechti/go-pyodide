# CPython WASI release the embedded interpreter is built from.
PYTHON_VERSION := 3.14.7
WASI_SDK := 24

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: fetch-python
fetch-python: ## Download the CPython WASI release into internal/wasm and precompile the stdlib
	go run ./internal/build -version $(PYTHON_VERSION) -sdk $(WASI_SDK)
	@ls -la internal/wasm/python.wasm

.PHONY: fetch-tzdata
fetch-tzdata: ## Download and verify the pinned offline timezone database
	go run ./internal/build -tzdata-only

.PHONY: patch-python
patch-python: ## Apply and precompile patches without downloading the interpreter
	go run ./internal/build -patch-only

.PHONY: test
test: ## Run the Go tests
	CGO_ENABLED=0 go test ./...

.PHONY: bench
bench: ## Run the benchmarks
	CGO_ENABLED=0 go test -run '^$$' -bench . -benchmem ./

.PHONY: lint
lint: ## gofmt and go vet
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...

.PHONY: example
example: ## Run the example programs
	CGO_ENABLED=0 go run ./examples/basic
	CGO_ENABLED=0 go run ./examples/packages
