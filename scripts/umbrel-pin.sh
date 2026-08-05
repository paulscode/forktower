#!/usr/bin/env bash
#
# Pin the Umbrel app to an exact image, by digest.
#
# While an app is in testing its compose file names a moving tag, which is
# convenient for us and wrong for a user: they should get the bits that were
# tested, not whatever the tag points at on the day they install. This rewrites
# that line to `<repo>:<version>@sha256:...`.
#
# **The digest must be the image index's, not an architecture's.** A multi-arch
# tag is an index listing one image per platform, and `docker manifest inspect`
# shows those per-platform digests prominently — they are the obvious thing to
# copy and the wrong thing to pin. Pinning one would serve that architecture and
# leave every other user unable to install at all. For this project that means
# the Raspberry Pi users, who are arguably the likeliest audience.
#
# So this reads the index digest, and refuses unless the index actually carries
# both architectures. A release that silently dropped arm64 would otherwise be
# pinned in place and look perfectly fine from here.

set -euo pipefail

cd "$(dirname "$0")/.."

compose="deploy/umbrel/docker-compose.yml"
repo="${IMAGE_NAME:-paulscode/forktower}"
tag="${1:-}"

if [ -z "${tag}" ]; then
  tag="$(yq e '.version' manifest.yaml 2>/dev/null || true)"
fi
[ -n "${tag}" ] && [ "${tag}" != "null" ] || {
  printf '  could not work out which tag to pin\n' >&2; exit 1; }

fail() { printf '  %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || fail "docker is needed to read the image index"
command -v jq >/dev/null || fail "jq is needed to read the image index"

raw="$(docker buildx imagetools inspect "${repo}:${tag}" --raw 2>/dev/null || true)"
[ -n "${raw}" ] || fail "${repo}:${tag} is not in the registry — push it first"

media="$(printf '%s' "${raw}" | jq -r '.mediaType // ""')"
case "${media}" in
  *image.index*|*manifest.list*) ;;
  *)
    fail "${repo}:${tag} is a single image, not a multi-architecture index.
  Pinning it would leave every other architecture unable to install.
  Build it with \`make image-push\`, which pushes both."
    ;;
esac

arches="$(printf '%s' "${raw}" \
  | jq -r '[.manifests[] | select(.platform.os=="linux") | .platform.architecture] | sort | join(",")')"

# Both must be present; extras are fine. Requiring an exact set would refuse a
# release that had *gained* an architecture, which is not a problem — the
# property worth enforcing is that nobody who could install yesterday cannot
# install today.
for want in amd64 arm64; do
  printf '%s' ",${arches}," | grep -q ",${want}," || fail "${repo}:${tag} carries
  [${arches}] and is missing ${want}. Every user on that architecture would be
  unable to install, and pinning it here would freeze that in place while
  looking perfectly fine from this machine."
done

digest="$(docker buildx imagetools inspect "${repo}:${tag}" --format '{{.Manifest.Digest}}' 2>/dev/null)"
[ -n "${digest}" ] || fail "could not read the index digest for ${repo}:${tag}"

# Rewrite the one line, leaving the comments above it — which explain why the
# digest is there — exactly where they are.
tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
sed -E "s|^([[:space:]]*)image: ${repo}(:[^@[:space:]]*)?(@sha256:[a-f0-9]+)?$|\\1image: ${repo}:${tag}@${digest}|" \
  "${compose}" > "${tmp}"

grep -q "image: ${repo}:${tag}@${digest}" "${tmp}" \
  || fail "the image line in ${compose} did not match the expected shape; pin it by hand"

mv "${tmp}" "${compose}"
trap - EXIT

printf '  pinned %s to\n    %s:%s@%s\n' "${compose}" "${repo}" "${tag}" "${digest}"
printf '  architectures: %s\n' "${arches}"
printf '\n  Now: make umbrel-sync, then commit and push the store repository.\n'
