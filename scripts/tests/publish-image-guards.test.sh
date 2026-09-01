#!/usr/bin/env bash
# An automated control for the refusals in
# scripts/release/publish-image.sh. Issue #88 (B5.2).
#
# The script it drives is the one step in this repository that does
# something irreversible: it puts bytes in a public registry under a
# semantic version. Every other release artifact can be regenerated after
# the fact, and a bad registry tag cannot. So its refusals get a control
# for the same reason #174's generator got one in
# record-release-hashes-guards.test.sh, only more so: this one runs once,
# on the day it matters, and nobody will be watching it work for the first
# time.
#
# Structure copied from that file on purpose. Throwaway `git init`
# repository per refusal, the real script, the GUARDS_ONLY=1 seam that
# stops immediately after the guard block and before the first Docker
# command, and an assertion on the exit code AND the distinct message.
# Every refusal exits 2, so an exit-code-only assertion cannot tell "the
# tree is dirty" from "there is a private key sitting in it", and those
# call for very different reactions.
#
# Positive controls, because every assertion here is a negative one:
#
#   * a clean checkout whose HEAD is the commit the manifest records
#     reaches the seam, or these tests would pass equally against a script
#     that refuses everything;
#   * guard 6's two arms are driven through the same `go` stub at exit 0
#     and exit 1, so a stub that was not on PATH, or a script that ignored
#     the exit status, fails the pair;
#   * the workflow scanner at the end is run against a file carrying the
#     shape it hunts, so its silence on the real workflow is evidence.
#
# The last section leaves the script and checks
# .github/workflows/release.yml, which is the other half of the same
# release path and the only other place a refusal on this path can be
# written wrong.
set -uo pipefail

unset GIT_INDEX_FILE GIT_DIR GIT_WORK_TREE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR GIT_PREFIX

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="${REPO_ROOT}/scripts/release/publish-image.sh"

failures=0
current=""
tmpdirs=()

cleanup() {
  for d in ${tmpdirs+"${tmpdirs[@]}"}; do
    [ -n "$d" ] && rm -rf "$d"
  done
}
trap cleanup EXIT

fail() {
  failures=$((failures + 1))
  echo "FAIL: ${current}: $1" >&2
  if [ -n "${2:-}" ]; then
    echo "--- script output ---" >&2
    echo "$2" >&2
    echo "---------------------" >&2
  fi
}

# new_repo prints the path of a throwaway repository shaped like this one
# from the script's point of view: the two JSON files it reads, the paths
# its dirty check looks at, and a committed HEAD.
#
# manifest_commit is written AFTER the commit, so it can name the real
# HEAD. That leaves container/release-manifest.json modified, which is
# deliberate and harmless: the dirty guard looks at core, apps, ui and
# container/Dockerfile, and the manifest is none of them. If that ever
# changes, the positive control below stops reaching the seam and says so.
new_repo() {
  local dir
  dir="$(mktemp -d)"
  tmpdirs+=("$dir")
  git -C "$dir" init -q -b main
  git -C "$dir" config user.email t@example.invalid
  git -C "$dir" config user.name t
  # The repository's real .gitignore, not a convenient subset of it.
  # Guard 5 asks git which paths exist, and git's answer depends on the
  # exclusion configuration, so a fixture with no .gitignore is a fixture
  # missing the exact property that decides the guard. That is how the
  # untracked arm below passed while the shipped configuration made the
  # scan blind: .gitignore ignores *.key, and the scan was suppressing
  # ignored files. Every future guard is exercised against the shipped
  # exclusions because of this line.
  cp "${REPO_ROOT}/.gitignore" "$dir/.gitignore"
  mkdir -p "$dir/core" "$dir/distribution/packaging" "$dir/ui" "$dir/container" "$dir/provenance"
  printf 'package main\n' >"$dir/core/main.go"
  printf 'ui\n' >"$dir/ui/marker"
  printf 'FROM scratch\n' >"$dir/container/Dockerfile"
  cat >"$dir/distribution/packaging/canonical.json" <<'JSON'
{ "image": { "reference": "ghcr.io/spdrman/backup-manager:1.0.0", "published": false } }
JSON
  printf '{ "version": "test", "commit": "0000000000000000000000000000000000000000" }\n' \
    >"$dir/container/release-manifest.json"
  printf '{}\n' >"$dir/provenance/sbom.spdx.json"
  git -C "$dir" add -A
  git -C "$dir" commit -qm base
  echo "$dir"
}

