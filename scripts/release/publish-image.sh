#!/usr/bin/env bash
# Push the canonical multi-architecture image, read its registry digests
# back, sign it and attest its SBOM. Issue #88 (B5.2),
# docs/EPIC-B-multi-nas.md §61 ("container images SHOULD be signed where
# the project's registry/tooling supports it") and §73 Work Package 5.2.
#
# This is the step #174 left open. That issue built the slot: the release
# manifest carries an explicit per-architecture registry_digest, null for
# exactly as long as distribution/packaging/canonical.json records
# image.published false, with a test that makes the two move together. What
# it could not do was fill the slot in, because filling it in requires a
# real push to a real registry. This script is that push.
#
# NOTHING HAS BEEN PUSHED YET. Running this is an operator action, not a
# gate step, and it is deliberately not wired into scripts/ci-local.sh or
# any workflow that runs on its own. Three reasons, in order of how much
# they matter:
#
#   1. It publishes. ghcr.io/spdrman/backup-manager:1.0.0 is a first
#      publication under a semantic version, and a registry tag is not a
#      thing you take back cleanly. Whoever does it should mean to.
#   2. It needs a credential this repository does not and must not hold.
#      See "Credentials" below: there is no key to store here, and that is
#      a design decision rather than an omission.
#   3. Pushing from a feature branch would put an image in the registry
#      built from a commit that is not on main, which is #174's failure
#      moved out of the manifest and into the registry, where no ancestry
#      check can reach it. Guard 2 below refuses it.
#
# ----------------------------------------------------------------------
# Credentials
# ----------------------------------------------------------------------
#
# No signing key is generated, stored or read from disk by this script, and
# none should ever exist in this repository.
#
# The signature is keyless (Sigstore). In the release workflow the job's
# GitHub OIDC token is exchanged for a short-lived Fulcio certificate bound
# to the workflow's own identity, the signature is recorded in Rekor, and
# the certificate expires in minutes. There is no long-lived private key,
# so there is nothing to rotate, nothing to leak and nothing to commit by
# accident. The identity a verifier pins is the workflow path plus the tag
# ref, which is recorded in provenance/release-provenance.json
# under signing.identity so it is settled before the first signature rather
# than discovered after it.
#
# For a release signed by hand off CI, cosign reads a key from the
# environment (`--key env://COSIGN_PRIVATE_KEY`) and this script requires
# that form. A key file on disk is refused by guard 5, which asks git for
# every path in the working tree matching a key-shaped name, tracked,
# untracked or ignored, because "I will delete it afterwards" is how key
# material ends up somewhere it cannot be taken back from.
#
# Registry authentication is `docker login ghcr.io` in the operator's own
# session, or GITHUB_TOKEN in the workflow. This script never reads, echoes
# or writes either.
#
# ----------------------------------------------------------------------
# After a successful push
# ----------------------------------------------------------------------
#
# This script prints the per-architecture digests and stops. Recording them
# is two deliberate edits, in this order:
#
#   1. distribution/packaging/canonical.json: image.published false -> true
#   2. container/release-manifest.json: registry_digest null -> the digest
#      printed for that architecture
#
# TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag fails on
# either one alone, which is the point: a published flag with no digest,
# and a digest with no published flag, are both half-truths.
#
# ----------------------------------------------------------------------
# The guards
# ----------------------------------------------------------------------
#
# Eight refusals, each with its own message and its own exit-2 path, driven
# on every non-FAST ci-local.sh run by
# scripts/tests/publish-image-guards.test.sh through the GUARDS_ONLY=1 seam
# below. The ninth, binary parity against the release manifest, needs a
# Docker build and so runs past that seam; scripts/release/verify-manifest-parity.sh
# carries its own reasoning. A refusal that only executes at release time, after a
# two-architecture build, is a refusal nobody has watched work, and this
# script's whole job is to be trusted on the one day it runs.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

