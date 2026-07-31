# The Eye of Satoshi, built from the copy vendored at third_party/rust-teos.
#
# This is the watchtower a Core Lightning node registers with. It has no
# equivalent: teos speaks the BOLT13 draft and LND's tower speaks its own
# protocol, and they do not interoperate — so a Core Lightning user has exactly
# one option, and it is a project with no active maintainer. That is why the
# source is vendored rather than fetched, and why the build below refuses to
# drift in any respect.
#
# Built with `make teos-image`.

ARG TEOS_RUST=1.81.0

FROM rust:${TEOS_RUST}-bookworm AS builder

# prost-build shells out to protoc, and its absence is a build failure a long
# way into the compile with a message that does not obviously name the cause.
#
# libprotobuf-dev as well as the compiler, and that is not belt-and-braces:
# `protobuf-compiler` alone gives you protoc without the well-known types, and
# the tower's service definition imports `google/protobuf/empty.proto`. The
# failure reads as a problem with teos's own .proto files rather than as a
# missing package.
RUN apt-get update \
 && apt-get install -y --no-install-recommends protobuf-compiler libprotobuf-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY third_party/rust-teos/ ./

# **--locked is the mechanical form of a decision, not a habit.** The committed
# Cargo.lock is the only reason a project with no active maintainer still
# builds: dependency resolution is frozen, so the bit-rot that normally kills an
# unmaintained Rust project has not reached it. Without this flag, a build on a
# machine where something wanted a newer dependency would quietly resolve
# differently and succeed — and the first anyone would know is when it stopped.
#
# Only teosd and its CLI. The watchtower-client plugin belongs on the user's own
# Core Lightning node, not in this image.
RUN cargo build --release --locked --bin teosd --bin teos-cli

FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --home-dir /data --shell /usr/sbin/nologin teos

COPY --from=builder /src/target/release/teosd /usr/local/bin/teosd
COPY --from=builder /src/target/release/teos-cli /usr/local/bin/teos-cli
# The licence travels with the binary, because the binary is somebody else's
# work under the terms in it.
COPY --from=builder /src/LICENSE /usr/share/licenses/rust-teos/LICENSE

USER teos
WORKDIR /data

# The tower's API. Where it *binds* is set at run time and is never a wildcard —
# a watchtower accepts a session from anybody who can reach it.
EXPOSE 9814

ENTRYPOINT ["/usr/local/bin/teosd"]
CMD ["--datadir", "/data"]
