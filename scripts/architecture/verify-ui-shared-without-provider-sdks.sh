#!/usr/bin/env bash
# EPIC-B WP1.1 RED plan: "ui/shared/ builds with provider SDK directories
# removed" (docs/EPIC-B-multi-nas.md §69 WP1.1, §11).
#
# apps/generic/ is the vendor-neutral baseline the default Vite build
# targets (VITE_PLATFORM defaults to "generic" in ui/shared/vite.config.ts)
# and apps/common/ carries no frontend of its own — neither is a "provider
# SDK directory" in the sense this check cares about, so both stay. Every
# actual NAS-vendor provider directory is deleted.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

wt=""
cleanup() { [ -n "$wt" ] && arch::cleanup_worktree "$wt"; }
trap cleanup EXIT

arch::make_worktree wt

for provider in ugos synology truenas unraid openmediavault proxmox; do
  rm -rf "${wt:?}/apps/${provider}"
done

echo "==> npm ci (ui/shared, with every provider SDK directory removed)"
(cd "$wt/ui/shared" && npm ci --no-audit --no-fund)

echo "==> npm run build (ui/shared, with every provider SDK directory removed)"
(cd "$wt/ui/shared" && npm run build)

echo "OK: ui/shared builds with every provider SDK directory removed."
