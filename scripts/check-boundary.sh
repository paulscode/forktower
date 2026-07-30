#!/usr/bin/env bash
#
# The project's planning documents are private and are not published with this
# repository. Anything that ships must therefore stand on its own: a reader of
# this repository cannot follow a reference into a document they do not have, so
# a dangling reference is worse than no reference at all.
#
# This checks that no file outside the private directory cites one of those
# documents, their internal identifiers, or the maintainer's test hosts.
#
# Untracked files are checked too. An earlier version listed only tracked files,
# which meant a brand-new file was invisible to this gate until the moment it was
# committed — exactly one step too late to be useful. Ignored files are still
# skipped, since those are build output and scratch space that never ship.
# When something needs saying publicly, it belongs in docs/ — written for its
# reader, not as a pointer.
#
# Run by `make lint` and in CI. Exits non-zero on the first violation.

set -euo pipefail

cd "$(dirname "$0")/.."

if [ -t 1 ]; then R=$'\033[1;31m'; G=$'\033[1;32m'; N=$'\033[0m'; else R=""; G=""; N=""; fi

# Patterns are assembled from fragments so that this file does not match itself.
ids="$(printf 'F''T-[0-9]{3}|Q''-[0-9]{1,2}|S''-[0-9]{1,2}')"
priv="$(printf 'internal''_docs')"
hosts="$(printf 'worthy''-maverick|wide''-treason|pauls''-umbrel')"
PATTERN="${priv}|${ids}|${hosts}"

# Files exempt by nature. Each has to name the private directory in order to do
# its job, which is the opposite of leaking it:
#
#   - this script, which names the patterns it looks for;
#   - .gitignore and .dockerignore, which must name the directory to exclude it —
#     .dockerignore especially, since without that line the whole planning
#     archive would be copied into every image build context;
#   - the private directory itself, which is not shipped at all.
mapfile -t candidates < <(
  git ls-files -z --cached --others --exclude-standard 2>/dev/null | tr '\0' '\n' |
    grep -v '^scripts/check-boundary\.sh$' |
    grep -v '^\.gitignore$' |
    grep -v '^\.dockerignore$' |
    grep -v '^internal_docs/' || true
)

if [ "${#candidates[@]}" -eq 0 ]; then
  echo "  boundary: no files yet, nothing to check"
  exit 0
fi

violations=0
for f in "${candidates[@]}"; do
  [ -f "$f" ] || continue
  # Text files only; skip anything binary.
  if ! grep -Iq . "$f" 2>/dev/null; then continue; fi
  if hits=$(grep -nEI "$PATTERN" "$f" 2>/dev/null); then
    if [ "$violations" -eq 0 ]; then
      printf '%sboundary check failed%s — shipped files must not reference the private planning docs:\n\n' "$R" "$N"
    fi
    while IFS= read -r line; do
      printf '  %s:%s\n' "$f" "$line"
    done <<< "$hits"
    violations=$((violations + 1))
  fi
done

if [ "$violations" -gt 0 ]; then
  printf '\nMove the content into docs/ and reference that instead.\n'
  exit 1
fi

printf '  %sboundary%s: %d files clean\n' "$G" "$N" "${#candidates[@]}"
