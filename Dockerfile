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
# **A Bitcoin Core release, and that is the load-bearing part.** Core has never
# merged BIP-110, so it follows the status-quo chain — which is the whole job of
# this node, since the user's own node is on the other side of the question.
#
# This was pinned to 28.0 for a while on the reasoning that a release predating
# the rules cannot enforce them. That was a proxy: what actually matters is
# whether the client carries an RDTS deployment, not when it was built. The proxy
# cost about eighty thousand blocks of script verification on every first sync,
# because assumevalid froze in August 2024 along with everything else. So the
# pin now tracks a current release and `make check` asserts the real property
# directly — see scripts/check-no-rdts.sh, which fails the build if the pinned
# client knows what RDTS is.
#
# Do not swap this for Bitcoin Knots. 29.3.knots20260508 and later refuse to
# start without `consensusrules=rdts`, so they enforce the new rules and would
# follow the very chain this node exists to see around. The one build that does
# not — 29.3.knots20260507 — is a terminal release with no security path, and
# its stricter relay policy (a sub-dust fee penalty, in particular) would
# deprioritise exactly the anchor and HTLC outputs a Lightning commitment
# carries, in the mempool this node is watched for.
#
# Verified against a checksum committed here rather than one fetched alongside
# the download: a checksum an attacker can replace with the file is not a check.
# Also built on the build machine: this stage downloads and unpacks a tarball
# chosen by TARGETARCH and never runs anything from it, so emulating it would buy
# nothing and cost minutes.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS bitcoin

ARG BITCOIN_VERSION=31.1
ARG BITCOIN_SHA256_X86_64=b80d9c3e04da78fb6f0569685673418cf686fadba9042d926d13fb87ff503f9e
ARG BITCOIN_SHA256_AARCH64=dcf1873f2208ba4f962f3398d47e154c39c0084be8f4553e05c940d0ace3d004
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

# jq and yq are for the StartOS 0.3.5.1 half: that version's settings screen
# writes YAML into the data volume, and its health checks are shell scripts the
# platform runs and reads JSON back from.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl xz-utils tor jq \
 && rm -rf /var/lib/apt/lists/*

ARG YQ_VERSION=4.44.3
ARG YQ_SHA256_X86_64=a2c097180dd884a8d50c956ee16a9cec070f30a7947cf4ebf87d5f36213e9ed7
ARG YQ_SHA256_AARCH64=0e7e1524f68d91b3ff9b089872d185940ab0fa020a5a9052046ef10547023156
ARG TARGETARCH
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) yqarch=amd64 ; yqsha="${YQ_SHA256_X86_64}"  ;; \
      arm64) yqarch=arm64 ; yqsha="${YQ_SHA256_AARCH64}" ;; \
      *) echo "no yq build for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 3 \
      "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_${yqarch}" \
      -o /usr/local/bin/yq; \
    echo "${yqsha}  /usr/local/bin/yq" | sha256sum -c -; \
    chmod +x /usr/local/bin/yq

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
# The Umbrel entrypoint, which translates that platform's environment — two
# different Bitcoin apps with two different variable prefixes — into the names
# the shared renderer expects.
COPY docker_entrypoint_umbrel.sh /usr/local/bin/docker_entrypoint_umbrel.sh
# The StartOS 0.3.5.1 entrypoint and its health checks. That version runs one
# image with no per-process daemons, so s6 supervises and the platform asks these
# scripts how things are going.
COPY docker_entrypoint_0351.sh /usr/local/bin/docker_entrypoint_0351.sh
COPY check-dashboard.sh /usr/local/bin/check-dashboard.sh
COPY check-chains.sh /usr/local/bin/check-chains.sh
RUN chmod +x /usr/local/bin/docker_entrypoint.sh \
             /usr/local/bin/docker_entrypoint_040.sh \
             /usr/local/bin/docker_entrypoint_umbrel.sh \
             /usr/local/bin/docker_entrypoint_0351.sh \
             /usr/local/bin/check-dashboard.sh \
             /usr/local/bin/check-chains.sh \
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
RUN mkdir -p /mnt/bitcoind /mnt/lnd \
 && touch /mnt/lnd/tls.cert /mnt/lnd/readonly.macaroon

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
