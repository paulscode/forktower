#!/bin/sh
#
# The Umbrel entrypoint.
#
# Thin, like the StartOS one: everything about rendering the configuration lives
# in docker_entrypoint.sh, which every deployment runs. What this adds is the
# translation from Umbrel's environment to the names that renderer expects.
#
# **Umbrel exports a different prefix per Bitcoin app.** The official app exports
# `APP_BITCOIN_*`; Bitcoin Knots exports `APP_BITCOIN_KNOTS_*` and mirrors every
# one of them back onto `APP_BITCOIN_*` at the end of its own exports. The app
# declares a dependency on `bitcoin`, which Knots satisfies by declaring
# `implements: [bitcoin]` — so the generic prefix is the one to trust, and the
# Knots-specific one is a fallback in case that mirroring ever stops.
#
# The Lightning node is genuinely optional: Forktower watches both chains with no
# Lightning node at all.

set -eu

log() { printf '%s\n' "$*" >&2; }

# ── The user's own Bitcoin node ───────────────────────────────────────────────
#
# The generic prefix first, because the declared dependency guarantees it and
# Knots mirrors its own values onto it. The Knots-specific prefix is only reached
# if that mirroring is ever dropped, and it exists so that would be a warning in
# the log rather than an app that cannot find a node.
if [ -n "${UMBREL_CORE_IP:-}" ]; then
  SF_IP="${UMBREL_CORE_IP}"
  SF_RPC_PORT="${UMBREL_CORE_RPC_PORT:-8332}"
  SF_RPC_USER="${UMBREL_CORE_RPC_USER:-}"
  SF_RPC_PASS="${UMBREL_CORE_RPC_PASS:-}"
  SF_ZMQ_RAWBLOCK_PORT="${UMBREL_CORE_ZMQ_RAWBLOCK_PORT:-}"
  SF_ZMQ_RAWTX_PORT="${UMBREL_CORE_ZMQ_RAWTX_PORT:-}"
  SF_WHICH="your Bitcoin node"
elif [ -n "${UMBREL_KNOTS_IP:-}" ]; then
  SF_IP="${UMBREL_KNOTS_IP}"
  SF_RPC_PORT="${UMBREL_KNOTS_RPC_PORT:-8332}"
  SF_RPC_USER="${UMBREL_KNOTS_RPC_USER:-}"
  SF_RPC_PASS="${UMBREL_KNOTS_RPC_PASS:-}"
  SF_ZMQ_RAWBLOCK_PORT="${UMBREL_KNOTS_ZMQ_RAWBLOCK_PORT:-}"
  SF_ZMQ_RAWTX_PORT="${UMBREL_KNOTS_ZMQ_RAWTX_PORT:-}"
  SF_WHICH="Bitcoin Knots"
else
  log "Forktower cannot start: no Bitcoin node was found on this Umbrel."
  log ""
  log "  Forktower compares the chain your node follows against the one it does"
  log "  not, so it needs your node to compare against. Install either the"
  log "  Bitcoin Node app or Bitcoin Knots from the app store, wait for it to"
  log "  finish syncing, then restart Forktower."
  exit 1
fi

export FORKTOWER_SF_RPC_URL="http://${SF_IP}:${SF_RPC_PORT}"
[ -n "${SF_RPC_USER}" ] && export FORKTOWER_SF_RPC_USER="${SF_RPC_USER}"
[ -n "${SF_RPC_PASS}" ] && export FORKTOWER_SF_RPC_PASSWORD="${SF_RPC_PASS}"

# ZMQ, when the Bitcoin app publishes it. Both Umbrel Bitcoin apps do, which
# means the fast path is available here rather than the five-second poll the
# daemon falls back to — a sighting in the memory pool is time the user gets.
[ -n "${SF_ZMQ_RAWBLOCK_PORT}" ] && \
  export FORKTOWER_SF_ZMQ_RAWBLOCK="tcp://${SF_IP}:${SF_ZMQ_RAWBLOCK_PORT}"
[ -n "${SF_ZMQ_RAWTX_PORT}" ] && \
  export FORKTOWER_SF_ZMQ_RAWTX="tcp://${SF_IP}:${SF_ZMQ_RAWTX_PORT}"

# ── The Lightning node, if there is one ───────────────────────────────────────
#
# The credential is the read-only macaroon LND wrote itself, bind-mounted as a
# single file along with the certificate. **Not the data directory**, which is
# what the neighbouring apps mount: Forktower has no business holding anything
# that can move money, and a mount is not something a careful macaroon policy can
# take back afterwards.
#
# The mounts default to /dev/null when no Lightning node is installed, so the
# compose file is valid either way and an empty file here means "no node".
LND_CERT=/mnt/lnd/tls.cert
LND_MACAROON=/mnt/lnd/readonly.macaroon
if [ -n "${UMBREL_LND_IP:-}" ] && [ -s "${LND_MACAROON}" ] && [ -s "${LND_CERT}" ]; then
  export FORKTOWER_LND_REST_URL="https://${UMBREL_LND_IP}:${UMBREL_LND_REST_PORT:-8080}"
  export FORKTOWER_LND_TLS_PATH="${LND_CERT}"
  export FORKTOWER_LND_MACAROON_PATH="${LND_MACAROON}"
  log "Forktower: reading channels from your Lightning node (read-only)."
else
  log "Forktower: no Lightning node connected — watching both chains only."
fi

log "Forktower: your node is ${SF_WHICH} at ${SF_IP}."

exec /usr/local/bin/docker_entrypoint.sh "$@"
