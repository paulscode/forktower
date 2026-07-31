#!/usr/bin/env bash
#
# Keep a copy of rust-teos that this project controls.
#
# **Why a copy at all.** rust-teos is the only watchtower a Core Lightning node
# can register with — the two watchtower protocols do not interoperate — and it
# is in care-and-maintenance with no active owner: 235 commits in 2022, ten in
# 2025, and the most recent release is from February 2023. Depending on a
# repository that might quietly disappear, for the one piece of software our
# Core Lightning users have no alternative to, is not a risk worth carrying when
# the whole source is a megabyte.
#
# The tree is committed under third_party/, `.git` and all its history stripped.
# That is the point: a mirror that still needs the network is not a mirror.
#
# Not `vendor/`: that name is reserved by the Go toolchain, and a directory
# there switches the whole build into vendoring mode and then fails for want of
# a modules.txt this project does not have.
#
# **Why a commit and not a tag.** The last release predates three years of
# fixes, including the HTTPS client support we want. There will not be another.
#
# Run by `make vendor-teos`. Idempotent: a copy already at the pinned commit is
# left alone, so this is safe to run from a build.

set -euo pipefail

cd "$(dirname "$0")/.."

if [ -t 1 ]; then R=$'\033[1;31m'; G=$'\033[1;32m'; N=$'\033[0m'
else R=""; G=""; N=""; fi

# shellcheck source=../deploy/teos/pinned.env
. deploy/teos/pinned.env

DEST="third_party/rust-teos"
STAMP="$DEST/VENDORED"

if [ -f "$STAMP" ] && grep -qx "commit=$TEOS_COMMIT" "$STAMP"; then
  printf '%s  teos: already vendored at %s%s\n' "$G" "${TEOS_COMMIT:0:12}" "$N"
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf 'teos: fetching %s at %s\n' "$TEOS_REPO" "${TEOS_COMMIT:0:12}"

# Fetch exactly the one commit rather than cloning a history nothing here needs.
# A shallow fetch of a named object cannot be redirected by a branch moving.
git init --quiet "$work/src"
git -C "$work/src" remote add origin "$TEOS_REPO"
git -C "$work/src" fetch --quiet --depth 1 origin "$TEOS_COMMIT"
git -C "$work/src" checkout --quiet FETCH_HEAD

got="$(git -C "$work/src" rev-parse HEAD)"
if [ "$got" != "$TEOS_COMMIT" ]; then
  printf '%steos: fetched %s, expected %s%s\n' "$R" "$got" "$TEOS_COMMIT" "$N" >&2
  exit 1
fi

# The lockfile is the only reason an unmaintained Rust project still builds:
# dependency resolution is frozen, so the bit-rot that normally kills one has
# not reached it. Nothing may run `cargo update` against this tree — the
# Dockerfile builds with --locked so an attempt fails rather than quietly
# succeeding against different dependencies.
if [ ! -f "$work/src/Cargo.lock" ]; then
  printf '%steos: no Cargo.lock in the fetched tree — refusing to vendor it%s\n' "$R" "$N" >&2
  exit 1
fi
if [ ! -f "$work/src/LICENSE" ]; then
  printf '%steos: no LICENSE in the fetched tree — refusing to vendor it%s\n' "$R" "$N" >&2
  exit 1
fi

# Drop the git metadata. A nested repository inside ours would be recorded as a
# bare pointer rather than as files, which is the opposite of a mirror.
rm -rf "$work/src/.git"

rm -rf "$DEST"
mkdir -p "$(dirname "$DEST")"
mv "$work/src" "$DEST"

{
  printf '# Written by scripts/vendor-teos.sh. Do not edit.\n'
  printf '#\n'
  printf '# Anything under this directory is somebody else'"'"'s work, vendored\n'
  printf '# unmodified. See deploy/teos/README.md.\n'
  printf 'repo=%s\n' "$TEOS_REPO"
  printf 'commit=%s\n' "$TEOS_COMMIT"
} > "$STAMP"

printf '%s  teos: vendored at %s (%s files)%s\n' \
  "$G" "${TEOS_COMMIT:0:12}" "$(find "$DEST" -type f | wc -l | tr -d ' ')" "$N"
