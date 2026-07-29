# Forktower
#
# Two namespaces live here and must not collide:
#
#   the Go loop         build / test / lint / integration / run-dev / forkbench-*
#   the packaging loop  image / s9pk / release   (added with the packaging work,
#                                                 in s9pk.mk)
#
# Bare `make` builds the binaries. This is a code repository first and a
# packaging repository second.

BINARIES   := forktowerd forkbench
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR    := bin

# Reproducibility: no cgo (the SQLite driver is pure Go, so this is free),
# trimmed paths, and no build id. A user's only independent check on a published
# binary is rebuilding it and comparing, so this matters more than it looks.
GO_ENV     := CGO_ENABLED=0
GO_FLAGS   := -trimpath -mod=readonly
LDFLAGS    := -s -w -buildid= -X main.version=$(VERSION)

GOLANGCI   ?= golangci-lint

.DEFAULT_GOAL := build
.PHONY: build test lint fmt integration run-dev forkbench-up forkbench-down \
        check-boundary vuln tidy clean help

## build: compile all binaries into bin/
build:
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do \
	  echo "  build  $$b"; \
	  $(GO_ENV) go build $(GO_FLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$b ./cmd/$$b || exit 1; \
	done

## test: unit and component tests, with the race detector
test:
	go test -race -count=1 ./...

## lint: static analysis, formatting check, and the documentation boundary check
lint: check-boundary
	$(GOLANGCI) config verify
	@# `run` does not check formatters; `fmt --diff` is the gate that does, and
	@# it exits non-zero when a file would be reformatted.
	$(GOLANGCI) fmt --diff ./...
	$(GOLANGCI) run ./...

## fmt: apply formatting
fmt:
	$(GOLANGCI) fmt ./...

## integration: tests that need Docker (build-tagged, opt-in)
integration:
	go test -race -count=1 -tags integration ./...

## vuln: check dependencies against the Go vulnerability database
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## check-boundary: no shipped file may reference the private planning documents
check-boundary:
	@./scripts/check-boundary.sh

## run-dev: run the daemon against a local development configuration
run-dev: build
	$(BIN_DIR)/forktowerd --config deploy/compose/forktower.example.toml

## forkbench-up: bring up the local two-chain test world
forkbench-up: build
	$(BIN_DIR)/forkbench up

## forkbench-down: tear it down again
forkbench-down: build
	$(BIN_DIR)/forkbench down

## tidy: tidy and verify module requirements
tidy:
	go mod tidy
	go mod verify

## clean: remove build output
clean:
	rm -rf $(BIN_DIR)

## help: list targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
