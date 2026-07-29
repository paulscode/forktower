#!/usr/bin/env bash
#
# Forktower developer toolchain setup.
#
# Installs, verifying every download against a pinned SHA-256:
#   • Go <GO_VERSION> into /usr/local/go          (needs sudo)
#   • golangci-lint v<GOLANGCI_VERSION> into ~/.local/bin  (no sudo)
#   • a PATH line in ~/.bashrc and ~/.profile so /usr/local/go/bin wins over
#     the distro Go in /usr/bin
#
# Safe to re-run: anything already at the pinned version is skipped, and the
# PATH line is only added once.
#
# Usage:  ./scripts/dev-setup.sh          (preferred — prompts for sudo when needed)
#         sudo ./scripts/dev-setup.sh     (also works; user-level files still go
#                                          to the invoking user's home)

set -euo pipefail

GO_VERSION="1.26.5"
GO_SHA256="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"

GOLANGCI_VERSION="2.12.2"
GOLANGCI_SHA256="8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553"

PATH_MARKER="# forktower dev setup: Go toolchain"
PATH_LINE='export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"'

# ── output helpers ───────────────────────────────────────────────────────────
if [ -t 1 ]; then
  B=$'\033[1m'; G=$'\033[1;32m'; Y=$'\033[1;33m'; R=$'\033[1;31m'; N=$'\033[0m'
else
  B=""; G=""; Y=""; R=""; N=""
fi
say()  { printf '%s==>%s %s\n' "$B" "$N" "$*"; }
ok()   { printf '%s  ok%s %s\n' "$G" "$N" "$*"; }
skip() { printf '%s skip%s %s\n' "$Y" "$N" "$*"; }
die()  { printf '%serror%s %s\n' "$R" "$N" "$*" >&2; exit 1; }

# ── preflight ────────────────────────────────────────────────────────────────
[ "$(uname -s)" = "Linux" ] || die "this script targets Linux (found $(uname -s))"

case "$(uname -m)" in
  x86_64)  GOARCH="amd64" ;;
  aarch64) GOARCH="arm64" ;;
  *)       die "unsupported architecture $(uname -m); install Go and golangci-lint by hand" ;;
esac

for tool in curl tar sha256sum install grep; do
  command -v "$tool" >/dev/null || die "required tool not found: $tool"
done

# Resolve whose home directory the user-level bits belong in, so that running
# this under sudo does not install golangci-lint into /root/.local/bin.
if [ "$(id -u)" -eq 0 ]; then
  if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
    TARGET_USER="$SUDO_USER"
    TARGET_HOME="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
    say "running as root via sudo; user-level files go to ${TARGET_USER} (${TARGET_HOME})"
  else
    TARGET_USER="root"
    TARGET_HOME="${HOME:-/root}"
    printf '%swarn%s running as real root; user-level files go to %s\n' "$Y" "$N" "$TARGET_HOME"
  fi
  SUDO=""
else
  TARGET_USER="$(id -un)"
  TARGET_HOME="$HOME"
  command -v sudo >/dev/null || die "sudo not found, and Go install needs root; re-run as root"
  SUDO="sudo"
  say "requesting sudo up front (needed to write /usr/local/go)"
  sudo -v
fi
[ -n "$TARGET_HOME" ] && [ -d "$TARGET_HOME" ] || die "could not resolve a home directory for $TARGET_USER"

# Run a command as the target user when we are root, otherwise directly.
as_user() {
  if [ "$(id -u)" -eq 0 ] && [ "$TARGET_USER" != "root" ]; then
    sudo -u "$TARGET_USER" "$@"
  else
    "$@"
  fi
}

TMPDIR_="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_"' EXIT

# fetch <url> <dest> <expected-sha256>
fetch() {
  local url="$1" dest="$2" want="$3" got
  curl -fL --retry 3 --connect-timeout 20 -o "$dest" "$url" \
    || die "download failed: $url"
  got="$(sha256sum "$dest" | cut -d' ' -f1)"
  [ "$got" = "$want" ] \
    || die "checksum mismatch for $(basename "$dest")
       expected $want
       got      $got
     Refusing to install. Do not retry blindly — check the release page."
  ok "checksum verified: $(basename "$dest")"
}

