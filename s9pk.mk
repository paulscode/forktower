# Packaging.
#
# Kept apart from the Go loop in Makefile because they are different jobs with
# different tools, and mixing them is how a `make` in the wrong directory starts
# a fifteen-minute image build. This repository is code first and packaging
# second: bare `make` still builds the binaries.
#
# StartOS s9pk targets are not here yet — they arrive with the platform
# packaging work. What is here is the container image, which is what the compose
# and Umbrel deployments run.

IMAGE_NAME    ?= paulscode/forktower
IMAGE_TAG     ?= $(VERSION)
IMAGE         := $(IMAGE_NAME):$(IMAGE_TAG)
PLATFORMS     ?= linux/amd64,linux/arm64

# Pinned, and overridable for a test build. A release that predates the fork's
# rules, so the second node follows the status-quo chain by construction.
BITCOIN_VERSION ?= 28.0

.PHONY: image image-push image-check

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
