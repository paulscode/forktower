#!/usr/bin/env bash
#
# The second Bitcoin node must not know what RDTS is.
#
# Forktower's whole subject is the difference between two chains. The user's own
# node follows one of them; this node exists to follow the other. If the client
# we bundle ever gains BIP-110 support, that stops being true — and it stops
# being true *silently*, because a node enforcing the new rules looks perfectly
# healthy while agreeing with the node it was supposed to disagree with. The
# dashboard would report two chains in step and a user would conclude they were
# safe.
#
# This used to be guarded by pinning an old release: one that predates the rules
# cannot enforce them. That reasoning is sound and the guarantee is real, but it
# is a proxy — it tests *when* the client was built rather than *what it does* —
# and it cost roughly eighty thousand blocks of extra script verification on
# every first sync, because assumevalid ages along with everything else.
#
# So the property is now asserted directly. Ask the pinned binary what
# deployments it knows about, and fail if any of them is RDTS-shaped. A version
# bump that would quietly change which chain this node follows cannot get past
# this.
#
# Checks the *binary*, not the source or the version string: what matters is the
# thing that will actually run.

set -euo pipefail

cd "$(dirname "$0")/.."

# The pattern, in one place so the check and its self-test cannot drift.
#
# Deliberately broad. A future client might spell this differently, and a check
# that only catches the spelling we already know about is a check that passes on
# the day it matters.
RDTS_PATTERN='rdts|bip[ -]?110|consensusrules|reduced data'

# --self-test: prove the check would actually fire.
#
# A check that has only ever been seen to pass is not known to work. This feeds
# it the exact wording Bitcoin Knots 29.3 prints — read off a real 29.3 node —
# and fails if the pattern does not catch it.
if [ "${1:-}" = "--self-test" ]; then
  knots_help='  -consensusrules=<rules>
       Enforce the specified consensus rules (default: none). Must be rdts to
       use this software.'
  if printf '%s' "${knots_help}" | grep -qiE "${RDTS_PATTERN}"; then
    printf '  self-test: an RDTS-enforcing client would be caught\n'
    exit 0
  fi
  printf '  self-test FAILED: the pattern does not catch Bitcoin Knots 29.3,\n' >&2
  printf '  which means this check would pass a client that enforces the new\n' >&2
  printf '  rules — the exact thing it exists to prevent.\n' >&2
  exit 1
fi

version="$(grep -m1 '^ARG BITCOIN_VERSION=' Dockerfile | cut -d= -f2)"
if [ -z "${version}" ]; then
  printf '  could not read ARG BITCOIN_VERSION from Dockerfile\n' >&2
  exit 1
fi

# Built once and reused; this stage is cached after the first run.
image="forktower-rdts-check:${version}"
if ! docker build --target bitcoin -t "${image}" . >/dev/null 2>&1; then
  printf '  could not build the Bitcoin stage to check it\n' >&2
  exit 1
fi

# `-help-debug` lists every consensus rule the build knows how to enforce. On a
# client with BIP-110 this is where `rdts` appears — it is the value
# `-consensusrules` accepts, and on Knots 29.3+ the text says it is required.
help="$(docker run --rm --entrypoint /out/bitcoind "${image}" -help-debug 2>&1 || true)"

if [ -z "${help}" ]; then
  printf '  the pinned bitcoind produced no help output; cannot verify it\n' >&2
  exit 1
fi

if printf '%s' "${help}" | grep -qiE "${RDTS_PATTERN}"; then
  printf '\n' >&2
  printf '  the pinned Bitcoin client knows about RDTS / BIP-110.\n\n' >&2
  printf '  That client would follow the enforcing chain — the same one the\n' >&2
  printf '  user'"'"'s own node follows — and Forktower would report two chains in\n' >&2
  printf '  step while watching one of them twice. Pin a client that has not\n' >&2
  printf '  merged BIP-110 (any Bitcoin Core release, as of this writing).\n\n' >&2
  printf '  Matched:\n' >&2
  printf '%s' "${help}" | grep -iE "${RDTS_PATTERN}" | sed 's/^/    /' >&2
  exit 1
fi

printf '  second node: %s, with no RDTS support\n' "${version}"
