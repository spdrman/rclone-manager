#!/usr/bin/env bash
# The on-device driver for D2.1's resource certification (issue #89).
#
# It runs ON an authorized UGREEN NAS, against the canonical release as
# installed through the UGOS UPK, and it decides nothing. Everything it
# produces is raw: identity read off the device, raw process samples, raw
# per-request timings. Aggregation, threshold comparison and the pass/fail
# verdict all live in distribution/hwcert, where they are unit-tested and
# where scripts/hwcert/selftest.sh proves they can go red.
#
# That split is the point. Nobody can unit-test "sample /proc on a NAS",
# and nobody has to: the only thing this file can get wrong is a number,
# and a wrong number shows up in the record for a reviewer to see. If it
# decided anything, the deciding would be untested.
#
#   scripts/hwcert/measure-ugos.sh --base-url http://127.0.0.1:9443 \
#       --operator "who ran it" --out samples.json
#   ./hwcert record --samples samples.json --out ugos-resource-<arch>.json
#
# docs/acceptance/ugos-resource-certification.md is the procedure this
# implements, and it is the authority on every parameter below. The
# defaults here are read out of its own hwcert:method block rather than
# written down twice.
#
# # Nothing here writes a credential to disk
#
# The administrator password is read from a terminal prompt, handed to
# curl down a pipe, and never appears in argv, in the environment, in a
# file or in a shell history. The session and CSRF cookies live in shell
# variables for the length of the run. The emitted capture carries
# measurements and device identity only, which is why it is safe to commit
# the record built from it.
#
# # It refuses rather than fills in
#
# Every phase either produces samples or stops the run. A capture with a
# phase missing is better than a capture with a zero in it, because
# distribution/hwcert fails a missing measurement and would pass a zero on
# most of these metrics.
set -euo pipefail

PROCEDURE=docs/acceptance/ugos-resource-certification.md

BASE_URL=""
USERNAME="admin"
ENGINE="rclone-manager"
UI="web-ui"
OPERATOR=""
OUT="samples.json"
PROVIDER="ugos"
DOCKER="docker"
RAW_COPY_SOURCE=""
RAW_COPY_DEST=""
SHARE_RUNNING=""
SHARE_STOPPED=""
PROCEDURE_PATH=""
SKIP_TRANSFER=0

usage() {
  cat >&2 <<'EOF'
usage: scripts/hwcert/measure-ugos.sh [options]

  --base-url URL         the app's published listener, e.g. http://127.0.0.1:9443
  --username NAME        administrator account to authenticate as (default: admin)
  --engine NAME          engine container name (default: rclone-manager)
  --ui NAME              UI host container name (default: web-ui)
  --operator NAME        who is running this; it goes in the record
  --raw-copy-source PATH the artifact the raw-copy control reads
  --raw-copy-dest DIR    where the raw-copy control writes
  --share-running MB/S   file-share read throughput measured from a LAN
                         client with the app installed and idle
  --share-stopped MB/S   the same read with the app stopped
  --procedure PATH       the acceptance procedure to read the method from
                         (default: the checked-in one, when run in a checkout)
  --skip-transfer        omit the transfer phases; the resulting capture is
                         incomplete and hwcert will fail it, which is the
                         point of the flag: it is for shaking the plumbing
                         out, never for producing evidence
  --out PATH             where to write the capture (default: samples.json)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --base-url) BASE_URL="$2"; shift 2 ;;
    --username) USERNAME="$2"; shift 2 ;;
    --engine) ENGINE="$2"; shift 2 ;;
    --ui) UI="$2"; shift 2 ;;
    --operator) OPERATOR="$2"; shift 2 ;;
    --raw-copy-source) RAW_COPY_SOURCE="$2"; shift 2 ;;
    --raw-copy-dest) RAW_COPY_DEST="$2"; shift 2 ;;
    --share-running) SHARE_RUNNING="$2"; shift 2 ;;
    --share-stopped) SHARE_STOPPED="$2"; shift 2 ;;
    --procedure) PROCEDURE_PATH="$2"; shift 2 ;;
    --skip-transfer) SKIP_TRANSFER=1; shift ;;
    --out) OUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "measure-ugos: unknown option $1" >&2; usage; exit 2 ;;
  esac
done

