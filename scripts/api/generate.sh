#!/usr/bin/env bash
# Regenerate the /api/v1 bindings from the authoritative contract
# (issue #166).
#
# Run this after editing api/v1/openapi.json. The two files it writes are
# generated output and carry a DO NOT EDIT banner; scripts/api/check-contract-drift.sh
# fails CI if either one stops matching what this produces.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=./lib.sh
source scripts/api/lib.sh

mkdir -p "$(dirname "$API_GO_BINDING")" "$(dirname "$API_TS_BINDING")"
api::generate "$API_GO_BINDING" "$API_TS_BINDING"

echo "OK: regenerated"
echo "  $API_GO_BINDING"
echo "  $API_TS_BINDING"
