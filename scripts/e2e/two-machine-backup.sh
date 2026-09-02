#!/usr/bin/env bash
# Two throwaway machines, a temporary network, and one real backup
# (issue #356), plus the completed installs three other issues were closed
# without (#347, #346, #343).
#
# Everything between "nothing installed" and "an artifact is on disk" was
# proven in pieces and nowhere joined up. core/tests/sftpfixture proves
# the transport, distribution/packaging proves the artifacts, and
# scripts/install's own suite proves the installer's logic against a
# mocked Docker. None of them can say that a fresh install, pointed at a
# machine, pulls a backup off it. That is the only claim a user makes, and
# this is the test for it.
#
# What it stands up, per case:
#
#   * a MANAGER machine: docker-in-docker, so it is a box with Docker and
#     nothing else, playing the NAS;
#   * a SOURCE machine: a real sshd (atmoz/sftp, the same image
#     core/tests/sftpfixture uses) holding a payload of known content,
#     playing the VPS being backed up;
#   * a temporary network joining exactly those two.
#
# Then it installs rclone-manager onto the manager machine with
# scripts/install/install_docker_host.py, the real installer, creates a
# backup set through the CLI, runs it, and compares the artifact's SHA-256
# against the source's. Not "the file is there": the bug this was written
# after (#264) was a transfer that failed while everything around it
# looked healthy, so the assertion is on the bytes.
#
# # The order, and why the manager machine comes first
#
# The installer generates the SSH keypair now and prints the public half
# (#347), so the machine being backed up cannot authorise it until it
# exists. That is the real sequence an operator follows, so it is the
# sequence here: install, take the printed public key, authorise it on the
# source, then add the backup set. Nothing in this script writes a client
# key of its own.
#
# # Why docker-in-docker rather than the host's socket
#
# Mounting /var/run/docker.sock into a "manager" container would make the
# product's own containers siblings on the developer's host rather than
# residents of the fake machine. The installer would then be installing
# onto the machine this script is running on, which is the one thing this
# test exists not to do. dind costs a privileged container and about
# fifteen seconds; what it buys is that "a fresh machine" is true.
#
# # Why the image is built here rather than pulled
#
# The image under test is built from THIS working tree and moved into the
# manager machine's own daemon with `docker save | docker load`. So the
# run proves this code rather than whatever is published, and it needs no
# registry. #342 exists because a stale published default installed 0.1.0
# and reported success, so every case also asserts that the engine reports
# the version and commit that were installed.
#
# # The cases
#
#   plain           the ordinary install: --image and --no-pull, and the
#                   canonical compose file copied in from a checkout.
#
#   no-arguments    #347 and #346 together, and the reason both were
#                   reopened. `install` with NO arguments at all, on a
#                   machine holding one copied install_docker_host.py and
#                   no checkout anywhere: no --compose-file (the embedded
#                   copy is what gets written), no --ssh-key (the
#                   installer generates the keypair), no --prefix, no
#                   --image. It runs all the way to a serving stack and
#                   then pulls a real backup, which is the half neither
#                   issue's evidence reached.
#
#   connection-cap  #264, the shape that actually broke on real hardware.
#                   Both production sources carry an iptables rule
#                   rejecting a third simultaneous SSH connection from one
#                   address with a TCP reset. rclone-manager failed
#                   against it because every operation built its own Fs
#                   and nothing released one, so the pools accumulated
#                   until the third was refused. Listing succeeded and the
#                   transfer got "connection refused". The source machine
#                   here carries that same rule (-m connlimit, which needs
#                   NET_ADMIN and not privileged), and the case proves the
#                   cap bites before it trusts it.
#
#                   sshd's own MaxStartups was the cheaper thing to try
#                   and it does not model this: it bounds connections that
#                   have not finished authenticating, and a leaked pool's
#                   connections are authenticated and idle, so they never
#                   count. connlimit counts established connections per
#                   source address, which is the production rule restated.
#
#   lifecycle       #343's two counting criteria, which are written in
#                   deliberately anti-assertion language. It backs up,
#                   creates an administrator, counts users, backup sets
#                   and catalogued artifacts, runs --mode upgrade to a
#                   newer tag, counts again, then runs --mode
#                   factory-reset and watches the resulting install issue
#                   an enrollment link, which is the only thing that
#                   proves the administrator record went with the
#                   database.
#
# # Hygiene
#
# Every container and the network are torn down on success, on failure and
# on interrupt. Every name carries a per-run id, and no host port is
# published at all, so two of these can run at once. Every wait has a
# deadline and names what it was waiting for: an end-to-end test that
# hangs forever on a cold machine is worse than one that fails.
#
# # Cost, written down rather than left to be rediscovered
#
# One case is about seventy seconds on the machine this was written on,
# once the image under test is built. The image build is the expensive
# part on a cold Docker cache (minutes: it compiles the Go binaries and
# builds the UI bundle) and is done once for the whole run.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

# ---------------------------------------------------------------- output

step() { echo ""; echo "==> two-machine: $*"; }
note() { echo "    $*"; }

die() {
  echo "" >&2
  echo "==> two-machine: FAILED. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit 1
}

# A capability this machine does not have is not a failure and is not a
# pass either. Exit 3, the same status scripts/lib/ci-local-gate.sh uses
# for INCOMPLETE, so the caller can tell "could not run it" from "ran it
# and it failed" without parsing prose.
EXIT_CANNOT_RUN=3
cannot_run() {
  echo "" >&2
  echo "==> two-machine: CANNOT RUN. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit "$EXIT_CANNOT_RUN"
}

