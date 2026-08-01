# Packaging.
#
# Kept apart from the Go loop in Makefile because they are different jobs with
# different tools, and mixing them is how a `make` in the wrong directory starts
# a fifteen-minute image build. This repository is code first and packaging
# second: bare `make` still builds the binaries.
#
# What is here: the container image, which every deployment runs, and the
# StartOS 0.4.x package built from it.

PKG_ID        := forktower
IMAGE_NAME    ?= paulscode/forktower
IMAGE_TAG     ?= $(VERSION)
IMAGE         := $(IMAGE_NAME):$(IMAGE_TAG)
PLATFORMS     ?= linux/amd64,linux/arm64

# Pinned, and overridable for a test build. A release that predates the fork's
# rules, so the second node follows the status-quo chain by construction.
BITCOIN_VERSION ?= 28.0

.PHONY: image image-push image-check 040 040-x86_64 040-aarch64 check-node

## image: build the container image for this machine's architecture
image:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BITCOIN_VERSION=$(BITCOIN_VERSION) \
	  -t $(IMAGE) \
	  -t $(IMAGE_NAME):latest \
	  .
	@printf '  built %s\n' "$(IMAGE)"

## image-push: build for both architectures and push
#
# Both, always. An arm64 user on a Raspberry Pi is the likeliest person to be
# running this, and an image that only works on the maintainer's laptop is an
# image that works for nobody.
image-push:
	docker buildx build \
	  --platform $(PLATFORMS) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BITCOIN_VERSION=$(BITCOIN_VERSION) \
	  -t $(IMAGE) \
	  -t $(IMAGE_NAME):latest \
	  --push \
	  .

## image-check: prove the built image runs and is the version it claims
image-check:
	@docker run --rm $(IMAGE) --version
	@docker run --rm --entrypoint /usr/local/bin/bitcoind $(IMAGE) -version | head -1


# ── StartOS 0.4.x ─────────────────────────────────────────────────────────────
#
# The package's TypeScript is compiled to a single file by ncc and packed by
# start-cli, which builds the image itself from the Dockerfile at the repo root.
# That root-level layout is what the tooling assumes, and is why the packaging
# files sit at the repository root rather than under deploy/.
#
# The device and the build host run different start-cli versions — the 0.4.0.1
# test box reports 1.1.0 against this machine's 0.4.0-beta.9 — and the s9pk
# format is compatible across that gap. The version that governs the package is
# the npm @start9labs/start-sdk pinned in package.json.

PKG_SOURCES := $(shell find startos -type f -name '*.ts' 2>/dev/null) \
               package.json tsconfig.json

check-node:
	@command -v npm >/dev/null || { \
	  printf '  npm is needed to build the StartOS package\n' >&2; exit 1; }
	@command -v start-cli >/dev/null || { \
	  printf '  start-cli is needed to pack an s9pk\n' >&2; exit 1; }

node_modules: package.json
	npm ci --silent || npm install --silent
	@touch node_modules

javascript/index.js: node_modules $(PKG_SOURCES) | check-node
	npx tsc --noEmit
	npm run --silent build

## 040: the StartOS 0.4.x package, both architectures
040:         $(PKG_ID)-040.s9pk
040-x86_64:  $(PKG_ID)-040-x86_64.s9pk
040-aarch64: $(PKG_ID)-040-aarch64.s9pk

$(PKG_ID)-040.s9pk: javascript/index.js Dockerfile
	start-cli s9pk pack --icon icon.svg -o $@
	@printf '  packed %s\n' "$@"

$(PKG_ID)-040-x86_64.s9pk: javascript/index.js Dockerfile
	start-cli s9pk pack --icon icon.svg --arch=x86_64 -o $@
	@printf '  packed %s\n' "$@"

$(PKG_ID)-040-aarch64.s9pk: javascript/index.js Dockerfile
	start-cli s9pk pack --icon icon.svg --arch=aarch64 -o $@
	@printf '  packed %s\n' "$@"
