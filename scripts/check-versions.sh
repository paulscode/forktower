#!/usr/bin/env bash
#
# The version has to be the same number everywhere it is written down.
#
# Forktower writes it in three places, because three different tools need it and
# none of them can read the others: `manifest.yaml` for the 0.3.5.1 package,
# `startos/version.ts` for the 0.4.x one, and a git tag for everything else.
#
# **A release where they disagree is worse than one that fails to build.** The
# package would install and report one version while the daemon inside it
# reported another, and the number a user quotes in a bug report would be the
# wrong one — which costs somebody an afternoon at exactly the moment they are
# already having a bad day.
#
# manifest.yaml is the source of truth. The others are checked against it.

set -euo pipefail

cd "$(dirname "$0")/.."

fail() { printf '  %s\n' "$*" >&2; exit 1; }

manifest="$(yq e '.version' manifest.yaml 2>/dev/null || true)"
[ -n "${manifest}" ] && [ "${manifest}" != "null" ] \
  || fail "could not read .version from manifest.yaml"

# The 0.4.x package builds its version from these two, as `<app>:<revision>`.
app="$(grep -oP "appVersion = '\K[^']+" startos/version.ts 2>/dev/null || true)"
[ -n "${app}" ] || fail "could not read appVersion from startos/version.ts"

if [ "${app}" != "${manifest}" ]; then
  fail "manifest.yaml says ${manifest} and startos/version.ts says ${app}"
fi

# A tag is optional — most builds are not releases — but when the checkout is
# exactly on one, it has to agree too. `v` prefix tolerated because that is how
# tags are usually written.
tag="$(git describe --exact-match --tags 2>/dev/null || true)"
if [ -n "${tag}" ]; then
  if [ "${tag#v}" != "${manifest}" ]; then
    fail "the checkout is tagged ${tag} but manifest.yaml says ${manifest}"
  fi
  printf '  version: %s (tagged)\n' "${manifest}"
else
  printf '  version: %s (untagged working tree)\n' "${manifest}"
fi