# --------------------------------------------------------------- options

cases="all"
keep=0
while [ $# -gt 0 ]; do
  case "$1" in
    --case) cases="${2:-}"; shift 2 ;;
    --case=*) cases="${1#--case=}"; shift ;;
    # Leaves the two containers and the network up after a FAILING case,
    # for reading. Never the default: the whole point of the teardown is
    # that a crashed run leaves nothing behind.
    --keep-on-failure) keep=1; shift ;;
    -h|--help)
      sed -n '2,110p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) die "unknown option $1" "Usage: $0 [--case plain|no-arguments|connection-cap|lifecycle|all] [--keep-on-failure]" ;;
  esac
done

case "$cases" in
  all) case_list="plain no-arguments connection-cap lifecycle" ;;
  plain|no-arguments|connection-cap|lifecycle) case_list="$cases" ;;
  *) die "unknown case $cases" "Cases are: plain, no-arguments, connection-cap, lifecycle, all." ;;
esac

# ------------------------------------------------------------ identities
#
# Everything this run creates is named from run_id, so two runs on one
# machine never collide over a container name or a network name, and
# nothing published to a host port means they cannot collide over one of
# those either. The manager machine publishes the Web UI inside its own
# dind daemon, which is a network namespace of its own, so 8080 there is
# not 8080 here.
run_id="${E2E_RUN_ID:-$$-$(date +%s)-${RANDOM}}"
label_key="rclone-manager-e2e"
label="$label_key=two-machine"

# Where this run's throwaway host keys and payload live. Inside the
# working tree, gitignored, for the reason scripts/e2e/run-tests-repo-gate.sh
# gives for its own scratch: nothing this repository's tooling creates
# should need a recursive delete outside the workspace to clean up after
# itself. The files below are removed by name at teardown.
run_dir="$repo_root/.e2e-two-machine/$run_id"

product_image="rclone-manager-e2e:$run_id"
source_image="rclone-manager-e2e-source:1"
machine_image="rclone-manager-e2e-machine:1"

# What the fake machines are made of. Both are pinned to a major version
# rather than a digest, for the same reason core/tests/sftpfixture pins
# atmoz/sftp:alpine by tag: this script never trusts either image's
# contents, it verifies what it needs directly, so a moved tag surfaces as
# a red run rather than as a silent change of behaviour.
source_base="atmoz/sftp:alpine"
machine_base="docker:28-dind"

sftp_user="backupuser"
sftp_uid=1001

# The cap the connection-cap case imposes. Two, so the THIRD simultaneous
# connection from the manager machine is refused, which is the production
# rule this models exactly.
connection_cap=2

# The two tags the lifecycle case installs and then upgrades to. Same
# bytes, two references: what makes it an upgrade rather than a converge
# is the TAG the installer compares (installed_image_tag against
# image_tag), and this case is about the mode's own bookkeeping, not about
# two different builds.
lifecycle_from="rclone-manager-e2e-lifecycle:0.2.0"
lifecycle_to="rclone-manager-e2e-lifecycle:0.3.0"

# The administrator the lifecycle case creates, so there is a user for the
# upgrade to preserve and for the factory reset to destroy. A throwaway
# password for a container that is deleted minutes later, and it never
# leaves this script's own process except down a pipe into stdin.
lifecycle_admin_user="e2e-operator"
lifecycle_admin_pass="e2e-$run_id-not-a-real-password"

# ------------------------------------------------------------- teardown
#
# One trap covering EXIT, INT and TERM. Everything created is registered
# here the moment it exists, so an interrupt between two steps still tears
# down what the earlier one made.
created_containers=()
created_networks=()
created_case_dirs=()
teardown_done=0

teardown() {
  local status=$?
  [ "$teardown_done" = 1 ] && return
  teardown_done=1

  if [ "$keep" = 1 ] && [ "$status" != 0 ]; then
    echo "" >&2
    echo "==> two-machine: --keep-on-failure, so these are left up for reading:" >&2
    for c in "${created_containers[@]:-}"; do [ -n "$c" ] && echo "        container $c" >&2; done
    for n in "${created_networks[@]:-}"; do [ -n "$n" ] && echo "        network   $n" >&2; done
    echo "        run dir   $run_dir" >&2
    return
  fi

  echo ""
  echo "==> two-machine: tearing down"
  for c in "${created_containers[@]:-}"; do
    [ -n "$c" ] && docker rm -f "$c" >/dev/null 2>&1 || true
  done
  # After the containers, never before: a network with an endpoint on it
  # cannot be removed, and a "network is in use" error at teardown time is
  # how a network survives a run.
  for n in "${created_networks[@]:-}"; do
    [ -n "$n" ] && docker network rm "$n" >/dev/null 2>&1 || true
  done
  docker image rm -f "$product_image" >/dev/null 2>&1 || true
  remove_run_dir
}
trap teardown EXIT INT TERM