for tool in curl python3 awk sed "$DOCKER"; do
  command -v "$tool" >/dev/null 2>&1 || { echo "measure-ugos: $tool is required and is not on PATH" >&2; exit 1; }
done
[ -n "$BASE_URL" ] || { echo "measure-ugos: --base-url is required" >&2; exit 2; }
[ -n "$OPERATOR" ] || { echo "measure-ugos: --operator is required; a hardware run is somebody's run" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/hwcert-measure.XXXXXX")
trap 'rm -rf "$work"' EXIT

die() { echo "measure-ugos: $*" >&2; exit 1; }
say() { echo "==> $*"; }

# ---------------------------------------------------------------- method

# The sampling parameters come out of the acceptance procedure, never from
# defaults written here: a window this script chose for itself would be a
# second copy of a number the procedure is supposed to own.
if [ -z "$PROCEDURE_PATH" ]; then
  if root=$(git rev-parse --show-toplevel 2>/dev/null); then
    PROCEDURE_PATH="$root/$PROCEDURE"
  fi
fi
[ -n "$PROCEDURE_PATH" ] && [ -f "$PROCEDURE_PATH" ] || \
  die "no acceptance procedure to read the sampling method from; copy $PROCEDURE to the device and pass --procedure"

method_json=$(python3 - "$PROCEDURE_PATH" <<'PY'
import re, sys, json
text = open(sys.argv[1]).read()
m = re.search(r"<!--\s*hwcert:method\s*-->(.*?)<!--\s*/hwcert:method\s*-->", text, re.S)
if not m:
    sys.exit("the procedure has no hwcert:method block, so nothing says what window to sample over")
out = {}
for line in m.group(1).splitlines():
    line = line.strip()
    if not line.startswith("|"):
        continue
    cells = [c.strip().strip("`") for c in line.strip("|").split("|")]
    if len(cells) != 2 or cells[0].lower() == "parameter" or set(cells[0]) <= set("-: "):
        continue
    out[cells[0]] = float(cells[1])
if not out:
    sys.exit("the procedure's hwcert:method block has no rows in it")
print(json.dumps(out))
PY
) || die "could not read the sampling method"

m() { printf '%s' "$method_json" | python3 -c "import json,sys;print(int(json.load(sys.stdin)['$1']))"; }

IDLE_WINDOW=$(m idle_window_seconds)
IDLE_INTERVAL=$(m idle_sample_interval_seconds)
TRANSFER_INTERVAL=$(m transfer_sample_interval_seconds)
READ_WARMUP=$(m api_read_warmup_requests)
READ_TIMED=$(m api_read_timed_requests)
WRITE_WARMUP=$(m config_write_warmup_requests)
WRITE_TIMED=$(m config_write_timed_requests)
STARTUP_REPS=$(m startup_repetitions)
TRANSFER_BYTES=$(m transfer_artifact_bytes)
TRANSFER_REPS=$(m transfer_repetitions)

say "sampling method from $PROCEDURE_PATH: ${IDLE_WINDOW}s idle window at ${IDLE_INTERVAL}s, ${READ_TIMED} timed reads, ${WRITE_TIMED} timed writes"

# -------------------------------------------------------------- identity

say "step 0: device and release identity"

uname_machine=$(uname -m)
kernel=$(uname -r)
os_name=$( (. /etc/os-release 2>/dev/null && printf '%s' "${PRETTY_NAME:-$NAME}") || printf 'unknown')
firmware=$( (. /etc/os-release 2>/dev/null && printf '%s' "${VERSION:-${VERSION_ID:-}}") || printf '')
[ -n "$firmware" ] || die "could not read the UGOS firmware version from /etc/os-release; it is one of §68's required evidence fields and the record is refused without it"
vendor="UGREEN"
model=$(cat /sys/devices/virtual/dmi/id/product_name 2>/dev/null || printf 'unknown')
[ "$model" != "unknown" ] || die "could not read the device model; pass it in by editing the capture is not an option, a certification record has to name the hardware"
cores=$(getconf _NPROCESSORS_ONLN)
memory=$(awk '/MemTotal/{print $2*1024}' /proc/meminfo)

engine_pid=$("$DOCKER" inspect -f '{{.State.Pid}}' "$ENGINE") || die "no container named $ENGINE"
ui_pid=$("$DOCKER" inspect -f '{{.State.Pid}}' "$UI") || die "no container named $UI"
[ "$engine_pid" != "0" ] || die "$ENGINE is not running, so there is nothing to sample"
[ "$ui_pid" != "0" ] || die "$UI is not running, so there is nothing to sample"

"$DOCKER" ps --format '{{.Names}}\t{{.Image}}\t{{.Command}}\t{{.Ports}}' > "$work/containers.tsv"
"$DOCKER" inspect -f '{{.Name}}|{{.Image}}|{{.Config.Image}}' "$ENGINE" "$UI" > "$work/inspect.txt"

image_ref=$("$DOCKER" inspect -f '{{.Config.Image}}' "$ENGINE")
image_size=$("$DOCKER" image inspect -f '{{.Size}}' "$image_ref") || die "could not size $image_ref"
registry_digest=$("$DOCKER" image inspect -f '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$image_ref" 2>/dev/null || true)
registry_digest=${registry_digest#*@}

# The binary, hashed off the running container. This is what
# distribution/hwcert compares against container/release-manifest.json's
# binary_sha256, and it is the strongest identity claim the manifest can
# actually back today: registry_digest is null there until #88 pushes
# something.
"$DOCKER" cp "$ENGINE:/backup-manager-web" "$work/backup-manager-web" >/dev/null 2>&1 \
  || die "could not copy /backup-manager-web out of $ENGINE, so the installed build cannot be identified"
binary_sha=$(sha256sum "$work/backup-manager-web" | awk '{print $1}')
rm -f "$work/backup-manager-web"

app_version=$(curl -fsS "$BASE_URL/api/v1/system/version" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("core_version",""))') \
  || die "could not read /api/v1/system/version; is --base-url right?"
[ -n "$app_version" ] || die "the version endpoint reported no core_version"

say "  $model, $os_name $firmware, $uname_machine, ${cores} cores"
say "  $image_ref, engine binary ${binary_sha:0:12}, app $app_version"

# ------------------------------------------------------------------ auth

say "authenticating as $USERNAME"

login_headers=$(curl -fsS -D - -o /dev/null "$BASE_URL/") || die "could not reach $BASE_URL/"
csrf=$(printf '%s' "$login_headers" | sed -n 's/.*[Ss]et-[Cc]ookie: *bm_csrf=\([^;]*\).*/\1/p' | tail -1)
[ -n "$csrf" ] || die "no bm_csrf cookie came back from $BASE_URL/, so the login cannot be signed"

printf 'administrator password for %s (not echoed, never written to disk): ' "$USERNAME" >&2
read -rs password
echo >&2

# The password goes to python3 on stdin and to curl on stdin. It is never
# an argument, never an environment variable, and never a file.
session=$(
  printf '%s' "$password" \
  | python3 -c 'import json,sys;print(json.dumps({"username":sys.argv[1],"password":sys.stdin.read()}))' "$USERNAME" \
  | curl -fsS -D - -o /dev/null -X POST \
      -H 'Content-Type: application/json' \
      -H "X-CSRF-Token: $csrf" \
      -H "Cookie: bm_csrf=$csrf" \
      --data-binary @- "$BASE_URL/api/v1/auth/login" \
  | sed -n 's/.*[Ss]et-[Cc]ookie: *bm_session=\([^;]*\).*/\1/p' | tail -1
) || die "login failed"
password=""
unset password
[ -n "$session" ] || die "the login returned no bm_session cookie"

COOKIE="bm_session=$session; bm_csrf=$csrf"
api() { curl -fsS -H "Cookie: $COOKIE" -H "X-CSRF-Token: $csrf" "$@"; }

# -------------------------------------------------------------- sampling

# sample_proc <pid> <seconds> <interval> <label> writes one JSON array of
# ProcSamples. Presence, pid and start time are recorded on every sample,
# not just memory and CPU: an absent process and an idle one report the
# same CPU, and distribution/hwcert refuses a window it cannot tell apart.
sample_proc() {
  local pid=$1 seconds=$2 interval=$3 out=$4
  local ticks pagesize
  ticks=$(getconf CLK_TCK)
  pagesize=$(getconf PAGESIZE)
  local start; start=$(date +%s)
  : > "$out.raw"
  while :; do
    local now elapsed
    now=$(date +%s)
    elapsed=$((now - start))
    if [ "$elapsed" -gt "$seconds" ]; then break; fi
    if [ -r "/proc/$pid/stat" ]; then
      # Fields after the comm field, which can itself contain spaces:
      # everything is read relative to the last ')'.
      awk -v t="$elapsed" -v ticks="$ticks" -v pid="$pid" -v pagesize="$pagesize" '
        {
          line = $0
          i = index(line, ")")
          rest = substr(line, i + 2)
          n = split(rest, f, " ")
          # f[1] is state, which is field 3 of the original record, so
          # field N is f[N-2]: utime(14)=f[12], stime(15)=f[13],
          # starttime(22)=f[20], rss in pages(24)=f[22].
          printf "%d\t1\t%s\t%s\t%s\t%.3f\n", t, pid, f[20], f[22] * pagesize, (f[12] + f[13]) / ticks
        }' "/proc/$pid/stat" >> "$out.raw"
    else
      printf "%d\t0\t0\t0\t0\t0\n" "$elapsed" >> "$out.raw"
    fi
    sleep "$interval"
  done
  python3 - "$out.raw" > "$out" <<'PY'
import sys, json
rows = []
for line in open(sys.argv[1]):
    at, present, pid, start, rss, cpu = line.split("\t")
    rows.append({
        "at_seconds": float(at),
        "present": present == "1",
        "pid": int(pid),
        "start_ticks": int(start),
        "rss_bytes": int(rss),
        "cpu_seconds": float(cpu),
    })
if len(rows) < 2:
    sys.exit("the window produced %d sample(s); a rate needs at least two" % len(rows))
print(json.dumps(rows))
PY
}

# timed <n> <curl args...> issues n requests over ONE curl invocation, so
# they share a keep-alive connection, and prints one wall-clock time per
# request in milliseconds.
timed() {
  local n=$1; shift
  local urls=() i
  for ((i = 0; i < n; i++)); do urls+=("$1"); done
  shift
  curl -sS -o /dev/null -w '%{time_total}\n' -H "Cookie: $COOKIE" -H "X-CSRF-Token: $csrf" "$@" "${urls[@]}" \
    | awk '{printf "%.3f\n", $1 * 1000}'
}

# --------------------------------------------------- step 2: startup

say "step 2: startup to healthy, ${STARTUP_REPS} restarts"
startup_ms=()
for ((i = 0; i < STARTUP_REPS; i++)); do
  "$DOCKER" restart "$ENGINE" >/dev/null || die "could not restart $ENGINE"
  begin=$(python3 -c 'import time;print(time.time())')
  deadline=$(python3 -c 'import time;print(time.time()+120)')
  while :; do
    if curl -fsS -o /dev/null --max-time 2 "$BASE_URL/health/live" 2>/dev/null; then break; fi
    if python3 -c "import sys,time;sys.exit(0 if time.time() > $deadline else 1)"; then
      die "the engine did not answer /health/live within 120s of a restart"
    fi
    sleep 0.01
  done
  startup_ms+=("$(python3 -c "import time;print(round((time.time()-$begin)*1000,3))")")
done
engine_pid=$("$DOCKER" inspect -f '{{.State.Pid}}' "$ENGINE")

# --------------------------------------------------- step 3: idle window

say "step 3: idle window, ${IDLE_WINDOW}s at ${IDLE_INTERVAL}s (this is the long one)"
sample_proc "$engine_pid" "$IDLE_WINDOW" "$IDLE_INTERVAL" "$work/engine_idle.json" &
engine_sampler=$!
sample_proc "$ui_pid" "$IDLE_WINDOW" "$IDLE_INTERVAL" "$work/ui_idle.json" &
ui_sampler=$!
wait "$engine_sampler" || die "the engine idle window did not produce samples"
wait "$ui_sampler" || die "the UI idle window did not produce samples"

# ------------------------------------------------- step 4: API latency

say "step 4: /api/v1 read and configuration write latency"
timed "$READ_WARMUP" "$BASE_URL/api/v1/backup-sets" >/dev/null
timed "$READ_TIMED" "$BASE_URL/api/v1/backup-sets" > "$work/api_read_ms.txt"

# An idempotent settings write: the timezone the running configuration
# already has, sent back. It really rewrites config.yaml and moves the
# config revision, and it changes no behaviour.
api "$BASE_URL/api/v1/settings" \
  | python3 -c 'import json,sys;s=json.load(sys.stdin);print(json.dumps({"retention":{"timezone":s["retention"]["timezone"]}}))' \
  > "$work/settings-patch.json" || die "could not read /api/v1/settings"
timed "$WRITE_WARMUP" "$BASE_URL/api/v1/settings" -X PATCH -H 'Content-Type: application/json' -d "@$work/settings-patch.json" >/dev/null
timed "$WRITE_TIMED" "$BASE_URL/api/v1/settings" -X PATCH -H 'Content-Type: application/json' -d "@$work/settings-patch.json" > "$work/config_write_ms.txt"

# ------------------------------------------------- step 5: the transfer

transfer_mb=()
raw_copy_mb=()

if [ "$SKIP_TRANSFER" = 1 ]; then
  say "step 5: SKIPPED. The capture will be incomplete and hwcert will refuse it"
else
  [ -n "$RAW_COPY_SOURCE" ] && [ -n "$RAW_COPY_DEST" ] || \
    die "step 5 needs --raw-copy-source and --raw-copy-dest: without the device's own copy rate there is nothing to hold the app's transfer against"

  say "step 5: ${TRANSFER_REPS} backup runs, the API under one of them, and the raw-copy control"
  for ((i = 0; i < TRANSFER_REPS; i++)); do
    revision=$(curl -fsS "$BASE_URL/api/v1/system/version" | python3 -c 'import json,sys;print(json.load(sys.stdin)["config_revision"])')
    op=$(api -X POST -H 'Content-Type: application/json' \
      -H "Idempotency-Key: hwcert-$(date +%s)-$i" \
      -d "{\"action\":\"run_cycle\",\"config_revision\":\"$revision\"}" \
      "$BASE_URL/api/v1/operations" | python3 -c 'import json,sys;print(json.load(sys.stdin)["operation_id"])') \
      || die "could not submit a run cycle"

    if [ "$i" = 0 ]; then
      sample_proc "$engine_pid" "$((TRANSFER_INTERVAL * 12))" "$TRANSFER_INTERVAL" "$work/engine_transfer.json" &
      transfer_sampler=$!
      timed "$READ_TIMED" "$BASE_URL/api/v1/backup-sets" > "$work/api_read_under_transfer_ms.txt"
    fi

    while :; do
      body=$(api "$BASE_URL/api/v1/operations/$op")
      finished=$(printf '%s' "$body" | python3 -c 'import json,sys;o=json.load(sys.stdin);print(o.get("finished_at",""))')
      [ -n "$finished" ] && break
      sleep 1
    done
    elapsed=$(printf '%s' "$body" | python3 -c '
import json,sys,datetime
o=json.load(sys.stdin)
s=datetime.datetime.fromisoformat(o["started_at"].replace("Z","+00:00"))
f=datetime.datetime.fromisoformat(o["finished_at"].replace("Z","+00:00"))
print((f-s).total_seconds())')
    transfer_mb+=("$(python3 -c "print(round($TRANSFER_BYTES / 1000000.0 / max($elapsed, 1e-6), 3))")")

    if [ "$i" = 0 ]; then
      wait "$transfer_sampler" || die "the transfer window did not produce samples"
    fi
  done

  say "  the raw-copy control: the same bytes, the platform's own copy"
  for ((i = 0; i < TRANSFER_REPS; i++)); do
    begin=$(python3 -c 'import time;print(time.time())')
    cp "$RAW_COPY_SOURCE" "$RAW_COPY_DEST/hwcert-raw-copy.$i" || die "the raw copy failed"
    sync
    secs=$(python3 -c "import time;print(time.time()-$begin)")
    bytes=$(stat -c %s "$RAW_COPY_SOURCE")
    raw_copy_mb+=("$(python3 -c "print(round($bytes / 1000000.0 / max($secs, 1e-6), 3))")")
    rm -f "$RAW_COPY_DEST/hwcert-raw-copy.$i"
  done
fi

# ---------------------------------------------------------- the capture

say "writing $OUT"

# Every shell value below is passed as an argument, never interpolated into
# the python source: a model string with a quote in it is a device
# identifier, not a syntax error.
case "$uname_machine" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) die "uname -m says $uname_machine, which is neither of the architectures this release claims" ;;
esac

# The numeric series go through files for the same reason.
printf '%s\n' ${startup_ms[@]+"${startup_ms[@]}"} > "$work/startup_to_healthy_ms.txt"
printf '%s\n' ${transfer_mb[@]+"${transfer_mb[@]}"} > "$work/transfer_mb_per_second.txt"
printf '%s\n' ${raw_copy_mb[@]+"${raw_copy_mb[@]}"} > "$work/raw_copy_mb_per_second.txt"
if [ -n "$SHARE_RUNNING" ]; then printf '%s\n' "$SHARE_RUNNING" > "$work/share_read_mb_per_second_app_running.txt"; fi
if [ -n "$SHARE_STOPPED" ]; then printf '%s\n' "$SHARE_STOPPED" > "$work/share_read_mb_per_second_app_stopped.txt"; fi

python3 - "$OUT" "$work" "$method_json" "$architecture" "$PROVIDER" "$ENGINE" "$UI" \
  "$vendor" "$model" "$os_name" "$firmware" "$kernel" "$uname_machine" "$cores" "$memory" \
  "$app_version" "$image_ref" "$binary_sha" "$registry_digest" "$image_size" "$OPERATOR" <<'PY'
import datetime
import json
import os
import sys

(out, work, method_json, architecture, provider, engine, ui,
 vendor, model, os_name, firmware, kernel, uname_machine, cores, memory,
 app_version, image_ref, binary_sha, registry_digest, image_size, operator) = sys.argv[1:22]


def numbers(name):
    path = os.path.join(work, name + ".txt")
    if not os.path.exists(path):
        return []
    return [float(line) for line in open(path) if line.strip()]


def window(name, process):
    path = os.path.join(work, name + ".json")
    if not os.path.exists(path):
        return None
    return {"process": process, "samples": json.load(open(path))}


containers = []
for line in open(os.path.join(work, "containers.tsv")):
    cells = (line.rstrip("\n").split("\t") + ["", "", "", ""])[:4]
    name, image, command, ports = cells
    if name not in (engine, ui):
        continue
    containers.append({
        "name": name,
        "image": image,
        "image_id": "",
        "command": command.strip('"'),
        "published_ports": [p.strip() for p in ports.split(",") if p.strip()],
    })

capture = {
    "architecture": architecture,
    "provider": provider,
    "probe": {"goarch": "", "version": ""},
    "device": {
        "vendor": vendor,
        "model": model.strip(),
        "os_name": os_name.strip(),
        "firmware_version": firmware.strip(),
        "kernel": kernel,
        "uname_machine": uname_machine,
        "cpu_cores": int(cores),
        "memory_bytes": int(memory),
    },
    "release": {
        "version": app_version,
        "image_ref": image_ref,
        "binary_sha256": {"backup-manager-web": binary_sha},
        "registry_digest": registry_digest,
    },
    "deployment": {"containers": containers},
    "method": json.loads(method_json),
    "windows": {},
    "series": {},
    "scalars": {"image_size_bytes": int(image_size)},
    "captured_at": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
    "operator": operator,
}

for name, process in (("engine_idle", engine), ("ui_idle", ui), ("engine_transfer", engine)):
    w = window(name, process)
    if w is not None:
        capture["windows"][name] = w

# Absent is absent. A phase that produced nothing leaves its series out,
# and distribution/hwcert fails the metric as missing; writing a zero here
# would read as a measured collapse and pass every lower_is_better budget.
for name in ("startup_to_healthy_ms", "api_read_ms", "config_write_ms",
             "api_read_under_transfer_ms", "transfer_mb_per_second",
             "raw_copy_mb_per_second", "share_read_mb_per_second_app_running",
             "share_read_mb_per_second_app_stopped"):
    values = numbers(name)
    if values:
        capture["series"][name] = values

with open(out, "w") as fh:
    json.dump(capture, fh, indent=2)
    fh.write("\n")
PY

say "done. Turn it into an evidence record with a probe built for this device:"
say "    ./hwcert record --samples $OUT --out ugos-resource-$architecture.json"