# pin_manifest_to_head rewrites the manifest so its commit is this
# repository's HEAD, which is what guard 2 requires.
pin_manifest_to_head() {
  local dir="$1"
  printf '{ "version": "test", "commit": "%s" }\n' "$(git -C "$dir" rev-parse HEAD)" \
    >"$dir/container/release-manifest.json"
}

# stub_go writes a `go` onto PATH that exits with a fixed code, so guard
# 6's "the provenance bundle is stale" branch can be driven both ways
# without a Go toolchain or a real module in the fixture.
stub_go() {
  local dir="$1" code="$2" bin
  bin="${dir}/.stubbin"
  mkdir -p "$bin"
  cat >"${bin}/go" <<EOF
#!/usr/bin/env bash
exit ${code}
EOF
  chmod +x "${bin}/go"
  echo "$bin"
}

run_guards() {
  local dir="$1"
  shift
  local out rc
  out="$(cd "$dir" && env GUARDS_ONLY=1 "$@" bash "$SCRIPT" 2>&1)"
  rc=$?
  echo "$rc"
  echo "$out"
}

# run_publish_path drives the script WITHOUT the GUARDS_ONLY seam, which
# is the only way to observe what happens after the guard block. DRY_RUN=1
# is the belt: it stops before `docker buildx build --push`, so if a
# refusal under test ever stops refusing, this suite fails instead of
# pushing something out of a throwaway repository.
run_publish_path() {
  local dir="$1"
  shift
  local out rc
  out="$(cd "$dir" && env DRY_RUN=1 SIGN=0 "$@" bash "$SCRIPT" 2>&1)"
  rc=$?
  echo "$rc"
  echo "$out"
}

expect() {
  local rc="$1" out="$2" want_rc="$3" want="$4"
  if [ "$rc" != "$want_rc" ]; then
    fail "exit ${rc}, want ${want_rc}" "$out"
    return
  fi
  case "$out" in
    *"$want"*) ;;
    *) fail "output does not contain: ${want}" "$out" ;;
  esac
}

refute() {
  local out="$1" unwanted="$2"
  case "$out" in
    *"$unwanted"*) fail "output contains, and must not: ${unwanted}" "$out" ;;
  esac
}

split_rc() { echo "$1" | head -n1; }
split_out() { echo "$1" | tail -n +2; }

# --- positive control, first, so the refusals below mean something ------
current="a clean checkout at the commit the manifest records reaches the seam"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "every guard passed"
expect "$rc" "$out" 0 "Would publish ghcr.io/spdrman/backup-manager:1.0.0"

# --- guard 1: the files it reads are not there --------------------------
current="canonical.json missing"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
rm "$repo/distribution/packaging/canonical.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "distribution/packaging/canonical.json is not readable"

current="canonical.json present but carrying no reference"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf '{ "image": { "published": false } }\n' >"$repo/distribution/packaging/canonical.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "no image reference in"

# --- guard 2: the manifest has to describe a build this history has -----
#
# The refusals here replaced a single equality check (manifest commit ==
# HEAD) that no tree could satisfy, which is #260. Four arms now, because
# the four things that can be wrong call for four different reactions: the
# commit is not in this repository at all, it is in the repository but not
# in this history, it is in this history but only on a branch that a
# squash merge will rewrite (#174), or git could not answer.
current="the manifest pins a SHA that is not an object in this repository"
repo="$(new_repo)" # the fixture's manifest still pins all-zeroes
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "which is not a commit in this repository"

current="the manifest pins a real commit that is not an ancestor of HEAD"
repo="$(new_repo)"
git -C "$repo" checkout -q -b sidebranch
printf 'package main // side\n' >"$repo/core/main.go"
git -C "$repo" commit -qam "side"
side="$(git -C "$repo" rev-parse HEAD)"
git -C "$repo" checkout -q main
printf '{ "version": "test", "commit": "%s" }\n' "$side" >"$repo/container/release-manifest.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "which is not an ancestor of HEAD"

# #174 in the registry. The commit is perfectly reachable from HEAD, so
# the arm above passes it; what makes it unpublishable is that HEAD is a
# feature branch, and a squash merge rewrites the commit the registry tag
# would then be the only record of.
current="the manifest pins a commit only a feature branch has"
repo="$(new_repo)"
git -C "$repo" checkout -q -b feature
printf 'package main // feature\n' >"$repo/core/main.go"
git -C "$repo" commit -qam "feature work"
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "not reachable from any rewrite-free ref"
expect "$rc" "$out" 2 "#174"

