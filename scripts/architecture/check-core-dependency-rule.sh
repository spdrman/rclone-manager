#!/usr/bin/env bash
# The dependency-direction check (issue #106 WP1.1, extended to three
# layers by issue #165).
#
# EPIC-B WP1.1 asked for "a dependency-rule check (import-graph test via
# `go list` or a lint rule) asserting core/ has zero imports from apps/ or
# any provider SDK" (docs/EPIC-B-multi-nas.md §69 WP1.1, §7.1). Phase 6
# widens it, because EPIC B #81's standing constraint names a direction
# with three layers in it, not two:
#
#   core/app  ─X─► distribution/*
#   core/app  ─X─► NAS SDKs
#   httpapi   ───► app/core
#   web       ───► versioned API contract
#   platform  ───► app/core contracts only
#   adapters  ───► canonical image/Compose/runtime metadata
#
# So this script now checks every Go module in the repository against the
# layer it is declared to be in (scripts/architecture/layers.conf):
#
#   a core-layer module      may import neither platform nor distribution
#   a platform-layer module  may not import distribution
#   any core or platform module may not import a NAS/provider SDK
#
# This is the fast, static safety net: it inspects the resolved import
# graph in place, on every run, with no filesystem mutation.
# verify-core-without-apps.sh and verify-core-without-distribution.sh are
# the heavier, literal proofs of the same claims, by actually deleting the
# trees in a throwaway worktree.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

if [ ! -d core ]; then
  echo "FAIL: core/ module does not exist yet (dependency rule is inapplicable)." >&2
  exit 1
fi

readonly MODULE_PREFIX="github.com/spdrman/rclone-manager/"

# providerSDKPattern is the set of third-party NAS, container-manager and
# app-store SDK names no core- or platform-layer module may pull in. The
# full Phase 6 platform list, not just the ones with a directory today:
# the point of a negative check is to already be in place when someone
# reaches for the SDK, and adding the name after the import lands is too
# late to prevent anything.
#
# Matched case-insensitively as a substring of a dependency's import path,
# and only on paths OUTSIDE this repository's own module prefix: our own
# apps/truenas is caught by the layer rules below, and matching it here
# too would report one violation twice with two different explanations.
readonly PROVIDER_SDK_PATTERN='ugreen|ugos|truenas|ixsystems|unraid|synology|openmediavault|omv-|proxmox|portainer|casaos|zimaos|dockge'

fail=0
note() { echo "$@" >&2; fail=1; }

# forbidden_layers_for <layer> prints the layers a module in <layer> may
# not import from.
forbidden_layers_for() {
  case "$1" in
    core)         printf '%s\n' platform distribution ;;
    platform)     printf '%s\n' distribution ;;
    distribution) ;;
    *)            ;;
  esac
}

