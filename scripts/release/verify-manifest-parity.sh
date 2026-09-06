#!/usr/bin/env bash
# Prove that container/release-manifest.json describes the image this
# tree builds, by building it and hashing the binaries out of it. Issue
# #260.
#
# This is the half of the old guard 2 that a SHA comparison could not
# answer. That guard asked whether the manifest's commit equalled HEAD,
# which no tree can satisfy (the manifest is committed, so committing it
# always produces a commit the manifest does not name) and which would
# not have answered the real question even if it could: what matters is
# whether the bytes about to be pushed are the bytes the manifest
# records, and only a rebuild says that.
#
# The pattern is the one .github/workflows/release.yml already uses for
# the compliance bundle, which regenerates NOTICE and provenance/ inside
# the release and refuses if the result differs from what is committed.
# Drift becomes structurally impossible rather than merely loud: the
# release cannot ship a manifest describing different binaries, because
# the release is what produced the binaries it checks the manifest
# against.
#
# Run standalone to check a tree before cutting a release:
#
#     bash scripts/release/verify-manifest-parity.sh
#
# publish-image.sh runs it automatically, after every guard and before
# `docker buildx build --push`, because that command publishes in the
# same breath as it builds and there is no after.
#
# It is deliberately NOT wired into scripts/ci-local.sh. It is two full
# cross-architecture Docker builds, the manifest only changes at a
# release, and distribution/packaging already fails the build on the
# cheap questions (the pinned commit's reachability, the registry-digest
# and published-flag coupling).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

MANIFEST="${MANIFEST:-container/release-manifest.json}"

if [ ! -r "$MANIFEST" ]; then
  echo "refusing: ${MANIFEST} is not readable from $(pwd), so there is nothing to check the build against." >&2
  exit 2
fi

# The manifest is generated with a fixed two-space shape by
# record-release-hashes.sh, but this reads it with a real parser rather
# than sed: unlike publish-image.sh's json_string, which pulls two
# top-level scalars, this needs a nested per-architecture map, and a
# regex that silently matches the wrong "backup-manager" key would
# compare a hash against itself and pass.
if ! command -v python3 >/dev/null 2>&1; then
  echo "refusing: python3 is not on PATH, and this check reads a nested JSON structure out of ${MANIFEST}." >&2
  echo "A regex over nested JSON can silently match the wrong key and compare a hash against itself, which is a check that always passes." >&2
  exit 2
fi

read -r manifest_version manifest_commit unsafe <<EOF
$(python3 - "$MANIFEST" <<'PY'
import json, sys
with open(sys.argv[1]) as fh:
    m = json.load(fh)
print(m.get("version", ""), m.get("commit", ""), str(bool(m.get("unsafe_local_build", False))).lower())
PY
)
EOF

if [ "$unsafe" = "true" ]; then
  echo "refusing: ${MANIFEST} is stamped \"unsafe_local_build\": true, so it was generated with every reproducibility guard waived. There is no point proving a build matches it." >&2
  exit 2
fi
if [ -z "$manifest_commit" ] || [ -z "$manifest_version" ]; then
  echo "refusing: ${MANIFEST} records version '${manifest_version}' and commit '${manifest_commit}'; both are needed as build arguments, and a missing one would silently build something else." >&2
  exit 2
fi

arches="$(python3 - "$MANIFEST" <<'PY'
import json, sys
with open(sys.argv[1]) as fh:
    m = json.load(fh)
print(" ".join(a["architecture"] for a in m.get("architectures", [])))
PY
)"
if [ -z "$arches" ]; then
  echo "refusing: ${MANIFEST} records no architecture at all, so this check would pass by having nothing to compare." >&2
  exit 2
fi

# The same two-tool digest as record-release-hashes.sh, for the same
# reason: this has to produce the identical value on Linux and on macOS or
# the parity check is comparing a digest against a tool.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# recorded reads the manifest through a JSON parser rather than a sed
# expression, unlike publish-image.sh's json_string. The values here are
# nested two levels down and keyed by architecture, and this comparison is
# the whole point of the script: a pattern that silently matched the wrong
# architecture's digest would report parity against the wrong binary.
#
# An architecture the manifest does not carry prints an empty string, so
# the caller sees a mismatch and says which architecture it could not
# find, instead of this failing with a traceback.
recorded() {
  python3 - "$MANIFEST" "$1" "$2" <<'PY'
import json, sys
with open(sys.argv[1]) as fh:
    m = json.load(fh)
for a in m["architectures"]:
    if a["architecture"] == sys.argv[2]:
        print(a.get("binary_sha256", {}).get(sys.argv[3], ""))
        break
else:
    print("")
PY
}

echo "==> Proving ${MANIFEST} describes what this tree builds (version=${manifest_version} commit=${manifest_commit})" >&2

mismatches=0
for arch in $arches; do
  tag="backup-manager:parity-${arch}"
  echo "==> Building linux/${arch}" >&2
  docker buildx build \
    --platform "linux/${arch}" \
    --build-arg "VERSION=${manifest_version}" \
    --build-arg "COMMIT=${manifest_commit}" \
    -f container/Dockerfile \
    -t "$tag" \
    --load \
    . >&2

  cid=$(docker create --platform "linux/${arch}" "$tag" /backup-manager version)
  tmp=$(mktemp -d)
  docker cp "${cid}:/backup-manager" "${tmp}/backup-manager" >&2
  docker cp "${cid}:/backup-manager-web" "${tmp}/backup-manager-web" >&2
  docker rm "$cid" >/dev/null

  for binary in backup-manager backup-manager-web; do
    want="$(recorded "$arch" "$binary")"
    got="$(sha256_of "${tmp}/${binary}")"
    if [ -z "$want" ]; then
      echo "MISMATCH ${arch}/${binary}: the manifest records no hash for it at all, so nothing was compared." >&2
      mismatches=$((mismatches + 1))
    elif [ "$want" != "$got" ]; then
      # Both hashes, not "they differ". A verdict nobody can act on is
      # the failure mode this whole file exists to avoid.
      echo "MISMATCH ${arch}/${binary}:" >&2
      echo "  manifest records: ${want}" >&2
      echo "  this build made:  ${got}" >&2
      mismatches=$((mismatches + 1))
    else
      echo "    ok ${arch}/${binary} ${got}" >&2
    fi
  done

  rm -rf "$tmp"
done

if [ "$mismatches" -ne 0 ]; then
  echo >&2
  echo "refusing: ${mismatches} recorded hash(es) do not describe what this tree builds." >&2
  echo "Either the manifest is stale (regenerate it: VERSION=${manifest_version} scripts/release/record-release-hashes.sh, from a clean checkout of a commit already on main), or something between ${manifest_commit} and HEAD changed the image, in which case the release is not the build the manifest claims." >&2
  echo "Nothing has been pushed." >&2
  exit 2
fi

echo "==> Every recorded binary hash matches this build." >&2