# remove_run_dir deletes what this run wrote, by name, then removes the
# directories. Deliberately not a recursive delete: the files are a known,
# short list, and `rm -rf` on a path built from variables is how a script
# eventually deletes the wrong thing. An unexpected extra file leaves the
# directory behind, which is visible rather than silent.
remove_run_dir() {
  [ -d "$run_dir" ] || return 0
  for d in "${created_case_dirs[@]:-}"; do
    [ -n "$d" ] || continue
    rm -f "$d/authorized_keys/installer.pub" 2>/dev/null || true
    rmdir "$d/authorized_keys" "$d" 2>/dev/null || true
  done
  rm -f \
    "$run_dir/ssh_host_ed25519_key" "$run_dir/ssh_host_ed25519_key.pub" \
    "$run_dir/ssh_host_rsa_key" "$run_dir/ssh_host_rsa_key.pub" \
    "$run_dir/upload/payload.bin" "$run_dir/upload/schema.sql" "$run_dir/upload/notes.txt" \
    "$run_dir/product-image.tar" 2>/dev/null || true
  rmdir "$run_dir/upload" "$run_dir" 2>/dev/null || true
  rmdir "$repo_root/.e2e-two-machine" 2>/dev/null || true
}

# ------------------------------------------------------------- utilities

# wait_or_die <seconds> <what it is waiting for> <command...>
# Every wait in this script goes through here, so none of them can be the
# one that hangs a cold machine forever, and every timeout says what it
# was waiting for rather than only that it waited.
wait_or_die() {
  local budget="$1" what="$2"
  shift 2
  local deadline=$(( $(date +%s) + budget ))
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      die "timed out after ${budget}s waiting for $what."
    fi
    sleep 1
  done
}

sha256_of() {  # sha256_of <container> <path>
  docker exec "$1" sha256sum "$2" | awk '{print $1}'
}

# mgr_install runs the real installer on the manager machine. Every case
# goes through this one function, so the only difference between them is
# the argument list, which is the thing under test.
mgr_install() {
  local mgr="$1"; shift
  docker exec "$mgr" python3 /opt/rm/install_docker_host.py install "$@"
}

# mgr_compose runs a compose command on the manager machine's own daemon,
# against the deployment the installer staged there. The argument list is
# the installer's own (compose_argv in install_docker_host.py), so this
# drives exactly the deployment it produced rather than a second idea of
# one.
mgr_compose() {
  local mgr="$1" prefix="$2"; shift 2
  docker exec "$mgr" docker compose \
    -p rclone-manager \
    --env-file "$prefix/.env" \
    -f "$prefix/compose.yaml" \
    -f "$prefix/compose.image.yaml" \
    "$@"
}

bm() {  # bm <mgr> <prefix> <backup-manager args...>
  local mgr="$1" prefix="$2"; shift 2
  mgr_compose "$mgr" "$prefix" exec -T rclone-manager /backup-manager "$@"
}

# ------------------------------------------------------------- preflight

step "preflight"
for tool in docker ssh-keygen; do
  command -v "$tool" >/dev/null 2>&1 \
    || cannot_run "$tool is not on PATH, and this test needs it."
done
docker info >/dev/null 2>&1 \
  || cannot_run "the Docker daemon is not reachable." \
                "Start Docker and re-run. This test cannot be performed without it, and reporting a pass for a run that never happened is the one thing it must not do."
note "docker daemon reachable"

# Docker-in-docker needs a privileged container, which some hosts and most
# hardened CI runners refuse. Find that out in two seconds rather than
# after building an image.
if ! docker run --rm --privileged "$machine_base" true >/dev/null 2>&1; then
  cannot_run "this Docker daemon will not run a privileged container, so docker-in-docker cannot start." \
             "The manager machine has to be a machine, not a sibling of the containers under test:" \
             "mounting the host socket instead would install onto THIS host, which is the thing this test exists not to do."
fi
note "privileged containers are allowed, so docker-in-docker can start"

# What `--image` defaults to, read out of the installer itself rather than
# restated here. The no-arguments case passes no --image at all, so the
# only way it can install this working tree's build instead of reaching
# for a registry is if the image is already on the machine under exactly
# that reference. Reading the default keeps the two from drifting: if the
# installer's default moves, this moves with it, and if its shape changes
# this fails by name rather than silently installing a published release.
default_image="$(python3 - <<'PY'
import re, sys
src = open("scripts/install/install_docker_host.py", encoding="utf-8").read()
m = re.search(r'"--image",\s*default="([^"]+)"', src)
if not m:
    sys.exit(1)
print(m.group(1))
PY
)" || die "could not read the installer's own --image default out of scripts/install/install_docker_host.py." \
         "The no-arguments case (#347) has to pre-load the image under exactly that reference, or it would either" \
         "reach for a registry or test a published release instead of this working tree."
note "the installer's --image default is $default_image"

# ------------------------------------------------------- build the images

step "building the image under test from this working tree"
version="$(git rev-parse --short HEAD)"
commit="$(git rev-parse HEAD)"
if ! git diff --quiet HEAD 2>/dev/null || ! git diff --cached --quiet 2>/dev/null; then
  # Same convention as scripts/e2e/run-tests-repo-gate.sh: under a
  # pre-commit hook HEAD is the parent commit and the tree carries the
  # staged change, so the build genuinely is HEAD plus something and says
  # so.
  commit="$commit-dirty"
fi
note "VERSION=$version COMMIT=$commit"
docker build \
  -f container/Dockerfile \
  --build-arg "VERSION=$version" \
  --build-arg "COMMIT=$commit" \
  -t "$product_image" \
  . \
  || die "could not build the image under test from this working tree." \
         "Everything below tests that image, so there is nothing to fall back to: a published tag would test somebody else's build, which is #342."

