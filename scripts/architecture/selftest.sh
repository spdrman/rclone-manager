#!/usr/bin/env bash
# Positive controls for the architecture checks (issue #165).
#
# Every check in this directory is a negative assertion: "core imports
# nothing from distribution", "no platform file declares retention", "no
# tracked file is unclassified". A negative assertion that has never been
# seen to fail is indistinguishable from one that cannot fail, and this
# repository has already been bitten by exactly that: a scanner that looked
# correct silently missed `ADMIN_PASSWORD`, because `\b` never matches
# between `_` and `p`.
#
# So each rule here is mutation-tested against the REAL tree rather than
# against a synthetic fixture. A copy of the working tree gets one
# deliberate violation planted in a real package, the check runs, and it
# must fail AND name the rule. Then the copy is discarded. A check that
# passes a planted violation is reported as a self-test failure, which is
# the whole point.
#
# It is fast: no npm install, no Docker, no worktree of its own beyond a
# file copy, because every mutation targets a check that is static.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

root=$(pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rclone-manager-arch-selftest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# mutant <name> copies the working tree into $tmp/<name> and echoes its
# path.
#
# --cached --others --exclude-standard, not plain `git ls-files`: the copy
# has to include files that are present but not yet committed, because the
# checks being tested are themselves often uncommitted while they are being
# written. Copying tracked files only produced a self-test that silently
# "caught" every mutation, since the check script it invoked did not exist
# in the copy at all. --exclude-standard keeps node_modules and build output
# out, so the copy stays quick.
mutant() {
  local name=$1
  local dir="$tmp/$name"
  mkdir -p "$dir"
  (cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | (cd "$dir" && tar -xf -)
  # The checks call `git rev-parse --show-toplevel`, so the copy needs to
  # be a repository. An empty one with a single commit is enough: nothing
  # here builds a worktree of HEAD.
  git -C "$dir" init -q
  git -C "$dir" add -A
  git -C "$dir" -c user.email=selftest@example.invalid -c user.name=selftest commit -q -m "selftest baseline"
  printf '%s' "$dir"
}

# expect_check_fails <label> <dir> <expected-substring> <script> [args...]
#
# The expected substring is not decoration. Without it a mutation "passes"
# whenever the check fails for ANY reason, including the check script being
# absent from the copy or erroring before it ever looked at the mutation.
# That exact failure mode produced a green self-test here once, so the
# message the check prints is now part of what is asserted.
expect_check_fails() {
  local label=$1 dir=$2 expect=$3; shift 3
  if (cd "$dir" && "$@") >"$tmp/out" 2>&1; then
    echo "SELFTEST FAIL: $label — the check PASSED against a planted violation." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  elif ! grep -qF "$expect" "$tmp/out"; then
    echo "SELFTEST FAIL: $label — the check failed, but not for the planted reason." >&2
    echo "    expected its output to contain: $expect" >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  else
    echo "  ok (caught): $label"
    pass=$((pass + 1))
  fi
}

# expect_check_passes <label> <dir> <script> [args...]
expect_check_passes() {
  local label=$1 dir=$2; shift 2
  if (cd "$dir" && "$@") >"$tmp/out" 2>&1; then
    echo "  ok (clean):  $label"
    pass=$((pass + 1))
  else
    echo "SELFTEST FAIL: $label — the check FAILED against an unmutated tree, so its failures mean nothing." >&2
    sed 's/^/    /' "$tmp/out" >&2
    fail=$((fail + 1))
  fi
}

echo "==> negative controls: every static check is clean on the real tree"
expect_check_passes "check-layer-manifest" "$root" ./scripts/architecture/check-layer-manifest.sh
expect_check_passes "check-layer-ownership" "$root" ./scripts/architecture/check-layer-ownership.sh
expect_check_passes "check-core-dependency-rule" "$root" ./scripts/architecture/check-core-dependency-rule.sh

echo
echo "==> layer-manifest completeness"

d=$(mutant manifest-unclassified)
mkdir -p "$d/apps/casaos/frontend"
cat > "$d/apps/casaos/frontend/platform.ts" <<'EOF'
export const casaosBridge = { id: "casaos" };
EOF
git -C "$d" add -A
expect_check_fails "a new provider directory nobody classified" "$d" "apps/casaos/frontend/platform.ts" ./scripts/architecture/check-layer-manifest.sh

d=$(mutant manifest-stale)
printf '\ncore            -           a/path/that/does/not/exist\n' >> "$d/scripts/architecture/layers.conf"
expect_check_fails "a manifest entry pointing at a path that does not exist" "$d" "does not exist" ./scripts/architecture/check-layer-manifest.sh

d=$(mutant manifest-badkind)
sed -i.bak 's|^distribution    adapter     apps/unraid/template$|distribution    -           apps/unraid/template|' "$d/scripts/architecture/layers.conf"
rm -f "$d/scripts/architecture/layers.conf.bak"
expect_check_fails "a distribution entry with no adapter/canonical kind" "$d" "must be \"adapter\" or \"canonical\"" ./scripts/architecture/check-layer-manifest.sh

d=$(mutant manifest-noadapters)
sed -i.bak 's|^distribution    adapter |distribution    canonical |' "$d/scripts/architecture/layers.conf"
rm -f "$d/scripts/architecture/layers.conf.bak"
expect_check_fails "a manifest with no adapter paths at all, which would make the deletion proof vacuous" "$d" "would delete nothing and pass vacuously" ./scripts/architecture/check-layer-manifest.sh

echo
echo "==> dependency direction"

d=$(mutant dep-core-to-distribution)
mkdir -p "$d/apps/common/webhost"
cat > "$d/apps/common/webhost/selftest_reverse_import.go" <<'EOF'
package webhost

import _ "github.com/spdrman/rclone-manager/distribution/packaging"
EOF
# The reverse import has to resolve, so the mutant module gets a replace
# pointing at its own sibling copy. Without it `go list` fails for a
# dependency-resolution reason and the check would "catch" the wrong thing.
(
  cd "$d/apps/common"
  GOWORK=off go mod edit -require=github.com/spdrman/rclone-manager/distribution@v0.0.0
  GOWORK=off go mod edit -replace=github.com/spdrman/rclone-manager/distribution=../../distribution
)
expect_check_fails "a core-layer package importing distribution" "$d" "core/app ─X─► distribution" ./scripts/architecture/check-core-dependency-rule.sh

d=$(mutant dep-platform-to-distribution)
mkdir -p "$d/apps/generic/platform"
cat > "$d/apps/generic/platform/selftest_reverse_import.go" <<'EOF'
package platform

import _ "github.com/spdrman/rclone-manager/distribution/packaging"
EOF
(
  cd "$d/apps/generic"
  GOWORK=off go mod edit -require=github.com/spdrman/rclone-manager/distribution@v0.0.0
  GOWORK=off go mod edit -replace=github.com/spdrman/rclone-manager/distribution=../../distribution
)
expect_check_fails "a platform-layer package importing distribution" "$d" "platform/app ─X─► distribution" ./scripts/architecture/check-core-dependency-rule.sh

d=$(mutant dep-core-to-provider-sdk)
mkdir -p "$d/core/internal/lifecycle"
cat > "$d/core/internal/lifecycle/selftest_sdk_import.go" <<'EOF'
package lifecycle

import _ "github.com/truenas/api-client-golang/truenas"
EOF
(
  cd "$d/core"
  GOWORK=off go mod edit -require=github.com/truenas/api-client-golang@v0.0.0
  GOWORK=off go mod edit -replace=github.com/truenas/api-client-golang=./internal/selftest-fake-sdk
)
mkdir -p "$d/core/internal/selftest-fake-sdk/truenas"
cat > "$d/core/internal/selftest-fake-sdk/go.mod" <<'EOF'
module github.com/truenas/api-client-golang

go 1.27.0
EOF
cat > "$d/core/internal/selftest-fake-sdk/truenas/client.go" <<'EOF'
package truenas

// Client stands in for a real NAS vendor SDK, so the provider-SDK rule can
// be shown to fire against an import that actually resolves.
type Client struct{}
EOF
expect_check_fails "core importing a NAS vendor SDK" "$d" "─X─► NAS SDKs" ./scripts/architecture/check-core-dependency-rule.sh

echo
echo "==> layer ownership, one mutation per rule"

# One planted declaration per rule, in a REAL platform package and a REAL
# distribution package alternately, so no rule is proven only in one layer.
plant_ownership() {
  local label=$1 rule=$2 target=$3 pkg=$4 decl=$5
  local d
  d=$(mutant "own-$label")
  cat > "$d/$target" <<EOF
package $pkg

$decl
EOF
  expect_check_fails "$label" "$d" "$rule" ./scripts/architecture/check-layer-ownership.sh
}

plant_ownership "lifecycle-state declared in a runtime profile" 'violates rule "lifecycle-state"' \
  "apps/generic/platform/selftest_owns.go" "platform" \
  "// LifecycleState is planted by the self-test.
type LifecycleState int"

plant_ownership "retention-policy declared in a runtime profile" 'violates rule "retention-policy"' \
  "apps/generic/platform/selftest_owns.go" "platform" \
  "// ApplyRetentionPlan is planted by the self-test. Note the camel case:
// a word-boundary rule would never fire between \"Apply\" and \"Retention\".
func ApplyRetentionPlan() {}"

plant_ownership "validation-rules declared in a distribution package" 'violates rule "validation-rules"' \
  "distribution/packaging/selftest_owns.go" "packaging" \
  "// ValidatorCatalog is planted by the self-test.
var ValidatorCatalog = map[string]string{}"

plant_ownership "catalog-truth declared in a distribution package" 'violates rule "catalog-truth"' \
  "distribution/packaging/selftest_owns.go" "packaging" \
  "// RebuildCatalog is planted by the self-test.
func RebuildCatalog() {}"

plant_ownership "backup-policy declared in a runtime profile" 'violates rule "backup-policy"' \
  "apps/generic/platform/selftest_owns.go" "platform" \
  "// BackupPolicy is planted by the self-test.
type BackupPolicy struct{}"

# TypeScript side: the bridges are where a runtime profile would most
# plausibly grow a second opinion about retention.
d=$(mutant own-ts)
cat >> "$d/apps/truenas/frontend/platform.ts" <<'EOF'

// Planted by the self-test.
export interface RetentionPolicy { keep: number }
EOF
expect_check_fails "retention-policy declared in a provider bridge (TypeScript)" "$d" "violates rule \"retention-policy\"" ./scripts/architecture/check-layer-ownership.sh

# And the control the TypeScript scanner most needs: a mention inside a
# comment is NOT a declaration, and must not fire.
d=$(mutant own-ts-comment)
cat >> "$d/apps/truenas/frontend/platform.ts" <<'EOF'

// This bridge never defines a RetentionPolicy; core owns that.
// export interface RetentionPolicy { keep: number }
EOF
expect_check_passes "a commented-out retention declaration in a bridge (must NOT fire)" "$d" ./scripts/architecture/check-layer-ownership.sh

echo
echo "==> shared UI provider-SDK import scan"

# Only the fast static half is mutated here. The deletion half needs an
# npm ci in a fresh worktree, which is minutes rather than seconds, and it
# is the scan that carries the four platforms with no directory to delete.
d=$(mutant ui-relative-import)
cat >> "$d/ui/shared/src/types/platform.ts" <<'EOF'

// Planted by the self-test.
export { ugosBridge } from "../../../apps/ugos/frontend/platform";
EOF
expect_check_fails "the shared UI reaching into a provider directory" "$d" \
  "a relative reach into a provider directory" ./scripts/architecture/check-ui-shared-provider-imports.sh

d=$(mutant ui-bare-sdk)
cat >> "$d/ui/shared/src/types/platform.ts" <<'EOF'

// Planted by the self-test: a platform with no directory to delete, so
// only the scan can catch it.
export { probe } from "@casaos/app-store-sdk";
EOF
expect_check_fails "the shared UI importing a bare provider SDK package" "$d" \
  "a provider SDK package" ./scripts/architecture/check-ui-shared-provider-imports.sh

# The control the scan most needs: ui/shared names every platform in its
# PlatformId union and in prose, and none of that is an import.
d=$(mutant ui-prose-mention)
cat >> "$d/ui/shared/src/types/platform.ts" <<'EOF'

// Planted by the self-test: prose and a type, never an import. CasaOS,
// ZimaOS, Dockge and Portainer are named here on purpose.
export type SelftestPlatformId = "casaos" | "zimaos" | "dockge" | "portainer";
EOF
expect_check_passes "platform names in prose and in a type union (must NOT fire)" "$d" \
  ./scripts/architecture/check-ui-shared-provider-imports.sh

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "FAIL: $fail of $((pass + fail)) architecture controls did not behave as required." >&2
  exit 1
fi
echo
echo "OK: all $pass architecture controls behaved as required (every rule was shown to fire against a real planted violation, and shown not to fire on the real tree)."
