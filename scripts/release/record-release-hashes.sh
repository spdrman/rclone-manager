#!/usr/bin/env bash
# Phase 4 TDD Gate (docs/EPIC-B-multi-nas.md §72): "automated/provider
# conformance tests SHALL verify core version/hash parity" and issue
# #82/B4.1's own acceptance criterion "binary/image hashes are recorded."
# Section 8's release manifest rule is the same claim stated for the
# whole product: "The release manifest SHALL prove core parity through
# binary hashes and image/package digests."
#
# This builds container/Dockerfile for each of linux/amd64 and
# linux/arm64, extracts both shipped binaries (/backup-manager and
# /backup-manager-web) from each built image, hashes them with SHA-256,
# and writes the result to container/release-manifest.json - a record a
# reviewer (or a future provider package, per §8's "provider-specific
# packages MUST NOT contain different lifecycle logic") can diff against
# a claim of core parity, rather than trusting it by assertion.
#
# What this does NOT do: push anything to a registry. The "image digest"
# a real release records (docs/EPIC-B-multi-nas.md §37.2's "SHOULD record
# the resolved digest") is the digest a registry assigns on push
# (`docker buildx build --push ...` prints it, or
# `docker buildx imagetools inspect <ref>` reads it back). Without a
# configured registry to push to, this script instead records the LOCAL
# image ID `docker build` produced (labeled honestly as
# "local_image_id_sha256", never as "digest") - real evidence of what was
# actually built and hashed, not a fabricated stand-in for a registry
# digest that does not exist yet. See this script's own README note in
# docs/deployment.md for exactly this distinction.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

VERSION="${VERSION:-$(git describe --tags --always)}"
COMMIT="${COMMIT:-$(git rev-parse HEAD)}"
OUT="${OUT:-container/release-manifest.json}"
ARCHES="${ARCHES:-amd64 arm64}"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

echo "==> Recording release hashes for VERSION=${VERSION} COMMIT=${COMMIT}" >&2

entries=()
for arch in $ARCHES; do
  tag="backup-manager:release-hashes-${arch}"
  echo "==> Building linux/${arch} (${tag})" >&2
  docker buildx build \
    --platform "linux/${arch}" \
    --build-arg "VERSION=${VERSION}" \
    --build-arg "COMMIT=${COMMIT}" \
    -f container/Dockerfile \
    -t "$tag" \
    --load \
    . >&2

  cid=$(docker create --platform "linux/${arch}" "$tag" /backup-manager version)
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  docker cp "${cid}:/backup-manager" "${tmp}/backup-manager" >&2
  docker cp "${cid}:/backup-manager-web" "${tmp}/backup-manager-web" >&2
  docker rm "$cid" >/dev/null

  backup_manager_sha=$(sha256_of "${tmp}/backup-manager")
  backup_manager_web_sha=$(sha256_of "${tmp}/backup-manager-web")
  local_image_id=$(docker images --no-trunc --format '{{.ID}}' "$tag" | head -n1 | sed 's/^sha256://')

  rm -rf "$tmp"
  trap - EXIT

  entries+=("$(cat <<EOF
    {
      "architecture": "${arch}",
      "binary_sha256": {
        "backup-manager": "${backup_manager_sha}",
        "backup-manager-web": "${backup_manager_web_sha}"
      },
      "local_image_id_sha256": "${local_image_id}"
    }
EOF
)")
done

joined=""
for i in "${!entries[@]}"; do
  if [ "$i" -gt 0 ]; then
    joined+=$',\n'
  fi
  joined+="${entries[$i]}"
done

cat > "$OUT" <<EOF
{
  "version": "${VERSION}",
  "commit": "${COMMIT}",
  "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "note": "local_image_id_sha256 is this build's local Docker image ID, not a registry digest - no registry is configured for this repository yet. A real release additionally records the digest a registry assigns on push (docker buildx build --push, or docker buildx imagetools inspect <ref>).",
  "architectures": [
${joined}
  ]
}
EOF

echo "==> Wrote ${OUT}" >&2
cat "$OUT"
