#!/usr/bin/env bash
#
# What somebody else gets when they clone this repository must build.
#
# **This exists because it did not.** A `.gitignore` rule of `coverage.*`, meant
# for Go's coverage output, is a glob over every directory — and it silently
# swallowed `internal/responder/tower/coverage.go`. The file was never committed,
# every local build worked because the file was sitting in the working tree, and
# a fresh clone did not compile. Nothing noticed for two milestones.
#
# The failure is invisible from inside a working copy, which is exactly why it
# needs a check rather than care. Two things are verified here, cheaply:
#
#   1. no source file is ignored, which is the direct form of that mistake;
#   2. the tracked tree actually compiles, which catches it however it happens.

set -euo pipefail

cd "$(dirname "$0")/.."

fail=0

# ── 1. No source file may be ignored ──────────────────────────────────────────
#
# Deliberately by extension rather than by name. The next instance of this will
# not be called coverage.go, and a list of known-bad filenames only ever catches
# the mistake already made.
ignored_source="$(git status --porcelain --ignored 2>/dev/null \
  | sed -n 's/^!! //p' \
  | grep -E '\.(go|ts|tsx|js|mjs|sh|sql|html|css|yml|yaml|toml)$' \
  | grep -v '^scripts/embassy\.js$' \
  || true)"

if [ -n "${ignored_source}" ]; then
  printf '\n  these source files are ignored and would be missing from a clone:\n\n' >&2
  printf '%s\n' "${ignored_source}" | sed 's/^/    /' >&2
  printf '\n  If one is genuinely generated, add it to the exemption in this script\n' >&2
  printf '  with a reason. Otherwise narrow the .gitignore rule that matches it —\n' >&2
  printf '  `git check-ignore -v <file>` names the offending line.\n\n' >&2
  fail=1
fi

# ── 2. The tracked tree must compile ──────────────────────────────────────────
#
# The check above catches the shape of the mistake; this catches the mistake
# whatever shape it takes — a file never added, a rule matching something
# unexpected, a path only present locally.
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

if ! git clone --quiet --no-local . "${tmp}/clone" 2>/dev/null; then
  printf '  could not clone the repository to check it\n' >&2
  exit 1
fi

if ! (cd "${tmp}/clone" && go build ./... 2>"${tmp}/err"); then
  printf '\n  a fresh clone of this repository does not build:\n\n' >&2
  sed 's/^/    /' "${tmp}/err" >&2
  printf '\n  Something in the working tree is not committed. This is what a\n' >&2
  printf '  contributor would see on their first attempt.\n\n' >&2
  fail=1
fi

[ "${fail}" -eq 0 ] || exit 1

printf '  a fresh clone builds\n'
