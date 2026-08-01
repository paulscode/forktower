#!/usr/bin/env bash
#
# Generates every icon the project ships, from the one piece of source art.
#
# All outputs are committed, so a clean checkout builds without ImageMagick and
# without the source file — which is deliberate: `references/` is not published,
# and a build that needs an unpublished input is a build nobody else can run.
#
# Idempotent, and byte-identical on a re-run: `-strip` removes the timestamps and
# other metadata that would otherwise make every run a diff. A generator whose
# output churns teaches people to stop reading its diffs.
#
# ImageMagick 6 on this machine, so the command is `convert`, not `magick`.
#
# Usage: scripts/make-icons.sh [--check]
#   --check  regenerate into a temporary directory and fail if anything differs

set -euo pipefail

cd "$(dirname "$0")/.."

SOURCE="references/icon.png"

# name:size — the size is what the consumer needs, and the comment says who.
PNG_OUTPUTS=(
  # 256, and it has to be: StartOS 0.4.x's packer derives an .ico from this and
  # fails with "cannot filter out unhashed file icon.ico" at any other size —
  # an error that says nothing about icons. 0.3.5.1 is happy with 256 too, so
  # one file serves both.
  "icon.png:256"                # StartOS manifest assets.icon, both versions
  "web/favicon.png:32"          # dashboard favicon
  "web/logo.png:192"            # dashboard header
  "deploy/umbrel/icon.png:192"  # Umbrel store listing fallback
)

# The SVG wrapper. There is no vector original — the art is a raster render — so
# this is a 256x256 SVG around a base64 PNG, in the same form as the sibling
# projects' stores, which is what those stores are known to accept.
SVG_OUTPUTS=(
  "icon.svg"
  "deploy/umbrel/icon.svg"
)
SVG_SIZE=256

if [ -t 1 ]; then R=$'\033[1;31m'; G=$'\033[1;32m'; N=$'\033[0m'
else R=""; G=""; N=""; fi

check_only=0
if [ "${1:-}" = "--check" ]; then
  check_only=1
fi

if ! command -v convert >/dev/null 2>&1; then
  printf '%sImageMagick is not installed.%s\n' "$R" "$N" >&2
  printf 'The committed icons are already correct; you only need this to change them.\n' >&2
  exit 1
fi

if [ ! -f "$SOURCE" ]; then
  printf '%s%s is missing.%s\n' "$R" "$SOURCE" "$N" >&2
  printf 'It holds the source art and is deliberately not published, so this script\n' >&2
  printf 'cannot run from a plain checkout. The committed outputs are what ships.\n' >&2
  exit 1
fi

# render writes one PNG at one size.
render_png() {
  local path="$1" size="$2"
  mkdir -p "$(dirname "$path")"
  convert "$SOURCE" -resize "${size}x${size}" -strip "PNG32:$path"
}

# render_svg wraps a freshly rendered PNG. Written with printf rather than a
# heredoc so there is no trailing newline: the sibling stores' files have none,
# and matching them exactly is the whole point of copying the form.
render_svg() {
  local path="$1" png b64
  mkdir -p "$(dirname "$path")"

  png="$(mktemp)"
  convert "$SOURCE" -resize "${SVG_SIZE}x${SVG_SIZE}" -strip "PNG32:$png"
  b64="$(base64 -w0 "$png")"
  rm -f "$png"

  printf '<svg xmlns="http://www.w3.org/2000/svg" width="%s" height="%s" viewBox="0 0 %s %s"><image width="%s" height="%s" preserveAspectRatio="xMidYMid meet" href="data:image/png;base64,%s"/></svg>' \
    "$SVG_SIZE" "$SVG_SIZE" "$SVG_SIZE" "$SVG_SIZE" "$SVG_SIZE" "$SVG_SIZE" "$b64" > "$path"
}

if [ "$check_only" -eq 1 ]; then
  scratch="$(mktemp -d)"
  trap 'rm -rf "$scratch"' EXIT

  differs=0
  for entry in "${PNG_OUTPUTS[@]}"; do
    path="${entry%%:*}"; size="${entry##*:}"
    render_png "$scratch/$path" "$size"
    if ! cmp -s "$scratch/$path" "$path"; then
      printf '%s  %s differs from what the source art produces%s\n' "$R" "$path" "$N"
      differs=1
    fi
  done
  for path in "${SVG_OUTPUTS[@]}"; do
    render_svg "$scratch/$path"
    if ! cmp -s "$scratch/$path" "$path"; then
      printf '%s  %s differs from what the source art produces%s\n' "$R" "$path" "$N"
      differs=1
    fi
  done

  if [ "$differs" -ne 0 ]; then
    printf '\nRun scripts/make-icons.sh and commit the result.\n'
    exit 1
  fi
  printf '  %sicons%s: all %d outputs match the source art\n' \
    "$G" "$N" "$(( ${#PNG_OUTPUTS[@]} + ${#SVG_OUTPUTS[@]} ))"
  exit 0
fi

for entry in "${PNG_OUTPUTS[@]}"; do
  path="${entry%%:*}"; size="${entry##*:}"
  render_png "$path" "$size"
  printf '  %-28s %sx%s\n' "$path" "$size" "$size"
done

for path in "${SVG_OUTPUTS[@]}"; do
  render_svg "$path"
  printf '  %-28s %sx%s wrapper\n' "$path" "$SVG_SIZE" "$SVG_SIZE"
done

printf '  %sicons%s: done\n' "$G" "$N"