CANONICAL="distribution/packaging/canonical.json"
MANIFEST="container/release-manifest.json"
SBOM="provenance/sbom.spdx.json"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
SIGN="${SIGN:-1}"
DRY_RUN="${DRY_RUN:-0}"

json_string() {
  # Reads one top-level-ish "key": "value" pair. Deliberately dumb: the
  # two files it reads are generated with a fixed two-space shape, and a
  # JSON parser is not worth a dependency in a script whose failure mode
  # is caught by guard 1 or 6 anyway.
  sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1" | head -n1
}

# --- guard 1: the files this reads have to be here and readable ---------
for f in "$CANONICAL" "$MANIFEST"; do
  if [ ! -r "$f" ]; then
    echo "refusing: ${f} is not readable from $(pwd), so this script cannot tell what it would be publishing." >&2
    exit 2
  fi
done

REFERENCE="${REFERENCE:-$(json_string "$CANONICAL" reference)}"
if [ -z "$REFERENCE" ]; then
  echo "refusing: no image reference in ${CANONICAL}; that file is the single source of truth for what gets published, and publishing to a reference invented here is how a provider package ends up pointing at an image nobody built." >&2
  exit 2
fi

# --- guard 2: publish only a build this history can reach ---------------
#
# This guard used to require the manifest's commit to EQUAL HEAD, and no
# tree could satisfy that (#260). The manifest is committed, so
# generating it at commit C produces a file naming C and committing that
# file produces C+1; at C+1 the guard refuses, and checking out C does
# not help because C's own tree carries the previous release's manifest.
# The repository's history says the same thing out loud: the manifest was
# last regenerated by 3045f9e and it records 8ad3100. The guard has never
# been satisfiable, and nobody noticed because nothing has ever been
# published.
#
# The property that guard was reaching for is not "the manifest names
# HEAD". It is "the bytes about to be pushed are the bytes the manifest
# describes, and they are reproducible from a commit that stays
# checkable". Those are two separate questions and they are now asked
# separately:
#
#   * this guard asks the reachability half, textually and cheaply;
#   * scripts/release/verify-manifest-parity.sh asks the bytes half,
#     empirically, by rebuilding each architecture and hashing the two
#     binaries out of the image. It runs below, BEFORE the push, because
#     `docker buildx build --push` publishes in the same command that
#     builds.
#
# A SHA comparison could never have answered the bytes half anyway. The
# release cut necessarily adds a commit on top of the build it describes,
# and .dockerignore excludes container/, docs/, .github/, every *.md and
# all of provenance/ but one file from the build context, so that commit
# provably cannot change the image. Only a rebuild can say whether some
# other commit did.
#
# The one exception is provenance/third-party-licenses.json, which the
# runtime stage COPYs to /licenses so an image-only recipient can reach
# the inventory LICENSE and NOTICE point at (#407). It does not weaken
# the sentence above: that file is derived from the Go module graph and
# ui/shared's lockfile, and a release cut changes neither, so it comes
# out of `go run ./cmd/provenance -write` byte-identical. The rest of
# provenance/ stays excluded, release-provenance.json and checksums.txt
# among them, and those two do change on a cut, which is why the
# exception in .dockerignore names one file rather than opening the
# directory.
manifest_commit="$(json_string "$MANIFEST" commit)"
head_commit="$(git rev-parse HEAD)"
if [ -z "$manifest_commit" ]; then
  echo "refusing: ${MANIFEST} pins no commit, so there is nothing to check the tree being pushed against." >&2
  exit 2
fi
if ! git rev-parse --verify --quiet "${manifest_commit}^{commit}" >/dev/null; then
  echo "refusing: ${MANIFEST} pins ${manifest_commit}, which is not a commit in this repository, so nothing it records can be checked out." >&2
  exit 2
fi