step "building the two throwaway machine images"
# The source machine is the SFTP fixture's own image plus iptables, which
# is what lets the connection-cap case impose the production rule without
# the container being privileged.
docker build -q -t "$source_image" - >/dev/null <<DOCKERFILE || die "could not build the source machine image from $source_base."
FROM $source_base
RUN apk add --no-cache iptables
DOCKERFILE
# The manager machine is docker-in-docker plus python3, because the real
# installer is a Python script and this test runs the real installer.
# Nothing else: it is meant to be a box with Docker and nothing on it, and
# in particular no checkout of this repository (#346).
docker build -q -t "$machine_image" - >/dev/null <<DOCKERFILE || die "could not build the manager machine image from $machine_base."
FROM $machine_base
RUN apk add --no-cache python3
DOCKERFILE
note "source machine:  $source_image"
note "manager machine: $machine_image"

step "saving the image under test, to move it into the manager machine's own daemon"
mkdir -p "$run_dir"
docker save "$product_image" -o "$run_dir/product-image.tar" \
  || die "docker save $product_image failed."
note "$(du -h "$run_dir/product-image.tar" | awk '{print $1}') of image to move per case"

# ------------------------------------------------------- the source's keys
#
# The SOURCE machine's own host keys, generated fresh per run and removed
# at teardown, exactly as core/tests/sftpfixture does. The CLIENT key is
# not here: the installer generates that one now (#347) and this script
# takes the public half from its output, which is the sequence a real
# operator follows.
step "generating the source machine's host keys"
mkdir -p "$run_dir/upload"
# Two host keys, not one, for the reason core/tests/sftpfixture spells
# out: an SSH client negotiates a host-key algorithm by its OWN preference
# order, so pinning only ed25519 against a server offering both can still
# end up negotiating RSA and failing verification.
ssh-keygen -q -t ed25519 -N '' -C 'e2e host key' -f "$run_dir/ssh_host_ed25519_key"
ssh-keygen -q -t rsa -b 2048 -N '' -C 'e2e host key' -f "$run_dir/ssh_host_rsa_key"

# --------------------------------------------------------------- payload
#
# Three artifacts rather than one, deliberately. The leak the
# connection-cap case pins is per OPERATION, so one artifact's worth of
# work does not reach the ceiling: it takes several stats and copies
# before the third pool exists to be refused.
step "seeding the source machine's payload"
# Deterministic bytes rather than /dev/urandom, so a failure is
# reproducible and a digest mismatch can be reasoned about.
head -c 3145728 /dev/zero | openssl enc -aes-256-ctr -pbkdf2 -pass pass:rclone-manager-e2e-356 -nosalt 2>/dev/null > "$run_dir/upload/payload.bin" \
  || die "could not generate the payload."
printf 'CREATE TABLE artifacts (id text primary key);\n' > "$run_dir/upload/schema.sql"
printf 'issue 356: two machines, one temporary network, one real backup.\n' > "$run_dir/upload/notes.txt"
chmod 644 "$run_dir/upload/payload.bin" "$run_dir/upload/schema.sql" "$run_dir/upload/notes.txt"
chmod 777 "$run_dir/upload"
note "3 artifacts: payload.bin (3 MiB), schema.sql, notes.txt"

# ================================================================= a case