# ── 1. Go ────────────────────────────────────────────────────────────────────
say "Go ${GO_VERSION}"
if [ -x /usr/local/go/bin/go ] \
   && [ "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" = "go${GO_VERSION}" ]; then
  skip "/usr/local/go is already go${GO_VERSION}"
else
  if [ -e /usr/local/go ]; then
    existing="$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}' || true)"
    say "replacing existing /usr/local/go (${existing:-unrecognised install})"
  fi
  tarball="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
  fetch "https://go.dev/dl/${tarball}" "${TMPDIR_}/${tarball}" "$GO_SHA256"
  $SUDO rm -rf /usr/local/go
  $SUDO tar -C /usr/local -xzf "${TMPDIR_}/${tarball}"
  ok "installed $(/usr/local/go/bin/go version)"
fi

# ── 2. PATH ──────────────────────────────────────────────────────────────────
say "PATH (so /usr/local/go/bin precedes the distro Go in /usr/bin)"
for rc in "${TARGET_HOME}/.bashrc" "${TARGET_HOME}/.profile"; do
  if [ -f "$rc" ] && grep -Fq "$PATH_MARKER" "$rc"; then
    skip "$(basename "$rc") already updated"
  elif [ -f "$rc" ] && grep -Fq "$PATH_LINE" "$rc"; then
    skip "$(basename "$rc") already has the PATH line"
  else
    as_user tee -a "$rc" >/dev/null <<EOF

${PATH_MARKER}
${PATH_LINE}
EOF
    ok "appended to $(basename "$rc")"
  fi
done

# ── 3. golangci-lint ─────────────────────────────────────────────────────────
say "golangci-lint ${GOLANGCI_VERSION}"
BIN_DIR="${TARGET_HOME}/.local/bin"
if [ -x "${BIN_DIR}/golangci-lint" ] \
   && "${BIN_DIR}/golangci-lint" version 2>/dev/null | grep -Fq "${GOLANGCI_VERSION}"; then
  skip "${BIN_DIR}/golangci-lint is already ${GOLANGCI_VERSION}"
else
  gtar="golangci-lint-${GOLANGCI_VERSION}-linux-${GOARCH}.tar.gz"
  fetch "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_VERSION}/${gtar}" \
        "${TMPDIR_}/${gtar}" "$GOLANGCI_SHA256"
  tar -C "$TMPDIR_" -xzf "${TMPDIR_}/${gtar}"
  as_user mkdir -p "$BIN_DIR"
  as_user install -m 0755 \
    "${TMPDIR_}/golangci-lint-${GOLANGCI_VERSION}-linux-${GOARCH}/golangci-lint" \
    "${BIN_DIR}/golangci-lint"
  ok "installed to ${BIN_DIR}/golangci-lint"
fi

# ── 4. verify ────────────────────────────────────────────────────────────────
say "verifying"
export PATH="/usr/local/go/bin:${TARGET_HOME}/go/bin:${BIN_DIR}:$PATH"
go version        || die "go is not runnable"
golangci-lint version 2>&1 | head -1 || die "golangci-lint is not runnable"
printf '  GOPATH: %s\n' "$(go env GOPATH)"

printf '\n%sDone.%s Open a new terminal (or run: exec bash) so the PATH change takes effect,\n' "$G" "$N"
printf 'then confirm you get the new toolchain rather than the distro one:\n\n'
printf '    command -v go && go version     # expect /usr/local/go/bin/go, go%s\n\n' "$GO_VERSION"

if command -v dpkg >/dev/null && dpkg -s golang-go >/dev/null 2>&1; then
  printf '%snote%s The distro package golang-go (%s) is still installed. PATH order handles it,\n' \
    "$Y" "$N" "$(dpkg-query -W -f='${Version}' golang-go 2>/dev/null)"
  printf '     but if you want it gone, simulate the removal first — other packages may depend on it:\n'
  printf '         sudo apt -s remove golang-go\n\n'
fi