# check_module <module-dir> <layer>
check_module() {
  local dir=$1 layer=$2
  local output

  # GOWORK=off: resolve each module's import graph against its own go.mod
  # only, never against the repo-root go.work (which lists sibling modules
  # for local development convenience). A module standing alone is the
  # claim.
  #
  # `go list`'s own exit status is checked BEFORE its output is inspected.
  # This check's entire job is to fail loudly on a boundary violation, and
  # `... | grep ... || true` alone fails OPEN on the worst case: `go list`
  # erroring for a reason that has nothing to do with layering (the module
  # does not even build, say) produces error text that just does not match
  # the grep, and `|| true` swallows the pipeline's real exit code and
  # prints "OK." regardless. That happened here once already.
  if ! output=$(cd "$dir" && GOWORK=off go list -deps ./... 2>&1); then
    note "FAIL: $dir (GOWORK=off) go list -deps ./... itself failed:"
    echo "$output" >&2
    return
  fi

  local forbidden
  forbidden=$(forbidden_layers_for "$layer")

  local pkg rel dep_layer
  while IFS= read -r pkg; do
    [ -n "$pkg" ] || continue

    # node_modules is not repository code: nothing in it is tracked, and
    # the layer manifest classifies tracked files. It shows up here at all
    # only because `go list ./...` walks into it, and one npm package
    # (flatted) happens to ship a Go implementation inside a directory that
    # a Go module pattern therefore matches. Skipping it keeps the check
    # from reporting a violation that exists only on a developer machine
    # that has run npm ci, and never on CI.
    case "$pkg" in
      */node_modules/*) continue ;;
    esac

    case "$pkg" in
      "$MODULE_PREFIX"*)
        # An in-repository dependency: classify it and compare layers.
        rel=${pkg#"$MODULE_PREFIX"}
        dep_layer=$(arch::classify "$rel" | awk '{print $1}') || dep_layer=""
        if [ -z "$dep_layer" ]; then
          note "FAIL: $dir imports $pkg, which scripts/architecture/layers.conf classifies into no layer."
          continue
        fi
        if [ -n "$forbidden" ] && printf '%s\n' "$forbidden" | grep -qx "$dep_layer"; then
          note "FAIL: $dir is in the $layer layer and imports $pkg, which is in the $dep_layer layer."
          note "      rule violated: ${layer}/app ─X─► ${dep_layer} (EPIC B #81, \"Dependency rule\")"
        fi
        ;;
      *)
        # A third-party dependency: the only rule that applies is the
        # provider-SDK ban, and only for core and platform modules.
        if [ "$layer" = "core" ] || [ "$layer" = "platform" ]; then
          if printf '%s' "$pkg" | grep -qiE "$PROVIDER_SDK_PATTERN"; then
            note "FAIL: $dir is in the $layer layer and imports the provider SDK $pkg."
            note "      rule violated: ${layer}/app ─X─► NAS SDKs (EPIC B #81, \"Dependency rule\")"
          fi
        fi
        ;;
    esac
  done <<EOF
$output
EOF
}

# The literal WP1.1 claim, kept as its own explicit assertion rather than
# folded into the layer loop below: ci.yml names this step "core/ has zero
# imports from apps/ (static check)", and a reader should be able to find
# that exact sentence being checked.
core_deps=$(cd core && GOWORK=off go list -deps ./... 2>&1) || {
  echo "FAIL: core/ (GOWORK=off) go list -deps ./... itself failed:" >&2
  echo "$core_deps" >&2
  exit 1
}
bad=$(printf '%s\n' "$core_deps" | grep -F '/apps/' || true)
if [ -n "$bad" ]; then
  note "FAIL: core/ imports code from apps/:"
  echo "$bad" >&2
fi
bad=$(printf '%s\n' "$core_deps" | grep -F '/distribution/' || true)
if [ -n "$bad" ]; then
  note "FAIL: core/ imports code from distribution/:"
  echo "$bad" >&2
fi

# Every Go module the repository TRACKS, classified by the manifest.
# Discovered rather than listed, so a module added without being classified
# is a manifest failure (check-layer-manifest.sh) rather than a module this
# check silently never looked at.
#
# git ls-files, not a filesystem walk (issue #207). The question this check
# asks is what the repository contains, and a walk answers a different one:
# what happens to be lying on this disk. Agent worktrees live under
# .claude/worktrees/, untracked but very much present, and a walk descends
# into all of them and reports every module it meets there as unclassified.
# In the primary checkout that was 161 failures and a gate that no commit
# could get past, while a fresh clone and each individual worktree passed,
# which is why it stayed invisible until the Phase 6 merge.
#
# It also settles a disagreement rather than only fixing a bug:
# check-layer-manifest.sh already classifies `git ls-files`, so before this
# the two checks held different opinions about which files the repository
# consists of.
checked=0
while IFS= read -r gomod; do
  gomod=${gomod#./}
  dir=$(dirname "$gomod")
  layer=$(arch::classify "$gomod" | awk '{print $1}') || layer=""
  if [ -z "$layer" ]; then
    note "FAIL: the Go module at $dir is classified into no layer by scripts/architecture/layers.conf."
    continue
  fi
  echo "==> $dir ($layer layer)"
  check_module "$dir" "$layer"
  checked=$((checked + 1))
done < <(git ls-files -- '*go.mod' | sort)

# A run that inspected nothing must not report success. Without this,
# anything that made the listing above return no go.mod (a rename, a
# pathspec that stops matching, a run from outside a repository) would
# print a cheerful OK over zero modules.
if [ "$checked" -eq 0 ]; then
  echo "FAIL: no Go module was inspected, so this check verified nothing." >&2
  exit 1
fi

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "  The layers and what each owns: docs/architecture/layers.md" >&2
  exit 1
fi

echo "OK: $checked Go module(s) respect the layer dependency direction, and core/ imports nothing from apps/ or distribution/."