run_case() {
  local case_name="$1"
  local net="rm-e2e-net-$run_id-$case_name"
  local src="rm-e2e-source-$run_id-$case_name"
  local mgr="rm-e2e-manager-$run_id-$case_name"
  local case_dir="$run_dir/$case_name"

  step "case: $case_name"
  mkdir -p "$case_dir/authorized_keys"
  created_case_dirs+=("$case_dir")

  # ---------------------------------------------------- the network
  docker network create --label "$label" "$net" >/dev/null \
    || die "could not create the temporary network $net."
  created_networks+=("$net")
  note "temporary network $net"

  # --------------------------------------------- the manager machine
  docker run -d \
    --name "$mgr" \
    --network "$net" \
    --network-alias manager \
    --label "$label" \
    --privileged \
    -e DOCKER_TLS_CERTDIR= \
    "$machine_image" >/dev/null \
    || die "could not start the manager machine."
  created_containers+=("$mgr")

  wait_or_die 180 "the manager machine's own Docker daemon to come up" \
    docker exec "$mgr" docker info
  note "manager machine $mgr has Docker $(docker exec "$mgr" docker version --format '{{.Server.Version}}') and nothing else"

  # Nothing is installed yet, and the test says so rather than assuming
  # it: the GIVEN is a machine with Docker and no rclone-manager.
  if [ -n "$(docker exec "$mgr" docker ps -aq)" ]; then
    die "the manager machine already has containers on it, so it is not the fresh machine this test needs."
  fi

  # ------------------------------- move the image under test across
  step "  moving the image under test into the manager machine"
  # `docker save` piped into the manager machine's own `docker load`.
  # Saved once, above, rather than re-serialised per case: the tar is the
  # same bytes either way and the pipe is the same mechanism.
  docker exec -i "$mgr" docker load < "$run_dir/product-image.tar" \
    || die "could not move $product_image into the manager machine's daemon."
  docker exec "$mgr" docker image inspect "$product_image" >/dev/null \
    || die "$product_image is not on the manager machine after the load."
  note "$product_image is on the manager machine, and no registry was involved"

  # The installer, and NOTHING else. No checkout on this machine at all,
  # which is #346's actual criterion: the canonical compose has to come
  # from the copy embedded in the installer, because there is no
  # container/compose.yaml here to read.
  docker exec "$mgr" mkdir -p /opt/rm
  docker cp "$repo_root/scripts/install/install_docker_host.py" "$mgr:/opt/rm/install_docker_host.py" >/dev/null

  local prefix install_out
  case "$case_name" in
    no-arguments)
      # #347: `install` with NO arguments. The only reason this can be
      # hermetic is that the image is already on the machine under the
      # installer's own default reference, so preflight finds it present
      # and never reaches for a registry. What the engine then reports is
      # what proves which build actually got installed.
      docker exec "$mgr" docker tag "$product_image" "$default_image"
      # #346: nothing but the installer is on this machine. Say so out
      # loud rather than trusting the setup above to have stayed true.
      if docker exec "$mgr" sh -c 'ls /opt/rm' | grep -qv '^install_docker_host.py$'; then
        die "the manager machine has more than the installer on it, so this case is not proving #346's claim."
      fi
      # A checkout would be a container/compose.yaml, a go.mod or a
      # scripts/ tree. Searched where one could plausibly be rather than
      # over the whole filesystem, because /var/lib/docker holds the
      # unpacked layers of the image under test and a find over those
      # would answer a different question.
      if [ -n "$(docker exec "$mgr" sh -c 'find /opt /root /srv /home /usr/local -maxdepth 4 \( -name compose.yaml -o -name go.mod \) 2>/dev/null | head -1')" ]; then
        die "there is a checkout of this repository on the manager machine, so a no-checkout install is not what this case would be measuring."
      fi
      step "  installing with NO arguments at all (#347), from one copied file with no checkout (#346)"
      install_out="$(mgr_install "$mgr" 2>&1)" \
        || { echo "$install_out" >&2; die "the installer refused or failed with no arguments on a machine with nothing pre-existing." \
             "That is #347's whole criterion, and this is the run that was never performed before."; }
      prefix="/root/rclone-manager"
      ;;
    lifecycle)
      docker exec "$mgr" docker tag "$product_image" "$lifecycle_from"
      docker exec "$mgr" docker tag "$product_image" "$lifecycle_to"
      step "  installing $lifecycle_from"
      install_out="$(mgr_install "$mgr" --image "$lifecycle_from" --no-pull --timeout 240 2>&1)" \
        || { echo "$install_out" >&2; die "the installer refused or failed on the manager machine."; }
      prefix="/root/rclone-manager"
      ;;
    *)
      # The ordinary route: an explicit image, and the canonical compose
      # file copied in from a checkout, which is the other half of #346's
      # --compose-file contract.
      docker cp "$repo_root/container/compose.yaml" "$mgr:/opt/rm/compose.yaml" >/dev/null
      step "  installing with scripts/install/install_docker_host.py"
      install_out="$(mgr_install "$mgr" \
        --prefix /opt/rm/deploy \
        --compose-file /opt/rm/compose.yaml \
        --image "$product_image" \
        --no-pull \
        --timeout 240 2>&1)" \
        || { echo "$install_out" >&2; die "the installer refused or failed on the manager machine." \
             "That is the installer's own verdict on a machine with Docker and the image already loaded."; }
      prefix="/opt/rm/deploy"
      ;;
  esac
  echo "$install_out" | sed 's/^/       /'

  # ------------------------------------------------ what got installed
  #
  # #342: a stale --image default installed 0.1.0 and the installer said
  # "Installed." So the version the engine actually reports has to be the
  # version that was asked for, and this asks it rather than trusting the
  # tag. In the no-arguments case this is the ONLY thing standing between
  # a hermetic run and a silently published release.
  local reported
  reported="$(bm "$mgr" "$prefix" version)"
  echo "$reported" | sed 's/^/       /'
  echo "$reported" | grep -q "^backup-manager $version\$" \
    || die "the installed engine reports a different version from the one that was installed." \
           "asked for: $version" \
           "reported:  $(echo "$reported" | head -1)" \
           "This is #342's shape: a reference that installs something other than what was requested, while reporting success."
  echo "$reported" | grep -q "^commit $commit\$" \
    || die "the installed engine reports a different commit from the one that was built." \
           "asked for: $commit" \
           "reported:  $(echo "$reported" | grep '^commit ' || true)"
  note "the engine reports the version and commit that were installed"

  # ------------------------------------ the key the installer generated
  #
  # #347 again, from the other end: the installer generates the keypair
  # and prints the public half because nothing can be pulled until it is
  # on the machine being backed up. So this takes it out of the
  # installer's own output and puts it there, which is both the operator's
  # next step and the proof that what was printed is usable.
  local pubkey
  pubkey="$(echo "$install_out" | grep -oE 'ssh-ed25519 [A-Za-z0-9+/=]+( [^ ]*)?' | head -1 || true)"
  if [ -z "$pubkey" ]; then
    # Only the cases that let the installer default --ssh-key get one, and
    # today that is all of them. If that ever stops being true this says
    # so rather than silently authorising nothing.
    die "the installer did not print a generated public key, so there is nothing to authorise on the source machine." \
        "#347's own criterion is that a no-argument install produces a usable keypair and says what to do with it."
  fi
  printf '%s\n' "$pubkey" > "$case_dir/authorized_keys/installer.pub"
  note "authorising the key the installer generated: ${pubkey:0:40}..."

  # ---------------------------------------------- the source machine
  docker run -d \
    --name "$src" \
    --network "$net" \
    --network-alias source \
    --label "$label" \
    --cap-add NET_ADMIN \
    -v "$run_dir/ssh_host_ed25519_key:/etc/ssh/ssh_host_ed25519_key:ro" \
    -v "$run_dir/ssh_host_ed25519_key.pub:/etc/ssh/ssh_host_ed25519_key.pub:ro" \
    -v "$run_dir/ssh_host_rsa_key:/etc/ssh/ssh_host_rsa_key:ro" \
    -v "$run_dir/ssh_host_rsa_key.pub:/etc/ssh/ssh_host_rsa_key.pub:ro" \
    -v "$case_dir/authorized_keys:/home/$sftp_user/.ssh/keys:ro" \
    -v "$run_dir/upload:/home/$sftp_user/upload" \
    "$source_image" "$sftp_user::$sftp_uid:$sftp_uid:upload" >/dev/null \
    || die "could not start the source machine."
  created_containers+=("$src")

  wait_or_die 120 "the source machine's sshd to start listening" \
    docker exec "$src" sh -c 'nc -w 2 127.0.0.1 22 </dev/null 2>/dev/null | grep -q ^SSH-'
  note "source machine $src is serving SSH, authorising the installer's key"

  local source_ip
  source_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$src")"
  [ -n "$source_ip" ] || die "the source machine has no address on $net."
  note "source machine address on the temporary network: $source_ip"

  # The digests every assertion below is against, read from the SOURCE,
  # not from the copy this script wrote. What has to match is the bytes
  # the machine being backed up is actually serving.
  local want_payload want_schema want_notes
  want_payload="$(sha256_of "$src" "/home/$sftp_user/upload/payload.bin")"
  want_schema="$(sha256_of "$src" "/home/$sftp_user/upload/schema.sql")"
  want_notes="$(sha256_of "$src" "/home/$sftp_user/upload/notes.txt")"
  note "source payload.bin sha256 $want_payload"

  # ------------------------------------------- the production rule
  if [ "$case_name" = "connection-cap" ]; then
    # `! -i lo` so the readiness probe above (which dials 127.0.0.1 from
    # inside this same container) can never eat one of the two slots the
    # manager machine is meant to have.
    docker exec "$src" iptables -A INPUT ! -i lo -p tcp --syn --dport 22 \
      -m connlimit --connlimit-above "$connection_cap" --connlimit-mask 32 \
      -j REJECT --reject-with tcp-reset \
      || cannot_run "this kernel will not install an iptables connlimit rule inside the source container." \
                    "The connection-cap case models #264 by refusing a third simultaneous connection from one address," \
                    "and without the rule it would silently become a second copy of the plain case: green, and proving nothing." \
                    "Run with --case plain if this machine cannot carry the rule, and know that #264's shape is then untested."
    # Proven, not assumed. Hold two connections open from a throwaway
    # container on this network and check the third is refused, so a rule
    # that installs and does not bite cannot make the case vacuous.
    docker run --rm --network "$net" --label "$label" "$source_base" sh -c "
      (sleep 20 | nc $source_ip 22 >/dev/null 2>&1) &
      (sleep 20 | nc $source_ip 22 >/dev/null 2>&1) &
      sleep 3
      if nc -w 3 $source_ip 22 </dev/null >/dev/null 2>&1; then exit 1; fi
      exit 0
    " || die "the connection cap did not bite: a third simultaneous connection to the source machine was accepted." \
             "This case is only worth running if the cap is real. Without it, it is the plain case wearing a different name."
    note "connection cap proven: the source machine refuses a third simultaneous connection from one address"
  fi

  # ------------------------------------ create the set, through the CLI
  step "  creating a backup set through the CLI"
  bm "$mgr" "$prefix" backup-set create e2e/source \
    --config /etc/backup-manager/config \
    --host "$source_ip" \
    --user "$sftp_user" \
    --ssh-key-file /etc/backup-manager/id_ed25519 \
    --trust-host-key \
    --remote-path /upload \
    --local-path /data/backups/source \
    --completion-strategy rename \
    --read-only \
    --state-database /data/state/state.db \
    || die "creating the backup set through the CLI failed."

  # ------------------------------------------------------ run it
  step "  running the backup set"
  bm "$mgr" "$prefix" run --config /etc/backup-manager/config \
    || die "the backup cycle exited non-zero." \
           "$( [ "$case_name" = connection-cap ] && echo "This is the case that pins #264: the source refuses a third simultaneous connection, and a manager that leaks a connection pool per operation cannot get past it." || true )"

  # --------------------------------------------------- assert the bytes
  step "  checking the artifact against the source, by digest"
  local backups="$prefix/backups/source"
  local landed
  landed="$(docker exec "$mgr" sh -c "ls -1 $backups 2>/dev/null" || true)"
  note "in $backups: $(echo "$landed" | tr '\n' ' ')"

  local name want got
  for pair in "payload.bin:$want_payload" "schema.sql:$want_schema" "notes.txt:$want_notes"; do
    name="${pair%%:*}"
    want="${pair##*:}"
    docker exec "$mgr" test -f "$backups/$name" \
      || die "$name never landed on the manager machine." \
             "The backup directory holds: $(echo "$landed" | tr '\n' ' ')"
    got="$(sha256_of "$mgr" "$backups/$name")"
    [ "$got" = "$want" ] \
      || die "$name landed with different bytes from the source's." \
             "source:  $want" \
             "manager: $got" \
             "A file that exists is not a backup. This is the assertion #264 needed and no suite had."
    note "$name matches the source: $want"
  done

  # --read-only was passed, so the source has to come out of this
  # untouched. It is the posture a VPS being backed up actually wants
  # (issue #282, "pull from here, never delete here"), and asserting it
  # here is what makes that a proven property of a real run rather than a
  # flag nobody watched work.
  for pair in "payload.bin:$want_payload" "schema.sql:$want_schema" "notes.txt:$want_notes"; do
    name="${pair%%:*}"
    want="${pair##*:}"
    got="$(sha256_of "$src" "/home/$sftp_user/upload/$name")"
    [ "$got" = "$want" ] \
      || die "the source machine's own $name changed during the backup, and this set is read-only."
  done
  note "the source machine is untouched"

  # The product's own verdict, on top of the bytes: a set whose artifacts
  # all landed and verified is HEALTHY, and `status` exits non-zero on
  # anything else (FR-24).
  bm "$mgr" "$prefix" status --config /etc/backup-manager/config \
    || die "the engine's own status says this backup set is not healthy, even though the bytes match."

  if [ "$case_name" = "lifecycle" ]; then
    run_lifecycle "$mgr" "$prefix" "$want_payload"
  fi

  step "  case $case_name passed"

  # Released here rather than left to the exit trap. Each manager machine
  # is a whole Docker daemon with the image under test loaded into it and
  # two product containers running, and the Docker VM this was written on
  # has under 4 GiB: four cases holding on to all of that at once is how a
  # run starts failing for reasons that are about the machine rather than
  # about the product. The trap still names them, so a case that dies
  # before reaching this line is still cleaned up, and --keep-on-failure
  # still keeps what failed.
  release_case "$net" "$src" "$mgr"
}