# An ancestor of HEAD, not merely a commit that exists. A manifest naming
# a commit on some other branch describes a build this checkout is not
# about to make.
ancestry_rc=0
git merge-base --is-ancestor "$manifest_commit" "$head_commit" || ancestry_rc=$?
if [ "$ancestry_rc" -eq 1 ]; then
  echo "refusing: ${MANIFEST} pins ${manifest_commit}, which is not an ancestor of HEAD (${head_commit})." >&2
  echo "The image would be built from this tree and published under a manifest describing a build that is not in this history at all." >&2
  echo "Regenerate the manifest from a commit this branch contains." >&2
  exit 2
elif [ "$ancestry_rc" -ne 0 ]; then
  echo "refusing: git could not decide whether ${manifest_commit} is an ancestor of HEAD (merge-base --is-ancestor exited ${ancestry_rc}, which is neither 0 nor 1)." >&2
  echo "That is a fact about this checkout, not about the manifest: a shallow clone (git fetch --unshallow) or a missing object produces it." >&2
  echo "Nothing is pushed, because a check that did not run is not a check that passed." >&2
  exit 2
fi

# --- guard 2b: the pinned commit has to stay checkable (#174) -----------
#
# #174's harm was a manifest pinning a feature-branch commit that a squash
# merge rewrote out of existence. The generator refuses that at write
# time; this asks it again at publish time, because a registry tag is the
# one artifact that cannot be corrected by regenerating a file.
#
# Two refs answer, and both are rewrite-free by policy rather than by
# luck. origin/main only grows. origin/release is append-only, never
# force-pushed and never squash-merged into, which is what
# docs/release-branch.md commits the project to and what makes a commit
# on it as checkable as one on main. A local main or release is accepted
# as a fallback and says it is one, in the same shape
# distribution/packaging's ResolveAncestryRef already uses.
#
# CHECKABLE_FROM overrides the ref list, for a fork or a mirror whose
# rewrite-free ref is named something else.
reachable_from=""
undecided_refs=""
for ref in ${CHECKABLE_FROM:-origin/main origin/release main release}; do
  git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null || continue
  rc=0
  git merge-base --is-ancestor "$manifest_commit" "$ref" || rc=$?
  if [ "$rc" -eq 0 ]; then
    reachable_from="$ref"
    break
  elif [ "$rc" -ne 1 ]; then
    undecided_refs="${undecided_refs} ${ref}"
  fi
done
if [ -z "$reachable_from" ]; then
  if [ -n "$undecided_refs" ]; then
    echo "refusing: git could not decide whether ${manifest_commit} is reachable from${undecided_refs}, so this check did not run." >&2
    echo "That is a fact about this checkout (a shallow clone, a missing object), not about the manifest. Fetch the full history and try again." >&2
    exit 2
  fi
  echo "refusing: ${MANIFEST} pins ${manifest_commit}, which is not reachable from any rewrite-free ref in this checkout (tried: ${CHECKABLE_FROM:-origin/main origin/release main release})." >&2
  echo "A commit reachable only from a feature branch stops existing the moment that branch is squash merged, and the registry would be left pointing at a build nobody can reproduce (#174, in the registry, where no regeneration can reach it)." >&2
  echo "Publish from a commit whose build is pinned to main or to release." >&2
  exit 2
fi
echo "==> ${manifest_commit} is reachable from ${reachable_from}" >&2

# --- guard 3: never publish a waived build ------------------------------
if grep -q '"unsafe_local_build"[[:space:]]*:[[:space:]]*true' "$MANIFEST"; then
  echo "refusing: ${MANIFEST} is stamped \"unsafe_local_build\": true, so it was generated with every reproducibility guard waived. Publishing it would put an image in a public registry that no checkout can reproduce." >&2
  exit 2
fi

# --- guard 4: a dirty tree is not the release ---------------------------
dirty=$(git status --porcelain -- core apps ui container/Dockerfile)
if [ -n "$dirty" ]; then
  echo "refusing: the working tree is dirty in a path the image is built from, so the pushed image would not be reproducible from ${manifest_commit}:" >&2
  echo "$dirty" >&2
  exit 2
fi

