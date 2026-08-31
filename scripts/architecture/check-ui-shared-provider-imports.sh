#!/usr/bin/env bash
# The shared UI's provider-SDK import scan (issue #165).
#
# #165's acceptance criterion is stated over Phase 6's full platform list,
# which adds Portainer, CasaOS, ZimaOS and Dockge to the six that have
# directories today:
#
#   "GIVEN the shared UI source tree, WHEN it is scanned for UGOS, TrueNAS,
#    Unraid, Portainer, CasaOS, ZimaOS, Synology, OpenMediaVault or Proxmox
#    SDK imports, THEN none are found."
#
# Deleting a directory that does not exist yet proves nothing, so the four
# without directories are covered by scanning instead. That puts the rule in
# place BEFORE anyone reaches for the SDK, which is the only time a negative
# check is worth anything.
#
# This is the fast static half. verify-ui-shared-without-provider-sdks.sh is
# the heavy deletion proof, and calls this first so a violation reports in
# seconds rather than after an npm ci.
#
# The scan reads module SPECIFIERS, not whole lines. ui/shared legitimately
# names every platform in prose and in its PlatformId union (platform
# differences are capability data, per EPIC B #81, so the names have to
# exist somewhere), and a line-level grep would either fire on all of that
# or be watered down until it fired on nothing.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/architecture/lib.sh

# The full Phase 6 platform list, including the four with no directory yet.
PROVIDERS=(ugos synology truenas unraid openmediavault proxmox portainer casaos zimaos dockge)

# Third-party SDK package names, matched against a bare module specifier.
# Separate from PROVIDERS because a specifier like "@ixsystems/..." names
# TrueNAS's vendor rather than the platform.
SDK_PATTERN='ugreen|ugos|truenas|ixsystems|unraid|synology|openmediavault|proxmox|portainer|casaos|zimaos|dockge'

# ---- 2. import scan (fast, runs first so a violation reports in seconds) --

echo "==> scanning ui/shared/src for provider SDK imports (${#PROVIDERS[@]} platforms)"

# Every form that can pull a module into a TypeScript file: static import,
# re-export, dynamic import, and require. The specifier is captured, and
# only the specifier is matched.
specifier_re='(from|import|require)[[:space:]]*\(?[[:space:]]*["'"'"'][^"'"'"']+["'"'"']'

findings=""
while IFS= read -r file; do
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    spec=$(printf '%s' "$hit" | sed -E 's/.*["'"'"']([^"'"'"']+)["'"'"'].*/\1/')
    # A relative reach into a provider directory.
    if printf '%s' "$spec" | grep -qE '(^|/)apps/('"$(IFS='|'; echo "${PROVIDERS[*]}")"')(/|$)'; then
      findings="${findings}
  ${file}: imports ${spec} (a relative reach into a provider directory)"
      continue
    fi
    # A bare third-party provider SDK.
    case "$spec" in
      .*|/*) ;;
      *)
        if printf '%s' "$spec" | grep -qiE "$SDK_PATTERN"; then
          findings="${findings}
  ${file}: imports ${spec} (a provider SDK package)"
        fi
        ;;
    esac
  done < <(grep -oE "$specifier_re" "$file" || true)
done < <(find ui/shared/src -type f \( -name '*.ts' -o -name '*.tsx' \) | sort)

if [ -n "$findings" ]; then
  echo "FAIL: the shared UI imports provider-specific code. One shared UI, no provider SDKs in it (EPIC B #81):$findings" >&2
  exit 1
fi
echo "OK: no module specifier in ui/shared/src names a provider directory or a provider SDK."

