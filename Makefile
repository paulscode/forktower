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
.PHONY: build test lint fmt integration run-dev cover-html icons icons-check \
        forkbench-up forkbench-split forkbench-status forkbench-down \
        forkbench-ln-up forkbench-ln-status forkbench-fixtures demo-s1-detect \
        check check-boundary check-artifacts check-no-rdts check-versions \
        cover-check cover tidy-check vuln tidy clean help \
        vendor-teos teos-image

## check: everything that must pass before a commit — the only gate there is
#
# There is no continuous integration yet: the project is not published, so the
# workflow in .github/ has never run. Until it does, this target is the whole
# safety net, which is why it includes the two checks that would otherwise only
# happen on a build server — module tidiness and the vulnerability database.
check: build lint test cover-check tidy-check vuln check-no-rdts
	@printf '\n  all checks passed\n'

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
lint: check-boundary check-artifacts
	$(GOLANGCI) config verify
	@# `run` does not check formatters; `fmt --diff` is the gate that does, and
	@# it exits non-zero when a file would be reformatted.
	$(GOLANGCI) fmt --diff ./...
	$(GOLANGCI) run ./...
	@# Again with the integration tag. Those files are excluded from the build
	@# without it, so they were never linted — and they are the least-run code in
	@# the repository, which makes them the code that benefits from it most.
	$(GOLANGCI) run --build-tags integration ./...

## fmt: apply formatting
fmt:
	$(GOLANGCI) fmt ./...

## integration: tests that need Docker (build-tagged, opt-in)
#
# -p 1 runs one package at a time. Several of these drive the same containers, so
# the default parallelism would have two suites fighting over one world and
# failing in ways that look like flakiness rather than the collision they are.
integration:
	go test -race -count=1 -p 1 -timeout 20m -tags integration ./...

## vuln: check dependencies against the Go vulnerability database (needs network)
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## tidy-check: fail if go.mod or go.sum would change — catches an uncommitted
## dependency, which on a clean checkout becomes a build that does not work
tidy-check:
	@cp go.mod go.mod.checkbak
	@[ -f go.sum ] && cp go.sum go.sum.checkbak || true
	@go mod tidy
	@ok=1; \
	 cmp -s go.mod go.mod.checkbak || ok=0; \
	 if [ -f go.sum ] || [ -f go.sum.checkbak ]; then cmp -s go.sum go.sum.checkbak || ok=0; fi; \
	 mv go.mod.checkbak go.mod; \
	 [ -f go.sum.checkbak ] && mv go.sum.checkbak go.sum || true; \
	 if [ "$$ok" = "0" ]; then \
	   echo "  go.mod/go.sum are not tidy — run: make tidy"; exit 1; \
	 fi
	@echo "  modules tidy"

## check-no-rdts: the bundled second node must not enforce the new rules
#
# Not in `lint`, because it needs Docker and builds an image — but run it before
# changing the pinned Bitcoin version, and it runs in `check`. A client that
# enforces BIP-110 would follow the same chain as the user's own node, and
# Forktower would report two chains in step while watching one of them twice.
check-no-rdts:
	@./scripts/check-no-rdts.sh --self-test
	@./scripts/check-no-rdts.sh

## check-boundary: no shipped file may reference the private planning documents
check-boundary:
	@./scripts/check-boundary.sh

## check-artifacts: no compiled binary may be committed
check-artifacts:
	@./scripts/check-artifacts.sh

## cover-check: enforce the per-package coverage floors
cover-check:
	@./scripts/check-coverage.sh

## cover: per-package coverage, including packages that meet their floor
cover:
	@./scripts/check-coverage.sh -v

## cover-html: open a line-by-line coverage report
cover-html:
	go test -coverprofile=coverage.out ./... >/dev/null
	go tool cover -html=coverage.out

## run-dev: run the daemon against the forkbench world
#
# Uses its own configuration and its own database under .dev/, so a development
# run never touches whatever is in a real deployment's data directory.
run-dev: build
	@mkdir -p .dev
	@# Git records only the executable bit, so a checked-out config arrives
	@# world-readable and the daemon rightly complains that a file holding
	@# credentials is readable by others. Tightening it here keeps that warning
	@# meaningful: one that fires every single time is one people stop reading.
	@chmod 600 deploy/forkbench/forktower.dev.toml
	$(BIN_DIR)/forktowerd --config deploy/forkbench/forktower.dev.toml