# origin/release is a rewrite-free ref in its own right, and it has to
# answer on its own: a release cut carries the pipeline change that
# publishes it, so the first cut is pinned to release rather than to
# main. The local main is renamed away so nothing else can answer and the
# pass is attributable.
current="a commit reachable only from origin/release is publishable, and the ref is named"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
git -C "$repo" update-ref refs/remotes/origin/release "$(git -C "$repo" rev-parse HEAD)"
git -C "$repo" branch -m main cut
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "is reachable from origin/release"

current="CHECKABLE_FROM names the rewrite-free ref for a fork"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
git -C "$repo" branch -m main stable
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1 CHECKABLE_FROM=stable)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "is reachable from stable"

current="a manifest that pins no commit at all"
repo="$(new_repo)"
printf '{ "version": "test" }\n' >"$repo/container/release-manifest.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "pins no commit"
refute "$out" "not an ancestor of HEAD"

# --- guard 3: a waived manifest must never be published -----------------
current="a manifest stamped unsafe_local_build"
repo="$(new_repo)"
printf '{ "unsafe_local_build": true, "version": "test", "commit": "%s" }\n' \
  "$(git -C "$repo" rev-parse HEAD)" >"$repo/container/release-manifest.json"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "unsafe_local_build"
expect "$rc" "$out" 2 "public registry"

# --- guard 4: a dirty tree is not the release ---------------------------
current="a working tree dirty in a path the image is built from"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf 'uncommitted\n' >>"$repo/core/main.go"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "the working tree is dirty"

# --- guard 5: key material on disk --------------------------------------
#
# The fixture carries the real .gitignore, so the untracked key below is
# also an IGNORED key, which is the case that happens by accident:
# `cosign generate-key-pair` writes cosign.key into the working directory
# and .gitignore covers it. The check-ignore assertion before the run
# proves the fixture really has that property, so this arm cannot go back
# to passing because the fixture stopped reproducing what is deployed.
current="an untracked, gitignored private key beside the script"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf 'not a real key\n' >"$repo/cosign.key"
if ! git -C "$repo" check-ignore -q cosign.key; then
  fail "the fixture does not ignore cosign.key, so this arm is not testing the shipped configuration" ""
fi
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "private key material is present in the working tree"
expect "$rc" "$out" 2 "cosign.key"

current="a gitignored key in a subdirectory, which no unwildcarded pathspec reaches"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
mkdir -p "$repo/secrets"
printf 'not a real key\n' >"$repo/secrets/id_ed25519"
printf 'not a real key\n' >"$repo/secrets/release.pem"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "private key material is present in the working tree"
expect "$rc" "$out" 2 "secrets/id_ed25519"
expect "$rc" "$out" 2 "secrets/release.pem"

# The scoping half. Once ignored files are in scope, node_modules and the
# built bundle are full of vendored *.pem test fixtures, and a guard that
# refuses every release over one of those is a guard somebody disables.
# The two arms above are this one's positive control: they prove the scan
# looks at ignored files at all, so a pass here is the exclusion working
# rather than the whole scan being dead again.
current="a vendored .pem under node_modules is not treated as key material"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
mkdir -p "$repo/node_modules/some-pkg/fixtures" "$repo/ui/shared/dist"
printf 'not a real key\n' >"$repo/node_modules/some-pkg/fixtures/test-cert.pem"
printf 'not a real key\n' >"$repo/ui/shared/dist/inlined.pem"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "every guard passed"
refute "$out" "private key material is present in the working tree"

# -f, because the fixture now carries the real .gitignore and that ignores
# *.pem. Committing it anyway is the point of this arm: the outer net does
# not stop `git add -f`, a merge, or a rewritten history, so the tracked
# case has to be refused on its own and not as a side effect of the
# ignored scan finding the same file.
current="a tracked .pem is refused too, not only an untracked .key"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
printf 'not a real key\n' >"$repo/release-signing.pem"
git -C "$repo" add -f release-signing.pem
git -C "$repo" commit -qm "oops" >/dev/null
if [ -z "$(git -C "$repo" ls-files -- release-signing.pem)" ]; then
  fail "the fixture never tracked release-signing.pem, so this arm is testing the untracked path again" ""
fi
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "private key material is present in the working tree"
expect "$rc" "$out" 2 "release-signing.pem"

current="COSIGN_KEY_FILE is refused even with no key on disk"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
r="$(run_guards "$repo" SKIP_PROVENANCE_CHECK=1 COSIGN_KEY_FILE=/tmp/nope.key)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "COSIGN_KEY_FILE is set"
refute "$out" "present in the working tree"

# --- guard 6: the SBOM has to describe this tree ------------------------
current="no SBOM in the tree at all"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
rm "$repo/provenance/sbom.spdx.json"
r="$(run_guards "$repo")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "is not in the tree, so there is no SBOM to attest"

