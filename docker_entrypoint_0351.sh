#!/bin/sh
#
# The StartOS 0.3.5.1 entrypoint.
#
# Thin, like the others: everything about rendering the configuration lives in
# docker_entrypoint.sh, which every deployment runs. What this adds is reading
# the platform's own config file — on this version the settings screen writes
# YAML into the data volume, rather than the package's TypeScript being handed
# an object as it is on 0.4.x.
#
# **s6 supervises the processes here.** 0.3.5.1 runs exactly one image and has no
# equivalent of 0.4.x's per-process daemons, so the shared entrypoint's
# all-in-one mode is not a fallback on this version — it is the only shape
# available, and the reason the image carries s6 at all.

set -eu

# The platform mounts the data volume at /data and writes the settings screen's
# answers here. Overridable so this script can be run against a directory, which
# is what lets a test drive it: the version of this file that could not
# authenticate to the user's node was untestable outside a container, and that
# is most of why nobody noticed.
DATA_DIR="${FORKTOWER_DATA_DIR:-/data}"
CONFIG_YAML="${FORKTOWER_CONFIG_YAML:-${DATA_DIR}/start9/config.yaml}"

log() { printf '%s\n' "$*" >&2; }

# Read one value, falling back when the file or the key is absent — which is the
# normal state on the very first start, before the user has opened Settings.
cfg() {
  if [ -f "${CONFIG_YAML}" ]; then
    value="$(yq e "${1} // \"\"" "${CONFIG_YAML}" 2>/dev/null || printf '')"
    [ "${value}" = "null" ] && value=""
    if [ -n "${value}" ]; then
      printf '%s' "${value}"
      return
    fi
  fi
  printf '%s' "${2}"
}

FORKTOWER_PLATFORM=startos-0.3
FORKTOWER_DATA_DIR="${DATA_DIR}"
FORKTOWER_SQ_MODE=all-in-one
FORKTOWER_UI_LISTEN=0.0.0.0:8330
FORKTOWER_UI_AUTH=platform
export FORKTOWER_PLATFORM FORKTOWER_DATA_DIR FORKTOWER_SQ_MODE \
       FORKTOWER_UI_LISTEN FORKTOWER_UI_AUTH

# ── The user's own Bitcoin node ───────────────────────────────────────────────
#
# Reached at the address the platform gives a dependent, with the credentials
# from the Bitcoin package's own configuration. Cookie authentication is not
# available across packages on this version, so the RPC username and password
# are what there is.
SF_HOST="${BITCOIND_HOST:-bitcoind.embassy}"
export FORKTOWER_SF_RPC_URL="http://${SF_HOST}:8332"

# **Read from the node's published properties, because nothing hands them over.**
#
# This used to read BITCOIND_RPC_USER and BITCOIND_RPC_PASSWORD from the
# environment, on the belief that the platform set them for a declared
# dependency. It does not, and never did. There is no cookie file on this
# platform either, so the result was a configuration with an RPC address and no
# way to authenticate to it — the daemon refused to start, restarted, refused
# again, and the dashboard the user clicked "Launch UI" for was never served by
# anything.
#
# What does exist is `start9/stats.yaml` in the node's own volume, where that
# package publishes "RPC Username" and "RPC Password" for exactly this purpose.
# It is mounted read-only and scoped to that subdirectory; see manifest.yaml.
BITCOIND_STATS="${BITCOIND_STATS:-/mnt/bitcoind/stats.yaml}"
if [ -f "${BITCOIND_STATS}" ]; then
  # Bracket syntax, not `.data."RPC Username"`. The keys have spaces in them and
  # the dot-quote form this yq does not accept — it parses as far as the space
  # and reports a syntax error, which as a `2>/dev/null` command substitution is
  # indistinguishable from a node that published nothing.
  SF_RPC_USER="$(yq e '.data["RPC Username"].value // ""' "${BITCOIND_STATS}" 2>/dev/null || printf '')"
  SF_RPC_PASS="$(yq e '.data["RPC Password"].value // ""' "${BITCOIND_STATS}" 2>/dev/null || printf '')"
  if [ -n "${SF_RPC_USER}" ] && [ -n "${SF_RPC_PASS}" ]; then
    export FORKTOWER_SF_RPC_USER="${SF_RPC_USER}"
    export FORKTOWER_SF_RPC_PASS="${SF_RPC_PASS}"
  else
    echo "Forktower: your Bitcoin node's properties are readable but carry no RPC
  username and password. Forktower cannot read your node without them." >&2
  fi
else
  echo "Forktower: cannot read your Bitcoin node's published properties at
  ${BITCOIND_STATS}, so there are no credentials to connect with. If the Bitcoin
  service is still starting, this resolves itself once it has." >&2
fi

