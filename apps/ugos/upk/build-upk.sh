#!/usr/bin/env bash
# Stages the UGOS Pro package for one architecture, and refuses to stage a
# package whose image content is not the canonical release's.
#
# The name says "build" because that is what the step is called; nothing
# here compiles anything, and that is the whole rule this package sits
# under (issue #83). The image comes out of the canonical release BY
# DIGEST and is copied, never rebuilt. If this script ever grows a
# `docker build`, the package has stopped being a thin adapter.
#
# What it does, in order:
#
#   1. resolve the digest for the architecture out of
#      container/release-manifest.json;
#   2. `docker pull` that digest and tag it as the canonical reference;
#   3. `docker save` it into rootfs_<arch>/images/, with a sidecar
#      recording the digest it was pulled by and what the daemon agrees
#      it holds;
#   4. draw the App Center icon;
#   5. run distribution/cmd/upkverify over the staged tree, and stop dead
#      if it fails.
#
# Then, ON A UGOS DEVICE (ugcli is Linux-only and the device is where the
# App Center install happens):
#
#   ugcli check
#   ugcli pack --arch <arch> --build <n>
#
# Usage:
#   apps/ugos/upk/build-upk.sh [--arch amd64|arm64] [--source <ref>@sha256:...]
#
# --source is the escape hatch for an UNPUBLISHED release, and it is
# deliberately explicit. 0.3.0 is cut and not pushed, so the manifest
# records no digest to pull; rather than guess, this refuses and tells you
# to name the reference you want staged. Whatever you name is recorded in
# the sidecar verbatim, so the package always says where its bytes came
# from even when the release cannot say it for you.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

stage="apps/ugos/upk"
manifest="container/release-manifest.json"
arch="amd64"
source_ref=""

while [ $# -gt 0 ]; do
    case "$1" in
        --arch) arch="$2"; shift 2 ;;
        --source) source_ref="$2"; shift 2 ;;
        -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

case "$arch" in
    amd64|arm64) ;;
    *) echo "unsupported architecture: $arch" >&2; exit 2 ;;
esac

reference="$(python3 -c '
import json,sys
print(json.load(open(sys.argv[1]))["image"]["reference"])
' distribution/packaging/canonical.json)"

recorded_digest="$(python3 -c '
import json,sys
m=json.load(open(sys.argv[1]))
for a in m["architectures"]:
    if a["architecture"]==sys.argv[2]:
        print(a["registry_digest"] or "")
        break
' "$manifest" "$arch")"

if [ -z "$source_ref" ]; then
    if [ -z "$recorded_digest" ]; then
        cat >&2 <<EOF
ERROR: ${reference} records no ${arch} registry digest.

The release is cut and not pushed (canonical.json image.published is false
and ${manifest} has registry_digest null), so there is no
published digest to pull. Nothing here will guess one.

Either wait for the push, which fills the digest in and makes this script
need no argument at all, or name the reference to stage explicitly:

  $0 --arch ${arch} --source <reference>@sha256:<digest>

Whatever you name is recorded in the package's own provenance sidecar, and
the binary content is still held to ${manifest}.
EOF
        exit 2
    fi
    # Built out of canonical.json's own registry and repository fields
    # rather than by trimming the tag off the reference: a registry host
    # may carry a port, and a `%%:*` trim would then eat it.
    source_ref="$(python3 -c '
import json,sys
c=json.load(open("distribution/packaging/canonical.json"))["image"]
print(f"{c[\"registry\"]}/{c[\"repository\"]}@{sys.argv[1]}")
' "$recorded_digest")"
fi

images_dir="${stage}/rootfs_${arch}/images"
tar_name="backup-manager-$(python3 -c '
import json
print(json.load(open("distribution/packaging/canonical.json"))["image"]["tag"])
')-${arch}.tar"

echo "==> pulling ${source_ref} (linux/${arch})"
docker pull --platform "linux/${arch}" "${source_ref}" >/dev/null

# What the local daemon says it now holds, recorded as the LIST it is. A
# multi-architecture release legitimately produces two entries, the index
# digest and this architecture's own manifest digest, in an order nothing
# promises, so `RepoDigests 0` is a coin flip between them. The digest
# this package claims is the one the pull was made BY, which the daemon
# content-checked on the way in; the list is what it was seen to agree
# with.
daemon_digests="$(docker image inspect --format '{{json .RepoDigests}}' "${source_ref}")"
requested_digest="${source_ref##*@}"

echo "==> tagging as the canonical reference ${reference}"
docker tag "${source_ref}" "${reference}"

mkdir -p "${images_dir}"
echo "==> docker save -> ${images_dir}/${tar_name}"
rm -f "${images_dir}"/*.tar
docker save "${reference}" -o "${images_dir}/${tar_name}"

python3 - "$images_dir/image-source.json" "$reference" "$requested_digest" "$arch" "$tar_name" "$source_ref" "$daemon_digests" <<'SIDECAR'
import json, sys, datetime
path, reference, digest, arch, tar, requested, daemon = sys.argv[1:8]
if not digest.startswith("sha256:"):
    sys.exit("the source reference %s is not pinned to a digest; this package only ever stages a digest-pinned pull" % requested)
held = json.loads(daemon) or []
json.dump({
    "reference": reference,
    "digest": digest,
    "architecture": arch,
    "tar": tar,
    "fetched_at": datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "fetched_by": "apps/ugos/upk/build-upk.sh",
    "daemon_repo_digests": held,
}, open(path, "w"), indent=2)
open(path, "a").write("\n")
SIDECAR
echo "==> wrote ${images_dir}/image-source.json (${requested_digest})"

"${stage}/draw-icon.py" "${stage}/rootfs_common/icon.png"
echo "==> drew ${stage}/rootfs_common/icon.png"

echo "==> verifying the staged package against ${manifest}"
if ! go run ./distribution/cmd/upkverify -stage "${stage}" -arch "${arch}" -manifest "${manifest}"; then
    cat >&2 <<EOF

REFUSING TO STAGE. The bytes in this package are not the canonical
release's bytes for ${arch}. Packing it would ship a UGOS build the
release did not produce, which is the one thing issue #83 forbids.
EOF
    exit 1
fi

cat <<EOF

Staged ${stage} for ${arch}.

Next, on a developer-authorized UGOS Pro NAS (ugcli is Linux-only):

  ugcli check
  ugcli pack --arch ${arch} --build <n>
EOF
