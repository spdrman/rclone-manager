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
# the resolved digest") is the digest the registry assigns on push. The
# registry is ghcr.io and the reference is
# ghcr.io/spdrman/backup-manager (apps/common/packaging/canonical.json is
# the single source of truth for it), so the gap is no longer "no
# registry is configured": it is that nothing has been pushed there yet,
# which canonical.json records as image.published false. This script
# therefore writes `registry_digest: null` per architecture, a slot with
# one obvious way to fill it (`docker buildx build --push` prints the
# digest, `docker buildx imagetools inspect <ref>` reads it back), and
# keeps recording the LOCAL image ID `docker build` produced, labeled
# honestly as "local_image_id_sha256" and never as "digest". Doing the
# push, and signing and attesting what it points at, is issue #88's work.
#
# Where this script is run matters as much as what it records. The
# manifest is only worth anything while the commit it pins stays in
# main's history, and a commit made on a feature branch does not: a
# squash merge rewrites it, and the manifest is left pinning a SHA nobody
# can check out. That is issue #174, and the guards below are what stop
# it happening a second time. Run this from a clean checkout of a commit
# that is already on main.
#
# The guards are not exercised by running a release. They are driven on
# every non-FAST ci-local.sh run by
# scripts/tests/record-release-hashes-guards.test.sh, which builds a
# throwaway repository per refusal and asserts both the exit code and the
# message, through the GUARDS_ONLY=1 seam below. A refusal that is only
# ever executed at release time, after two Docker cross-builds, is a
# refusal nobody has watched work.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

VERSION="${VERSION:-$(git describe --tags --always)}"
COMMIT="${COMMIT:-$(git rev-parse HEAD)}"
ARCHES="${ARCHES:-amd64 arm64}"
REACHABLE_FROM="${REACHABLE_FROM:-origin/main}"
UNSAFE="${UNSAFE_LOCAL_BUILD:-0}"

# Where the output lands is part of the guard, not a detail. A waived run
# produces a manifest that is indistinguishable from a good one, so it
# must not default to the tracked path: one stray `git add -A` is all it
# takes for a throwaway experiment to become the file every parity check
# trusts, which is #174's harm arriving through the one door left open.
# Under a waiver the default moves to container/.generated/, which
# .gitignore already covers, and overwriting the tracked manifest takes a
# deliberate OUT=.
if [ "$UNSAFE" = "1" ]; then
  OUT="${OUT:-container/.generated/release-manifest.local.json}"
  mkdir -p "$(dirname "$OUT")"
else
  OUT="${OUT:-container/release-manifest.json}"
fi

