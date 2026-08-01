# Forktower.
#
# One image that can run two ways, because there are two deployments with
# different shapes and one runtime contract is easier to trust than two:
#
#   all-in-one   s6-overlay supervises forktowerd and a second Bitcoin node
#                inside this container. What StartOS needs — it runs exactly one
#                image — and what Umbrel gets for free.
#
#   external     forktowerd alone, with the second node as its own service. The
#                shape a self-hoster expects from docker-compose, and the one the
#                reference deployment uses.
#
# The entrypoint picks between them from FORKTOWER_SQ_MODE. Everything else about
# the image is the same either way, so what is tested in one is tested in both.

# ── The daemon ────────────────────────────────────────────────────────────────
#
# Built on the *build* machine's architecture and cross-compiled to the target,
# rather than run under emulation. Go cross-compiles natively and cgo is off
# here, so this costs nothing and avoids a great deal: the Go toolchain running
# under qemu-user deadlocks partway through linking — the build sits at 0.2% CPU
# with a defunct compiler behind it and never finishes, which reads as "slow"
# for the first hour.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

ARG TARGETARCH
WORKDIR /src

# Dependencies first, so a change to the code does not re-download the module
# cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# Static, and reproducible as far as the toolchain allows: no cgo (the SQLite
# driver is pure Go, so this costs nothing), trimmed paths, no build id. A user's
# only independent check on a published binary is rebuilding it and comparing.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/forktowerd ./cmd/forktowerd

# ── The second Bitcoin node ───────────────────────────────────────────────────
#
# A release that predates the fork's rules, so it follows the status-quo chain by
# construction rather than by configuration. Pinned, and verified against a
# checksum committed here rather than one fetched alongside the download: a
# checksum an attacker can replace with the file is not a check.
# Also built on the build machine: this stage downloads and unpacks a tarball
# chosen by TARGETARCH and never runs anything from it, so emulating it would buy
# nothing and cost minutes.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS bitcoin

ARG BITCOIN_VERSION=28.0
ARG BITCOIN_SHA256_X86_64=7fe294b02b25b51acb8e8e0a0eb5af6bbafa7cd0c5b0e5fcbb61263104a82fbc
ARG BITCOIN_SHA256_AARCH64=7fa582d99a25c354d23e371a5848bd9e6a79702870f9cbbf1292b86e647d0f4e
ARG TARGETARCH

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64  ; sha="${BITCOIN_SHA256_X86_64}"  ;; \
      arm64) arch=aarch64 ; sha="${BITCOIN_SHA256_AARCH64}" ;; \
      *) echo "no pinned Bitcoin Core build for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    tarball="bitcoin-${BITCOIN_VERSION}-${arch}-linux-gnu.tar.gz"; \
    curl -fsSL --retry 3 \
      "https://bitcoincore.org/bin/bitcoin-core-${BITCOIN_VERSION}/${tarball}" \
      -o /tmp/bitcoin.tar.gz; \
    echo "${sha}  /tmp/bitcoin.tar.gz" | sha256sum -c -; \
    mkdir -p /out; \
    tar -xzf /tmp/bitcoin.tar.gz -C /tmp; \
    cp "/tmp/bitcoin-${BITCOIN_VERSION}/bin/bitcoind" /out/; \
    cp "/tmp/bitcoin-${BITCOIN_VERSION}/bin/bitcoin-cli" /out/; \
    rm -rf /tmp/bitcoin.tar.gz "/tmp/bitcoin-${BITCOIN_VERSION}"

# ── The image ─────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

