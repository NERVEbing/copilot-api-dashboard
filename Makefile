.PHONY: check fmt fmt-check lint test vet build run

GO_CMD ?= go
LINT_CMD ?= golangci-lint

check: fmt-check lint test build

fmt:
	$(LINT_CMD) fmt --config .golangci.yml

fmt-check:
	$(LINT_CMD) fmt --diff --config .golangci.yml

lint:
	$(LINT_CMD) run --config .golangci.yml ./...

test:
	$(GO_CMD) test ./...

vet:
	$(GO_CMD) vet ./...

build:
	$(GO_CMD) build ./...

run:
	$(GO_CMD) run ./cmd/dashboard
