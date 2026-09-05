#!/usr/bin/env bash
# EPIC-B WP1.1 RED plan: "ui/shared/ builds with provider SDK directories
# removed" (docs/EPIC-B-multi-nas.md §69 WP1.1, §11), extended by issue
# #165 to Phase 6's full platform list and to a static import scan.
#
# Two halves, and they catch different things.
#
# 1. DELETION. Every provider directory is removed in a throwaway worktree
#    and ui/shared is installed and built there. This catches anything that
#    resolves a provider path at build time, including the "@platform-entry"
#    alias ui/shared/vite.config.ts resolves into apps/<platform>/frontend/.
#
#    apps/generic/ is the vendor-neutral baseline the default Vite build
#    targets (VITE_PLATFORM defaults to "generic") and apps/common/ carries
#    no frontend of its own, so neither is a "provider SDK directory" in the
#    sense this check cares about and both stay. Every actual NAS-vendor
#    directory goes.
#
# 2. IMPORT SCAN, in check-ui-shared-provider-imports.sh, run first from
#    here so a violation reports in seconds rather than after an npm ci. It
#    covers Portainer, CasaOS, ZimaOS and Dockge, which have no directory to
#    delete yet, and it is what makes #165's acceptance criterion true over
#    the full Phase 6 platform list rather than only over the six shipped
#    ones. See that script for why it matches module specifiers rather than
#    whole lines.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

# The full Phase 6 platform list. The four with no directory today are
# deliberately listed: rm -rf on a missing path is a no-op, and the import
# scan below is what actually covers them until #170 creates them.
PROVIDERS=(ugos synology truenas unraid openmediavault proxmox portainer casaos zimaos dockge)

echo "==> provider-SDK import scan (check-ui-shared-provider-imports.sh)"
bash scripts/architecture/check-ui-shared-provider-imports.sh

# ---- 1. deletion proof ----------------------------------------------------

wt=""
# Trapped on EXIT rather than run at the end, because this check deletes
# whole directories inside the worktree and then runs a build in it: an
# interrupt or a failing build partway through would otherwise leave a
# registered git worktree full of holes behind, and the next run inherits
# it. The guard is for the interrupt that lands before the worktree
# exists.
cleanup() { [ -n "$wt" ] && arch::cleanup_worktree "$wt"; }
trap cleanup EXIT

arch::make_worktree wt

for provider in "${PROVIDERS[@]}"; do
  rm -rf "${wt:?}/apps/${provider}"
done

echo "==> npm ci (ui/shared, with every provider SDK directory removed)"
(cd "$wt/ui/shared" && npm ci --no-audit --no-fund)

echo "==> npm run build (ui/shared, with every provider SDK directory removed)"
(cd "$wt/ui/shared" && npm run build)

echo "OK: ui/shared builds with every provider SDK directory removed."