## forkbench-up: bring up the local two-chain test world
forkbench-up: build
	$(BIN_DIR)/forkbench up

## forkbench-split: make the two chains disagree, permanently
forkbench-split: build
	$(BIN_DIR)/forkbench split

## forkbench-status: show both chains, and what Forktower makes of them
forkbench-status: build
	$(BIN_DIR)/forkbench status

## forkbench-down: tear it down again, including its state
forkbench-down: build
	$(BIN_DIR)/forkbench down

## forkbench-ln-up: add two Lightning nodes with a channel between them
forkbench-ln-up: build
	$(BIN_DIR)/forkbench ln-up

## forkbench-ln-status: show the Lightning nodes and their channels
forkbench-ln-status: build
	$(BIN_DIR)/forkbench ln-status

## demo-s1-detect: stage the attack this whole project is about, end to end
#
# A channel is opened and paid through, the counterparty is rolled back to an
# older state, the chains are split, and the counterparty publishes the
# commitment it had already promised not to — on the other chain only. On the
# chain the user's own node follows, nothing happened.
#
# Start Forktower first (`make run-dev` in another terminal) and watch it notice.
demo-s1-detect: build
	$(BIN_DIR)/forkbench up
	$(BIN_DIR)/forkbench ln-up
	$(BIN_DIR)/forkbench pay -times 3
	$(BIN_DIR)/forkbench snapshot-mallory
	$(BIN_DIR)/forkbench pay -times 3
	$(BIN_DIR)/forkbench split
	$(BIN_DIR)/forkbench breach -branch sq

## forkbench-fixtures: rebuild the real closing transactions the classifier is tested against
#
# Three worlds, because each scenario needs a channel of its own and a channel
# can only be closed once. Slow, and rarely needed: run it when the Lightning
# implementation is upgraded, or when the classifier is changed and the crafted
# transactions alone are not enough to trust it.
FIXTURES := internal/watcher/testdata
forkbench-fixtures: build
	$(BIN_DIR)/forkbench down
	$(BIN_DIR)/forkbench up
	$(BIN_DIR)/forkbench ln-up
	$(BIN_DIR)/forkbench pay -times 3
	$(BIN_DIR)/forkbench snapshot-mallory
	$(BIN_DIR)/forkbench pay -times 3
	$(BIN_DIR)/forkbench split
	$(BIN_DIR)/forkbench breach -branch sq -fixtures $(FIXTURES)
	$(BIN_DIR)/forkbench down
	$(BIN_DIR)/forkbench up
	$(BIN_DIR)/forkbench ln-up
	$(BIN_DIR)/forkbench pay -times 2
	$(BIN_DIR)/forkbench force-close -ln-node user -fixtures $(FIXTURES)
	$(BIN_DIR)/forkbench down
	$(BIN_DIR)/forkbench up
	$(BIN_DIR)/forkbench ln-up
	$(BIN_DIR)/forkbench pay -times 2
	$(BIN_DIR)/forkbench coop-close -fixtures $(FIXTURES)
	@printf '\n  fixtures rebuilt in %s\n' "$(FIXTURES)"

## vendor-teos: fetch the pinned rust-teos into vendor/, if it is not already there
vendor-teos:
	@./scripts/vendor-teos.sh

## teos-image: build the Core Lightning watchtower image from the vendored source
teos-image: vendor-teos
	docker build -f deploy/teos/Dockerfile -t forktower/teos:local .

## icons: regenerate the shipped icons from the source art
#
# Needs ImageMagick and references/icon.png, which is not published — so this is
# not part of `make check`. The outputs are committed precisely so that a clean
# checkout never needs either.
icons:
	@scripts/make-icons.sh

## icons-check: fail if the committed icons differ from what the art produces
icons-check:
	@scripts/make-icons.sh --check

# Packaging lives in its own file, and its targets are namespaced so that
# `make` on its own still means "build the binaries".
include s9pk.mk

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