# Refuse to record a manifest nobody will be able to reproduce (#174).
#
# Five separate ways that happens, all of them silent without this:
#
#   1. COMMIT does not name a commit in this repository at all, so
#      nothing recorded against it could ever be checked out. Both
#      operands of the ancestry question are checked, not just the ref.
#   2. COMMIT names something other than what is actually being built.
#      The build context is the working tree, so a hand-set COMMIT that
#      disagrees with HEAD stamps one commit onto the binaries of
#      another.
#   3. The working tree is dirty in a path the image is built from, so
#      the binaries are not the ones that commit describes.
#   4. REACHABLE_FROM cannot be resolved, so the ancestry question cannot
#      be asked at all.
#   5. COMMIT is not on main. This is the one that already bit us: the
#      previous manifest pinned c51a07f, a feature-branch commit that
#      GitHub's squash merge rewrote out of existence, so every parity
#      check comparing against it was comparing against a build no
#      checkout could reproduce.
#
# The fifth refusal keeps "git said no" apart from "git could not
# answer", the same distinction CommitReachableFrom preserves on the Go
# side. `git merge-base --is-ancestor` exits 1 for no and 128 when it
# cannot decide (a shallow clone, a missing object), and reporting a
# shallow clone as "not an ancestor of origin/main" sends the operator to
# regenerate from a different commit over a problem that is not in the
# manifest at all.
#
# UNSAFE_LOCAL_BUILD=1 waives all five, for a throwaway local experiment.
# What it writes is stamped and lands on a gitignored path, so a waived
# manifest cannot be committed by accident and cannot be mistaken for a
# good one if it is.
if [ "$UNSAFE" != "1" ]; then
  if ! git rev-parse --verify --quiet "${COMMIT}^{commit}" >/dev/null; then
    echo "refusing: COMMIT=${COMMIT} does not name a commit in this repository, so nothing recorded against it could be checked out." >&2
    exit 2
  fi
  head_commit=$(git rev-parse HEAD)
  if [ "$COMMIT" != "$head_commit" ]; then
    echo "refusing: COMMIT=${COMMIT} is not HEAD (${head_commit}), so the recorded commit would not describe the tree being built." >&2
    exit 2
  fi
  dirty=$(git status --porcelain -- core apps ui container/Dockerfile)
  if [ -n "$dirty" ]; then
    echo "refusing: the working tree is dirty in a path the image is built from, so these hashes would not be reproducible from ${COMMIT}:" >&2
    echo "$dirty" >&2
    exit 2
  fi
  if ! git rev-parse --verify --quiet "${REACHABLE_FROM}^{commit}" >/dev/null; then
    echo "refusing: cannot resolve ${REACHABLE_FROM} to check ${COMMIT} against it. Fetch it, or set REACHABLE_FROM." >&2
    exit 2
  fi
  ancestry_rc=0
  git merge-base --is-ancestor "$COMMIT" "$REACHABLE_FROM" || ancestry_rc=$?
  if [ "$ancestry_rc" -eq 1 ]; then
    echo "refusing: ${COMMIT} is not an ancestor of ${REACHABLE_FROM}." >&2
    echo "A commit that is not already on main does not survive a squash merge, and the manifest would pin a SHA that leaves the history (#174)." >&2
    echo "Regenerate from a checkout of a commit that is already on main." >&2
    exit 2
  elif [ "$ancestry_rc" -ne 0 ]; then
    echo "refusing: git could not decide whether ${COMMIT} is an ancestor of ${REACHABLE_FROM} (merge-base --is-ancestor exited ${ancestry_rc}, which is neither 0 nor 1)." >&2
    echo "That is a fact about this checkout, not about the manifest: a shallow clone (git fetch --unshallow) or a missing object produces it, and the commit may well be perfectly reachable." >&2
    echo "Nothing is recorded, because a check that did not run is not a check that passed." >&2
    exit 2
  fi
else
  echo "warning: UNSAFE_LOCAL_BUILD=1 waives all five reproducibility guards." >&2
  echo "warning: the manifest is stamped \"unsafe_local_build\": true, which apps/common/packaging refuses, and it defaults to a gitignored path. Do not commit it." >&2
fi

# GUARDS_ONLY=1 stops here, after every refusal and before the first
# Docker build, so the guards can be driven in a test. Nothing sets it on
# a real run and it is read nowhere else, so it cannot change what a
# release records.
if [ "${GUARDS_ONLY:-0}" = "1" ]; then
  echo "==> GUARDS_ONLY=1: every guard passed; stopping before the Docker build. Would write ${OUT}" >&2
  exit 0
fi

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
      "local_image_id_sha256": "${local_image_id}",
      "registry_digest": null
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

unsafe_stamp=""
if [ "$UNSAFE" = "1" ]; then
  # Present only on a waived run, so a good manifest is unchanged and the
  # Go side reads a missing key as false. Its whole job is to make an
  # unsafe manifest impossible to commit quietly.
  unsafe_stamp='
  "unsafe_local_build": true,'
fi

cat > "$OUT" <<EOF
{${unsafe_stamp}
  "version": "${VERSION}",
  "commit": "${COMMIT}",
  "generated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "note": "Regenerate with scripts/release/record-release-hashes.sh, and only at a commit that is ALREADY on main. A commit recorded from a feature branch stops existing the moment that branch is squash merged, and the manifest is then pinned to a SHA nobody can check out: that is issue #174, which is why the script now refuses to record a commit that is not an ancestor of origin/main and why apps/common/packaging's release-manifest-integrity check re-asks the same question on every run. binary_sha256 is hashed from the two binaries extracted out of the built image, so it is real evidence of what was compiled. registry_digest is the digest ghcr.io assigns ghcr.io/spdrman/backup-manager on push (docker buildx build --push prints it, docker buildx imagetools inspect reads it back), and it is null on every architecture below because nothing has been pushed yet; apps/common/packaging/canonical.json records the same fact as image.published false, and the two move together. local_image_id_sha256 is not a stand-in for it: it is the local Docker image ID this build produced, which identifies the image on the machine that built it and nowhere else. Filling registry_digest in from a real push, and signing and attesting what it points at, is issue #88's work, not this file's.",
  "architectures": [
${joined}
  ]
}
EOF

echo "==> Wrote ${OUT}" >&2
cat "$OUT"