# --- guard 5: no key material on disk, ever -----------------------------
#
# The one hard rule this script has. Keyless signing needs no key at all,
# and a hand-signed release supplies one through the environment; a file
# is neither, and a key file sitting in the working tree is one careless
# `git add` (or one `git add -f`, or one archive of the directory) from
# leaving the machine. Checked against the tree rather than against a
# convention, because the convention is what fails.
#
# Two passes, because one cannot see the whole tree. .gitignore ignores
# *.key, *.pem, *.p12, *.pfx and cosign.key, and --exclude-standard
# suppresses exactly those, so the single --cached --others pass this
# guard used to run went blind to the untracked ignored key that is the
# case it exists for: `cosign generate-key-pair` drops cosign.key in the
# working directory, ignored, invisible, and the guard passed beside it.
# The second pass asks for the ignored files by name and the two are
# unioned.
#
# node_modules and the built frontend bundle are excluded rather than
# scanned. Once ignored files are in scope a vendored test fixture named
# *.pem turns the guard into a false-positive generator, and a guard that
# cries wolf on every release is a guard somebody switches off.
#
# id_rsa and id_ed25519 carry no wildcard, and a git pathspec with no
# wildcard anchors at the repository root, so those two matched only a
# key sitting in the top directory. The product mounts its SSH key at
# /etc/backup-manager/id_ed25519, so a developer generating one for a
# test puts it in a subdirectory, which is precisely where the guard was
# not looking. The */ forms cover the rest of the tree.
key_pathspecs=(
  '*.key' '*.pem' 'cosign.key' '*cosign*.key' '*.p12' '*.pfx'
  'id_rsa' '*/id_rsa' 'id_ed25519' '*/id_ed25519'
  ':(exclude)*node_modules/*' ':(exclude)ui/shared/dist/*'
)
keyfiles=$(
  {
    git ls-files --cached --others --exclude-standard -- "${key_pathspecs[@]}" 2>/dev/null || true
    git ls-files --others --ignored --exclude-standard -- "${key_pathspecs[@]}" 2>/dev/null || true
  } | sed '/^$/d' | sort -u
)
if [ -n "$keyfiles" ]; then
  echo "refusing: private key material is present in the working tree, and this script will not run beside it:" >&2
  echo "$keyfiles" >&2
  echo "Keyless signing needs no key file at all. A hand-signed release passes its key through the environment (cosign --key env://COSIGN_PRIVATE_KEY) and never writes it down." >&2
  echo "Being listed in .gitignore is not an answer: this scan looks at ignored files too, because ignored is where a generated key lands." >&2
  exit 2
fi
if [ -n "${COSIGN_KEY_FILE:-}" ]; then
  echo "refusing: COSIGN_KEY_FILE is set (${COSIGN_KEY_FILE}). This script signs keylessly, or from env://COSIGN_PRIVATE_KEY; it does not read a key off disk." >&2
  exit 2
fi

# --- guard 6: the provenance bundle has to describe this release --------
#
# The SBOM is attested alongside the image, so an out-of-date bundle
# attaches a document describing a different tree to bytes that will
# outlive the correction.
#
# SKIP_PROVENANCE_CHECK=1 exists only so the guard suite can drive the
# five refusals above without a Go toolchain and a real module in every
# fixture. Unlike GUARDS_ONLY, which exits before the first Docker
# command and therefore cannot publish anything, this one REMOVES a
# refusal, so it is only ever safe on a run that cannot publish. Setting
# it on a run that would push is itself a refusal: an attestation is a
# signed claim, and a stale one attached to bytes that outlive the
# correction is worse than no attestation at all.
if [ "${SKIP_PROVENANCE_CHECK:-0}" = "1" ] && [ "${GUARDS_ONLY:-0}" != "1" ]; then
  echo "refusing: SKIP_PROVENANCE_CHECK=1 removes the check that the SBOM about to be attested describes this tree, and this run is not a guards-only run, so it could push and sign." >&2
  echo "It is a test-only seam: set GUARDS_ONLY=1 alongside it, or unset it and regenerate the bundle with (cd distribution && go run ./cmd/provenance -write)." >&2
  exit 2
