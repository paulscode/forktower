#!/usr/bin/env bash
#
# No compiled binary belongs in the source tree.
#
# One did: `go build ./cmd/forkbench` with no -o writes into the working
# directory, and five megabytes of stale, platform-specific executable was
# committed and went unnoticed for a whole milestone. .gitignore now covers the
# two names, but a name-based rule only stops the mistakes somebody has already
# made. This checks what the files *are*.
#
# Run by `make lint`.

set -euo pipefail

cd "$(dirname "$0")/.."

if [ -t 1 ]; then R=$'\033[1;31m'; G=$'\033[1;32m'; N=$'\033[0m'
else R=""; G=""; N=""; fi

violations=0

while read -r f; do
  [ -f "$f" ] || continue
  case "$(file -b --mime-type "$f" 2>/dev/null || echo unknown)" in
    application/x-executable | application/x-sharedlib | application/x-mach-binary | \
    application/x-dosexec | application/x-pie-executable)
      if [ "$violations" -eq 0 ]; then
        printf '%sartifact check failed%s — compiled binaries must not be committed:\n\n' "$R" "$N"
      fi
      printf '  %s (%s)\n' "$f" "$(du -h "$f" | cut -f1)"
      violations=$((violations + 1))
      ;;
  esac
done < <(git ls-files -z 2>/dev/null | tr '\0' '\n')

if [ "$violations" -gt 0 ]; then
  printf '\nBuild into bin/ instead: `make build`, or `go build -o bin/ ./cmd/...`.\n'
  exit 1
fi

printf '  %sartifacts%s: no compiled binaries are tracked\n' "$G" "$N"