current="the provenance check failing is a refusal"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
stub="$(stub_go "$repo" 1)"
r="$(run_guards "$repo" "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "are not what this tree generates"

current="the same stub at exit 0 reaches the seam (control for the pair above)"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
stub="$(stub_go "$repo" 0)"
r="$(run_guards "$repo" "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "every guard passed"
refute "$out" "are not what this tree generates"

# --- the SKIP_PROVENANCE_CHECK seam is test-only ------------------------
#
# Every arm above sets SKIP_PROVENANCE_CHECK=1 so the fixtures need no Go
# toolchain and no real module. That variable removes a refusal, and the
# refusal it removes is the one whose failure is permanent and public: an
# SBOM attested to published bytes describing a tree that is not the one
# published, and a signed claim cannot be regenerated the way a file can.
# So the seam has to be unusable on a run that could push, and that is
# what these two arms measure. They are the only arms in this file that go
# past the guard block.
current="SKIP_PROVENANCE_CHECK on a run that could publish is itself a refusal"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
r="$(run_publish_path "$repo" SKIP_PROVENANCE_CHECK=1)"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 2 "SKIP_PROVENANCE_CHECK=1 removes the check"
expect "$rc" "$out" 2 "test-only seam"
refute "$out" "stopping before docker buildx build"

current="the same run without it reaches the push (control for the refusal above)"
repo="$(new_repo)"
pin_manifest_to_head "$repo"
stub="$(stub_go "$repo" 0)"
r="$(run_publish_path "$repo" "PATH=${stub}:${PATH}")"
rc="$(split_rc "$r")"; out="$(split_out "$r")"
expect "$rc" "$out" 0 "stopping before docker buildx build"
refute "$out" "SKIP_PROVENANCE_CHECK=1 removes the check"

# --- the workflow that drives this script -------------------------------
#
# .github/workflows/release.yml is the other half of the release path, and
# its publish job holds id-token: write (the Sigstore identity Fulcio
# certifies) and packages: write. GitHub expands ${{ }} textually into a
# run: script before bash ever parses it, so a dispatch input interpolated
# there is shell source rather than data, in the job that mints the
# signing identity. The confirmation step is the worst place for it: its
# whole purpose is to make an irreversible publish deliberate, and it is
# reached with exactly the values that are not the expected tag.
#
# Inputs belong in env:, where the value stays data. This asserts nothing
# expands inside a run: body at all, which is the rule rather than the one
# instance.
WORKFLOW="${REPO_ROOT}/.github/workflows/release.yml"

# expansions_in_run_blocks prints every "line: text" inside a `run:` body
# that carries a ${{ }} expansion. Comment lines do not count; they are
# not shell source.
expansions_in_run_blocks() {
  awk '
    /^[[:space:]]*#/ { next }
    {
      indent = match($0, /[^[:space:]]/) - 1
      if (in_run && $0 ~ /[^[:space:]]/ && indent <= run_indent) { in_run = 0 }
      if (in_run && index($0, "${{") > 0) { printf "%d: %s\n", NR, $0 }
      if (!in_run && $0 ~ /^[[:space:]]*run:[[:space:]]*[|>]/) {
        in_run = 1
        run_indent = indent
      }
    }
  ' "$1"
}

current="release.yml never interpolates a dispatch input into a run: body"
found="$(expansions_in_run_blocks "$WORKFLOW")"
if [ -n "$found" ]; then
  fail "an expression is expanded into shell source in the release workflow" "$found"
fi

# Positive control: the assertion above is a negative one, so prove the
# scanner can see the shape it hunts. This is the exact text release.yml
# carried before the input was moved into env:.
current="the scanner finds the shape it hunts (control for the arm above)"
ctl="$(mktemp -d)"; tmpdirs+=("$ctl")
cat >"${ctl}/bad.yml" <<'YAML'
jobs:
  publish:
    steps:
      - name: Refuse an unconfirmed publish
        run: |
          tag="1.0.0"
          if [ "${{ inputs.confirm }}" != "$tag" ]; then
            exit 1
          fi
      - name: After
        uses: actions/checkout@v7
YAML
if [ -z "$(expansions_in_run_blocks "${ctl}/bad.yml")" ]; then
  fail "the scanner does not flag an input interpolated straight into a run: body, so its silence on the real workflow means nothing" ""
fi

if [ "$failures" -gt 0 ]; then
  echo "publish-image guards: ${failures} failing assertion(s)" >&2
  exit 1
fi
echo "publish-image guards: ok"
