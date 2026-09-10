#!/usr/bin/env bash
# Builds the minimal UPK proof's Docker image locally (issue #91) and exports it into the
# ugcli project's staging tree, ready to be copied to the developer-authorized NAS for
# `ugcli check` / `ugcli pack` (ugcli itself only runs on Linux, so packaging happens on
# the device — this script does the heavy build work locally instead, per the task's
# "prefer building on your own local worktree, only touch the device for install/verify").
#
# Usage: apps/ugos/upk-proof/build.sh [version] [arch]
#   version defaults to the version in packaging/project.yaml
#   arch defaults to amd64 (the only architecture this proof was actually tested against —
#   see apps/ugos/docs/upk-proof-procedure.md for why arm64 isn't included here)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

arch="${2:-amd64}"
version="${1:-$(grep -m1 '^version:' apps/ugos/upk-proof/packaging/project.yaml | awk '{print $2}')}"
image="backup-manager-upk-proof:${version}-${arch}"
out_dir="apps/ugos/upk-proof/packaging/rootfs_${arch}/images"
out_tar="${out_dir}/backup-manager-upk-proof-${version}-${arch}.tar"

echo "==> building ${image} (platform linux/${arch})"
docker build \
  --platform "linux/${arch}" \
  -f apps/ugos/upk-proof/Dockerfile \
  -t "${image}" \
  .

mkdir -p "${out_dir}"
echo "==> docker save -> ${out_tar}"
docker save "${image}" -o "${out_tar}"

echo "OK: ${out_tar}"
echo "Next: copy apps/ugos/upk-proof/packaging/ to the NAS, then on the NAS:"
echo "  cd <copied-packaging-dir> && ugcli check && ugcli pack --arch ${arch} --build <n>"