fi
if [ "${SKIP_PROVENANCE_CHECK:-0}" = "1" ]; then
  echo "==> SKIP_PROVENANCE_CHECK=1: guard 6 is not being run. Only reachable on a GUARDS_ONLY=1 run, which stops before the push." >&2
fi
if [ "${SKIP_PROVENANCE_CHECK:-0}" != "1" ]; then
  if [ ! -r "$SBOM" ]; then
    echo "refusing: ${SBOM} is not in the tree, so there is no SBOM to attest. Generate it with: (cd distribution && go run ./cmd/provenance -write)" >&2
    exit 2
  fi
  if ! (cd distribution && GOWORK=off go run ./cmd/provenance >/dev/null 2>&1); then
    echo "refusing: the compliance artifacts in provenance/ are not what this tree generates, so the SBOM about to be attested to a published image describes a different tree." >&2
    echo "Regenerate them with: (cd distribution && go run ./cmd/provenance -write)" >&2
    exit 2
  fi
fi

# GUARDS_ONLY=1 stops here, after every refusal and before the first
# Docker command, so the guards can be driven in a test. Nothing sets it
# on a real run and it is read nowhere else.
if [ "${GUARDS_ONLY:-0}" = "1" ]; then
  echo "==> GUARDS_ONLY=1: every guard passed; stopping before the push. Would publish ${REFERENCE} for ${PLATFORMS}" >&2
  exit 0
fi

echo "==> Publishing ${REFERENCE} (${PLATFORMS}) from ${manifest_commit}" >&2
if [ "$DRY_RUN" = "1" ]; then
  echo "==> DRY_RUN=1: stopping before docker buildx build --push" >&2
  exit 0
fi

# The bytes half of the old guard 2 (#260), and the last thing that
# happens before anything leaves this machine. `docker buildx build
# --push` publishes in the same command that builds, so there is no
# after: the image is rebuilt here, its two binaries are extracted and
# hashed, and a manifest that does not describe them stops the release.
# The push below is a cache hit on this build, not a second one.
bash scripts/release/verify-manifest-parity.sh >&2

VERSION="${VERSION:-$(json_string "$MANIFEST" version)}"
docker buildx build \
  --platform "$PLATFORMS" \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "COMMIT=${manifest_commit}" \
  -f container/Dockerfile \
  -t "$REFERENCE" \
  --push \
  . >&2

# Read the digests back out of the registry rather than off the push's
# own output. The push says what it believes it sent; the registry says
# what it holds, and the manifest is a claim about the registry.
echo "==> Reading digests back from the registry" >&2
index_digest=$(docker buildx imagetools inspect "$REFERENCE" --format '{{.Manifest.Digest}}')
echo "==> index digest: ${index_digest}" >&2

if [ "$SIGN" = "1" ]; then
  if ! command -v cosign >/dev/null 2>&1; then
    echo "refusing: SIGN=1 and cosign is not on PATH. Install it, or run with SIGN=0 and record signing.status as unsigned." >&2
    exit 2
  fi
  # Keyless. No --key, no key file, nothing written: the OIDC identity of
  # whoever runs this is what the signature binds to.
  cosign sign --yes "${REFERENCE}@${index_digest}" >&2
  cosign attest --yes --type spdxjson --predicate "$SBOM" "${REFERENCE}@${index_digest}" >&2
fi

echo
echo "Record these, and only these, in ${MANIFEST}:"
docker buildx imagetools inspect "$REFERENCE" --raw \
  | sed -n 's/.*"digest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
  | while read -r d; do echo "  registry_digest candidate: ${d}"; done
echo
echo "Then set image.published to true in ${CANONICAL}, regenerate the provenance bundle"
echo "((cd distribution && go run ./cmd/provenance -write)), and run the gate."