# release_case removes one finished case's containers and network. Safe to
# run twice: the exit trap will try again on everything, and `docker rm` on
# something already gone is not an error worth reporting.
release_case() {
  local net="$1"; shift
  for c in "$@"; do
    docker rm -f "$c" >/dev/null 2>&1 || true
  done
  # After the containers, never before: a network with an endpoint on it
  # cannot be removed.
  docker network rm "$net" >/dev/null 2>&1 || true
}

# ==================================== #343: upgrade, then factory reset
#
# Two of #343's acceptance criteria are written in deliberately
# anti-assertion language, and both are answered here rather than by a
# unit test: "proven by counting them before and after rather than by
# assertion", and "proven by the fresh install issuing an enrollment
# link". A mocked Docker cannot produce either, which is why the issue was
# reopened.
run_lifecycle() {
  local mgr="$1" prefix="$2" want_payload="$3"

  # The positive control for everything below, and #343's own observable
  # read in the one state where it is unambiguous: this install has no
  # administrator yet, so it MUST be issuing an enrollment link. Without
  # this, the assertion after the factory reset would also pass against a
  # log grep that never matches anything.
  step "  a fresh install with no administrator issues an enrollment link"
  wait_or_die 60 "the fresh install to issue its enrollment link" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' logs rclone-manager 2>/dev/null | grep -q 'no administrator account exists yet'"
  note "it does"

  # ------------------------------------------------ give it a user
  #
  # There has to BE an administrator for an upgrade to preserve and for a
  # factory reset to destroy. `auth create-admin` is the product's own
  # way to make one without a browser, and it refuses while the server
  # holds the store (a process-lifetime advisory flock), so the engine
  # comes down for the length of it.
  step "  creating an administrator, so there is a user to count"
  mgr_compose "$mgr" "$prefix" stop rclone-manager >/dev/null 2>&1
  printf '%s' "$lifecycle_admin_pass" | docker exec -i "$mgr" docker compose \
    -p rclone-manager --env-file "$prefix/.env" \
    -f "$prefix/compose.yaml" -f "$prefix/compose.image.yaml" \
    run --rm --no-deps -T rclone-manager \
    /backup-manager-web auth create-admin --username "$lifecycle_admin_user" --password-stdin \
    || die "could not create an administrator on the installed instance."
  mgr_compose "$mgr" "$prefix" start rclone-manager >/dev/null
  # On the engine answering, not on the record existing: `run --rm` wrote
  # that file before this line was reached, so waiting on it would wait
  # for nothing and the next command would race the restart.
  wait_or_die 180 "the engine to answer again after the administrator was created" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' exec -T rclone-manager /backup-manager version"

  # ---------------------------------------------------- count, before
  local users_before sets_before artifacts_before
  users_before="$(count_admins "$mgr" "$prefix")"
  sets_before="$(count_sets "$mgr" "$prefix")"
  artifacts_before="$(count_artifacts "$mgr" "$prefix")"
  note "before the upgrade: $users_before administrator(s), $sets_before backup set(s), $artifacts_before catalogued artifact(s)"
  [ "$users_before" = "1" ] || die "the lifecycle case did not end up with exactly one administrator to preserve (got $users_before)."
  [ "$sets_before" = "1" ] || die "the lifecycle case did not end up with exactly one backup set to preserve (got $sets_before)."
  [ "$artifacts_before" -ge 3 ] || die "the lifecycle case did not end up with the three catalogued artifacts to preserve (got $artifacts_before)."

  # -------------------------------------------------------- upgrade
  step "  upgrading $lifecycle_from to $lifecycle_to (#343)"
  local upgrade_out
  upgrade_out="$(mgr_install "$mgr" --mode upgrade --image "$lifecycle_to" --no-pull --timeout 240 2>&1)" \
    || { echo "$upgrade_out" >&2; die "--mode upgrade failed on a real install."; }
  echo "$upgrade_out" | sed 's/^/       /'
  echo "$upgrade_out" | grep -q "0.2.0" \
    || die "the upgrade never reported the version that was already installed, which is one of #343's own criteria."

  # ----------------------------------------------------- count, after
  local users_after sets_after artifacts_after
  users_after="$(count_admins "$mgr" "$prefix")"
  sets_after="$(count_sets "$mgr" "$prefix")"
  artifacts_after="$(count_artifacts "$mgr" "$prefix")"
  note "after the upgrade:  $users_after administrator(s), $sets_after backup set(s), $artifacts_after catalogued artifact(s)"
  [ "$users_after" = "$users_before" ] \
    || die "the upgrade did not preserve every user: $users_before before, $users_after after."
  [ "$sets_after" = "$sets_before" ] \
    || die "the upgrade did not preserve every backup set: $sets_before before, $sets_after after."
  [ "$artifacts_after" = "$artifacts_before" ] \
    || die "the upgrade did not preserve every catalogued artifact: $artifacts_before before, $artifacts_after after."
  note "the upgrade preserved every user, backup set and catalogued artifact, counted rather than asserted"

  # An upgrade that kept the administrator must NOT be issuing an
  # enrollment link. This is the control for the factory-reset assertion
  # below: without it, "the log has an enrollment link in it" would pass
  # against an install that always prints one.
  # The log read here is the NEW container's: the upgrade changed the
  # image reference, so compose recreated the engine rather than
  # restarting it, and this log starts at the upgraded instance's own
  # first boot. That is what makes the absence meaningful rather than an
  # artefact of when the line was printed.
  if engine_log "$mgr" "$prefix" | grep -q "no administrator account exists yet"; then
    die "the upgraded instance is issuing an enrollment link, so the administrator record did not survive the upgrade."
  fi
  note "and it issues no enrollment link, because the administrator is still there"

  # -------------------------------------------------- factory reset
  step "  factory-resetting (#343)"
  local reset_out
  reset_out="$(mgr_install "$mgr" --mode factory-reset --confirm-factory-reset \
    --image "$lifecycle_to" --no-pull --timeout 240 2>&1)" \
    || { echo "$reset_out" >&2; die "--mode factory-reset failed on a real install."; }
  echo "$reset_out" | sed 's/^/       /'
  echo "$reset_out" | grep -q "1 administrator account" \
    || die "factory-reset did not say it was about to destroy the administrator account, by name and count."

  if docker exec "$mgr" test -f "$prefix/state/local-auth.json"; then
    die "the administrator record is still on disk after a factory reset."
  fi
  note "state/local-auth.json is gone"

  # The criterion itself: the resulting install has to ISSUE AN
  # ENROLLMENT LINK, which is the only observable that says the
  # administrator record went with the database rather than being left
  # behind for the engine to find.
  wait_or_die 120 "the reset instance to issue a fresh enrollment link (#343)" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' logs rclone-manager 2>/dev/null | grep -q 'no administrator account exists yet'"
  note "the fresh install issues an enrollment link: $(engine_log "$mgr" "$prefix" | grep -o 'Open http[^ ]*' | tail -1)"

  # And the retained backups are still on disk, because a factory reset
  # drops the catalog that describes them and never the files themselves.
  local still
  still="$(sha256_of "$mgr" "$prefix/backups/source/payload.bin")"
  [ "$still" = "$want_payload" ] \
    || die "the retained backup did not survive the factory reset, which destroys the catalog and not the files."
  note "the retained backups are untouched"
}