# Still honoured where somebody sets them deliberately — a hand-run container, or
# a node that is not the one this platform installed.
[ -n "${BITCOIND_RPC_USER:-}" ] && export FORKTOWER_SF_RPC_USER="${BITCOIND_RPC_USER}"
[ -n "${BITCOIND_RPC_PASSWORD:-}" ] && export FORKTOWER_SF_RPC_PASSWORD="${BITCOIND_RPC_PASSWORD}"
:
export FORKTOWER_SF_ZMQ_RAWBLOCK="tcp://${SF_HOST}:28332"
export FORKTOWER_SF_ZMQ_RAWTX="tcp://${SF_HOST}:28333"

# Tor is the platform's, and it is already there.
export FORKTOWER_TOR_PROXY="${TOR_PROXY:-embassy:9050}"

# ── The Lightning node, when the user has opted in ────────────────────────────
#
# The mount is that volume's `public` subdirectory, never its root: the root also
# holds the wallet's seed words and its password in plain text. What is here is
# the read-only macaroon LND wrote for itself, which answers everything Forktower
# asks and can do nothing else.
LND_CERT=/mnt/lnd/tls.cert
LND_MACAROON=/mnt/lnd/readonly.macaroon
if [ -s "${LND_MACAROON}" ] && [ -s "${LND_CERT}" ]; then
  export FORKTOWER_LND_REST_URL="https://${LND_HOST:-lnd.embassy}:8080"
  export FORKTOWER_LND_TLS_PATH="${LND_CERT}"
  export FORKTOWER_LND_MACAROON_PATH="${LND_MACAROON}"
  log "Forktower: reading channels from your Lightning node (read-only)."
else
  log "Forktower: no Lightning node connected — watching both chains only."
fi

# ── The settings screen ───────────────────────────────────────────────────────
SQ_MODE_CHOICE="$(cfg '.second-node.mode' 'pruned')"
PRUNE_MB="$(cfg '.second-node.prune-mb' '20000')"

# `prune=0` is Bitcoin Core for "keep everything", which is what Full means.
# Blocks-only is still pruned: skipping the memory pool is a separate saving from
# not keeping the whole chain, and somebody who picked the lightest option did
# not mean to ask for 600 GB.
if [ "${SQ_MODE_CHOICE}" = "full" ]; then
  export FORKTOWER_SQ_PRUNE_MB=0
else
  export FORKTOWER_SQ_PRUNE_MB="${PRUNE_MB}"
fi
if [ "${SQ_MODE_CHOICE}" = "blocksonly" ]; then
  export FORKTOWER_SQ_BLOCKSONLY=1
else
  export FORKTOWER_SQ_BLOCKSONLY=0
fi

export FORKTOWER_SQ_CLEARNET="$(cfg '.second-node.clearnet' 'false')"
export FORKTOWER_SQ_ONION_ONLY="$(cfg '.second-node.onion-only' 'false')"
export FORKTOWER_SQ_EXTRA_PEERS="$(cfg '.second-node.extra-peers' '')"
export FORKTOWER_SQ_P2P_PORT=8433

# ── The companion watchtower ─────────────────────────────────────────────────
#
# The address the user's Lightning node dials. Sibling services on this platform
# reach each other at their `.embassy` hostnames — the same convention this file
# already uses for bitcoind and lnd above — so the tower is reachable from the
# node without Tor and without being exposed beyond this machine.
#
# That last part matters more than the convenience: an LND watchtower accepts a
# session from anyone who can reach it and has no allowlist.
export FORKTOWER_TOWER_LND_ENABLED="$(cfg '.watchtower.enabled' 'true')"
export FORKTOWER_TOWER_LND_BIND="0.0.0.0:9911"
export FORKTOWER_TOWER_LND_EXTERNAL_ADDR="${FORKTOWER_HOST:-forktower.embassy}:9911"
export FORKTOWER_LOG_LEVEL="$(cfg '.advanced.log-level' 'info')"

NTFY_URL="$(cfg '.notifications.ntfy-url' '')"
if [ -n "${NTFY_URL}" ]; then
  export FORKTOWER_NTFY_URL="${NTFY_URL}"
  NTFY_TOKEN="$(cfg '.notifications.ntfy-token' '')"
  [ -n "${NTFY_TOKEN}" ] && export FORKTOWER_NTFY_TOKEN="${NTFY_TOKEN}"
fi
WEBHOOK_URL="$(cfg '.notifications.webhook-url' '')"
[ -n "${WEBHOOK_URL}" ] && export FORKTOWER_WEBHOOK_URL="${WEBHOOK_URL}"

mkdir -p "${DATA_DIR}/start9"

# The shared renderer. Its path is overridable for the same reason the data
# directory is: so this file can be exercised without an image around it.
exec "${FORKTOWER_ENTRYPOINT:-/usr/local/bin/docker_entrypoint.sh}" "$@"
