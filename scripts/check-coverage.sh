#!/usr/bin/env bash
#
# Enforces a minimum test coverage per package.
#
# Per package, not a single project-wide average: an average lets a
# well-tested package hide an untested one, and the two halves of this codebase
# have genuinely different expectations. Decision logic is deliberately written
# free of I/O so that it can be tested exhaustively, and it is held to a high
# bar. Code whose whole job is to talk to a database, a node, or a socket is
# covered by integration tests that do not run here, and is held to a lower one.
#
# A package with no floor listed gets DEFAULT_MIN. Raising a floor after
# improving a package is encouraged; lowering one needs a reason in the table.
#
# Usage: check-coverage.sh [-v]

set -euo pipefail

cd "$(dirname "$0")/.."

DEFAULT_MIN=70

# package suffix (after the module path)  ->  minimum percent
#
# Anything listed below DEFAULT_MIN must say why. These are floors, not targets.
declare -A MIN=(
  # Pure decision logic: no I/O, so there is no excuse for a gap.
  [internal/sentinel]=90  # decision logic: no I/O, so no excuse for a gap
  [internal/deadline]=90
  [internal/watcher]=85
  [internal/config]=75
  # No I/O at all: a channel fan-out with a deliberate drop policy. Held high
  # because every branch is reachable from a test.
  [internal/bus]=95
  # Types, an interface, and small predicates over them. No I/O, nothing to
  # excuse a gap.
  [internal/chainview]=95
  # Half decision (which alert, what tier, what may be said to whom) and half
  # delivery. The deciding half is pure and near-total; the floor sits where the
  # whole package is, because the delivery half is exercised against an httptest
  # server rather than needing a container.
  [internal/alert]=90
  # Mostly decision: which of five states the user is shown, which words they
  # read, and which requests are refused. The handlers are exercised end to end
  # over httptest, so there is no container excuse for a gap here either.
  [internal/api]=90

  # I/O shells. The real exercise is in the integration suite, which needs
  # containers and does not run in this gate.
  #
  # store started at a floor of 60 on that reasoning and reached 79.9% with an
  # in-process SQLite database, which needs no container — so the floor moved up
  # to keep the achievement. Floors ratchet.
  [internal/store]=75
  # Lowered from 85 to 72 when the notification subsystem landed. The
  # request/response half reaches ~90% against a fake node over httptest, but the
  # notification path needs a peer that actually speaks the publish/subscribe wire
  # protocol, which cannot honestly be faked in a unit test — it is covered by the
  # integration suite instead, which does use a real node. This is a floor moving
  # *down* with a reason, which the policy allows and which is worth noticing.
  [internal/chainview/bitcoindview]=72
  [internal/registry/lnd]=40
  [internal/registry/cln]=40
  [internal/companion]=40

  # A fake used by other packages' tests. It has no tests of its own, and
  # measuring it here would report 0% no matter how heavily it is exercised,
  # because `go test -cover` credits a statement only to the package whose tests
  # ran it. The risk this floor cannot address — a fake that answers differently
  # from a real node — is not one tests of the fake would catch either; it is held
  # off by the compile-time assertions that it satisfies the same interfaces, and
  # by the integration suite running the same scenarios against a real node.
  [internal/chainview/chainviewtest]=0

  # Composition roots: wiring, almost no logic of their own.
  [internal/app]=0
  [cmd/forktowerd]=0
  [cmd/forkbench]=0
)

if [ -t 1 ]; then R=$'\033[1;31m'; G=$'\033[1;32m'; Y=$'\033[1;33m'; N=$'\033[0m'
else R=""; G=""; Y=""; N=""; fi

MODULE="$(go list -m)"

failed=0
checked=0

# `go test -cover` reports the statement-weighted figure per package, which is
# the number to hold a floor against. An earlier version of this script averaged
# the per-function percentages from `go tool cover -func`, which overstates:
# it weights a one-line helper the same as a long uncovered function, and read
# 84.5% for a package `go test` measures at 75.0%.
# A package that declares no code cannot be under-covered. Placeholder packages
# hold a doc comment and nothing else; exempting them keeps the floors meaningful
# for the packages that do have logic, and the exemption lapses by itself the
# moment real code arrives.
is_placeholder() {
  local files
  files="$(go list -f '{{range .GoFiles}}{{.}} {{end}}' "$1" 2>/dev/null)"
  [ "$(tr -s ' ' '\n' <<<"$files" | grep -c '\.go$')" -eq 1 ] &&
    [ "$(tr -s ' ' '\n' <<<"$files" | grep -c '^doc\.go$')" -eq 1 ]
}

# `go test -cover` emits three line shapes, and an earlier version of this script
# knew only two. A package with tests: "ok <pkg> 0.0s coverage: N% of statements".
# A package with no test files at all: "? <pkg> [no test files]". And — the one
# that was missed, which silently skipped exactly the case this gate exists for —
# a package with code but no tests, which begins with a tab and no verdict word:
# "\t<pkg>\t\tcoverage: 0.0% of statements". So find the package by looking for
# the module path anywhere on the line rather than by field position.
while read -r line; do
  case "$line" in
    *"$MODULE"*) ;;
    *) continue ;;
  esac

  pkg="$(tr -s ' \t' '\n\n' <<<"$line" | grep -m1 -F "$MODULE" || true)"
  [ -n "$pkg" ] || continue

  case "$line" in
    *"[no statements]"*)
      # Nothing to cover.
      continue
      ;;
    *"[no test files]"*)
      if is_placeholder "$pkg"; then continue; fi
      pct="0.0"
      ;;
    *"coverage:"*)
      pct="$(sed -E 's/.*coverage: ([0-9.]+)% of statements.*/\1/' <<<"$line")"
      ;;
    *)
      continue
      ;;
  esac

  suffix="${pkg#"$MODULE"/}"
  [ "$suffix" = "$pkg" ] && suffix="."

  min="${MIN[$suffix]-$DEFAULT_MIN}"
  checked=$((checked + 1))

  # Integer comparison; sub-percent granularity is noise for a floor.
  whole="${pct%.*}"
  if [ "$whole" -lt "$min" ]; then
    printf '%s  %-46s %5s%%  (floor %s%%)%s\n' "$R" "$suffix" "$pct" "$min" "$N"
    failed=$((failed + 1))
  elif [ "${1:-}" = "-v" ]; then
    printf '  %-46s %5s%%  (floor %s%%)\n' "$suffix" "$pct" "$min"
  fi
done < <(go test -cover ./... 2>&1)

if [ "$checked" -eq 0 ]; then
  printf '%s  coverage: no packages measured — is the profile empty?%s\n' "$Y" "$N"
  exit 1
fi

if [ "$failed" -gt 0 ]; then
  printf '\n%d package(s) below their coverage floor.\n' "$failed"
  printf 'Add tests, or change the floor in %s with a reason.\n' "scripts/check-coverage.sh"
  exit 1
fi

printf '  %scoverage%s: %d packages at or above their floor\n' "$G" "$N" "$checked"