count_admins() {  # 1 when the administrator record exists, 0 otherwise
  if docker exec "$1" test -f "$2/state/local-auth.json"; then echo 1; else echo 0; fi
}

count_sets() {  # backup sets the engine reports, through its own read surface
  # `sources` prints one indented line per backup set, each carrying
  # remote_path=. Counting that rather than the indentation, because the
  # indentation is a format and the field is a fact.
  bm "$1" "$2" sources --config /etc/backup-manager/config | grep -c 'remote_path=' || true
}

count_artifacts() {  # catalogued artifacts, through the engine's own read surface
  # `artifacts` ends with its own "N artifact(s)" line, which is the
  # engine counting its catalog rather than this script counting lines it
  # happens to recognise.
  bm "$1" "$2" artifacts --config /etc/backup-manager/config \
    | sed -n 's/^\([0-9][0-9]*\) artifact(s)$/\1/p' | tail -1
}

engine_log() {
  docker exec "$1" docker compose -p rclone-manager --env-file "$2/.env" \
    -f "$2/compose.yaml" -f "$2/compose.image.yaml" logs rclone-manager 2>/dev/null || true
}

for one in $case_list; do
  run_case "$one"
done

step "every case passed"
echo "    A fresh install on a throwaway machine pulled a backup off another throwaway"
echo "    machine over a temporary network, and the bytes match."