ARG S6_OVERLAY_VERSION=3.2.0.2
ARG TARGETARCH

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils tor \
 && rm -rf /var/lib/apt/lists/*

# s6-overlay supervises the co-resident processes in all-in-one mode. Verified
# against checksums committed here for the same reason as Bitcoin Core: a
# checksum fetched from beside the file is not a check on the file.
ARG S6_SHA256_NOARCH=6dbcde158a3e78b9bb141d7bcb5ccb421e563523babbe2c64470e76f4fd02dae
ARG S6_SHA256_X86_64=59289456ab1761e277bd456a95e737c06b03ede99158beb24f12b165a904f478
ARG S6_SHA256_AARCH64=8b22a2eaca4bf0b27a43d36e65c89d2701738f628d1abd0cea5569619f66f785
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) s6arch=x86_64  ; s6sha="${S6_SHA256_X86_64}"  ;; \
      arm64) s6arch=aarch64 ; s6sha="${S6_SHA256_AARCH64}" ;; \
      *) echo "no s6-overlay build for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}"; \
    curl -fsSL --retry 3 "${base}/s6-overlay-noarch.tar.xz" -o /tmp/s6-noarch.tar.xz; \
    curl -fsSL --retry 3 "${base}/s6-overlay-${s6arch}.tar.xz" -o /tmp/s6-arch.tar.xz; \
    echo "${S6_SHA256_NOARCH}  /tmp/s6-noarch.tar.xz" | sha256sum -c -; \
    echo "${s6sha}  /tmp/s6-arch.tar.xz" | sha256sum -c -; \
    tar -C / -Jxpf /tmp/s6-noarch.tar.xz; \
    tar -C / -Jxpf /tmp/s6-arch.tar.xz; \
    rm -f /tmp/s6-noarch.tar.xz /tmp/s6-arch.tar.xz

COPY --from=build   /out/forktowerd  /usr/local/bin/forktowerd
COPY --from=bitcoin /out/bitcoind    /usr/local/bin/bitcoind
COPY --from=bitcoin /out/bitcoin-cli /usr/local/bin/bitcoin-cli

COPY rootfs/ /
COPY deploy/compose/sq-anchors.txt /usr/share/forktower/sq-anchors.txt
COPY docker_entrypoint.sh /usr/local/bin/docker_entrypoint.sh
# The StartOS 0.4.x entrypoint, which renders the configuration and stops —
# there the package's own supervisor starts the processes, so s6 never runs.
COPY docker_entrypoint_040.sh /usr/local/bin/docker_entrypoint_040.sh
RUN chmod +x /usr/local/bin/docker_entrypoint.sh /usr/local/bin/docker_entrypoint_040.sh \
 && find /etc/s6-overlay -type f -name run -exec chmod +x {} + \
 && find /etc/s6-overlay/scripts -type f -exec chmod +x {} + 2>/dev/null || true

# The mount points the platform binds onto.
#
# **They have to exist in the image.** StartOS does not create a mount point: a
# bind mount onto a path that is not there fails with `mount exited with exit
# status: 32`, which names neither the path nor the reason.
#
# `/mnt/lnd` is used only by the short-lived container that copies the Lightning
# credentials out, never by the daemon — the LND volume also holds the wallet
# seed in plain text.
RUN mkdir -p /mnt/bitcoind /mnt/lnd

# Runs as a non-root user. Nothing here needs root, and a Bitcoin node that does
# is a Bitcoin node whose bugs are worth more to somebody.
RUN useradd --system --create-home --home-dir /home/forktower forktower \
 && mkdir -p /data /data/sq \
 && chown -R forktower:forktower /data /home/forktower

ENV FORKTOWER_SQ_MODE=all-in-one \
    FORKTOWER_DATA_DIR=/data \
    FORKTOWER_UI_LISTEN=0.0.0.0:8330 \
    S6_BEHAVIOUR_IF_STAGE2_FAILS=2 \
    S6_KEEP_ENV=1

# 8330 dashboard, 8433 the second node's peer port. Deliberately not the ports
# the user's own node uses: two Bitcoin nodes on one machine that both want 8333
# is a support question nobody should have to ask.
EXPOSE 8330 8433

VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/docker_entrypoint.sh"]
