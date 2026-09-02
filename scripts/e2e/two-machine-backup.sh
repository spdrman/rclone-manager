#!/usr/bin/env bash
# Two throwaway machines, a temporary network, and one real backup
# (issue #356).
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
#   * a SOURCE machine: a real sshd (atmoz/sftp, the same image
#     core/tests/sftpfixture uses) holding a payload of known content,
#     playing the VPS being backed up;
#   * a MANAGER machine: docker-in-docker, so it is a box with Docker and
#     nothing else, playing the NAS;
#   * a temporary network joining exactly those two.
#
# Then it installs rclone-manager onto the manager machine with
# scripts/install/install_docker_host.py, the real installer, creates a
# backup set through the CLI, runs it, and compares the artifact's SHA-256
# against the source's. Not "the file is there": the bug this was written
# after (#264) was a transfer that failed while everything around it
# looked healthy, so the assertion is on the bytes.
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
# --image is passed a tag built from THIS working tree and moved into the
# manager machine's own daemon with `docker save | docker load`. So the
# run proves this code rather than whatever is published, and it needs no
# registry. #342 exists because a stale published default installed 0.1.0
# and reported success, so this also asserts that the version the engine
# reports is the version that was asked for.
#
# # The case that matters most
#
# --case connection-cap models what actually broke on real hardware: both
# production sources carry an iptables rule rejecting a third simultaneous
# SSH connection from one address with a TCP reset. rclone-manager failed
# against it because every operation built its own Fs and nothing released
# one, so the pools accumulated until the third was refused. Listing
# succeeded and the transfer got "connection refused". The source machine
# here carries that same rule (`-m connlimit`, which needs NET_ADMIN and
# not privileged), so the case goes red against that behaviour and green
# against the fix.
#
# sshd's own MaxStartups was the cheaper thing to try and it does not
# model this: it bounds connections that have not finished authenticating,
# and a leaked pool's connections are authenticated and idle, so they
# never count. connlimit counts established connections per source
# address, which is the production rule restated.
#
# # Hygiene
#
# Every container and the network are torn down on success, on failure and
# on interrupt. Every name carries a per-run id, and no host port is
# published at all, so two of these can run at once. Every wait has a
# deadline and names what it was waiting for: an end-to-end test that
# hangs forever on a cold machine is worse than one that fails.
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
      sed -n '2,80p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) die "unknown option $1" "Usage: $0 [--case plain|connection-cap|all] [--keep-on-failure]" ;;
  esac
done

case "$cases" in
  all) case_list="plain connection-cap" ;;
  plain|connection-cap) case_list="$cases" ;;
  *) die "unknown case $cases" "Cases are: plain, connection-cap, all." ;;
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

# Where this run's throwaway key material and payload live. Inside the
# working tree, gitignored, for the reason scripts/e2e/run-tests-repo-gate.sh
# gives for its own scratch: nothing this repository's tooling creates
# should need a recursive delete outside the workspace to clean up after
# itself. The files below are removed by name at teardown.
run_dir="$repo_root/.e2e-two-machine/$run_id"

# The images. The product image is per-run, because it is built from this
# working tree and two runs may be testing different trees. The two
# machine images are fixed and tiny derivations of upstream images, so
# they are shared and Docker's layer cache makes a rebuild instant.
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

# ------------------------------------------------------------- teardown
#
# One trap covering EXIT, INT and TERM. Everything created is registered
# here the moment it exists, so an interrupt between two steps still tears
# down what the earlier one made.
created_containers=()
created_networks=()
teardown_done=0
last_case_failed=0

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
  rm -f \
    "$run_dir/ssh_host_ed25519_key" "$run_dir/ssh_host_ed25519_key.pub" \
    "$run_dir/ssh_host_rsa_key" "$run_dir/ssh_host_rsa_key.pub" \
    "$run_dir/id_ed25519" "$run_dir/id_ed25519.pub" \
    "$run_dir/known_hosts" \
    "$run_dir/authorized_keys/id_ed25519.pub" \
    "$run_dir/upload/payload.bin" "$run_dir/upload/schema.sql" "$run_dir/upload/notes.txt" \
    "$run_dir/product-image.tar" 2>/dev/null || true
  rmdir "$run_dir/authorized_keys" "$run_dir/upload" "$run_dir" 2>/dev/null || true
  rmdir "$repo_root/.e2e-two-machine" 2>/dev/null || true
}

# ------------------------------------------------------------- utilities

# wait_until <seconds> <what it is waiting for> -- <command...>
# Every wait in this script goes through here, so none of them can be the
# one that hangs a cold machine forever, and every timeout says what it
# was waiting for rather than only that it waited.
wait_until() {
  local budget="$1"; shift
  local what="$1"; shift
  [ "$1" = "--" ] && shift
  local deadline=$(( $(date +%s) + budget ))
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      return 1
    fi
    sleep 1
  done
  return 0
}

wait_or_die() {
  local budget="$1" what="$2"
  shift 2
  wait_until "$budget" "$what" "$@" \
    || die "timed out after ${budget}s waiting for $what."
}

sha256_of() {  # sha256_of <container> <path>
  docker exec "$1" sha256sum "$2" | awk '{print $1}'
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
# Nothing else: it is meant to be a box with Docker and nothing on it.
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

# ------------------------------------------------------- key material
#
# Generated fresh per run and removed at teardown, exactly as
# core/tests/sftpfixture does: nothing here is a real credential, and a
# key that lives longer than the container it authorises is a key somebody
# eventually has to reason about.
step "generating this run's throwaway keys"
mkdir -p "$run_dir/authorized_keys" "$run_dir/upload"
# Two host keys, not one, for the reason core/tests/sftpfixture spells
# out: an SSH client negotiates a host-key algorithm by its OWN preference
# order, so pinning only ed25519 against a server offering both can still
# end up negotiating RSA and failing verification.
ssh-keygen -q -t ed25519 -N '' -C 'e2e host key' -f "$run_dir/ssh_host_ed25519_key"
ssh-keygen -q -t rsa -b 2048 -N '' -C 'e2e host key' -f "$run_dir/ssh_host_rsa_key"
ssh-keygen -q -t ed25519 -N '' -C 'e2e client key' -f "$run_dir/id_ed25519"
cp "$run_dir/id_ed25519.pub" "$run_dir/authorized_keys/id_ed25519.pub"
chmod 600 "$run_dir/id_ed25519"
chmod 777 "$run_dir/upload"

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
note "3 artifacts: payload.bin (3 MiB), schema.sql, notes.txt"

# ================================================================= a case

run_case() {
  local case_name="$1"
  local net="rm-e2e-net-$run_id-$case_name"
  local src="rm-e2e-source-$run_id-$case_name"
  local mgr="rm-e2e-manager-$run_id-$case_name"

  step "case: $case_name"

  # ---------------------------------------------------- the network
  docker network create --label "$label" "$net" >/dev/null \
    || die "could not create the temporary network $net."
  created_networks+=("$net")
  note "temporary network $net"

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
    -v "$run_dir/authorized_keys:/home/$sftp_user/.ssh/keys:ro" \
    -v "$run_dir/upload:/home/$sftp_user/upload" \
    "$source_image" "$sftp_user::$sftp_uid:$sftp_uid:upload" >/dev/null \
    || die "could not start the source machine."
  created_containers+=("$src")

  wait_or_die 120 "the source machine's sshd to start listening" \
    docker exec "$src" sh -c 'nc -w 2 127.0.0.1 22 </dev/null 2>/dev/null | grep -q ^SSH-'
  note "source machine $src is serving SSH"

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

  # ------------------------------------------- what the installer needs
  #
  # The installer never generates or reads a key: it takes host PATHS and
  # nothing else, so the machine has to have them already, exactly as a
  # real NAS would.
  docker exec "$mgr" mkdir -p /opt/rm/secrets
  docker cp "$repo_root/scripts/install/install_docker_host.py" "$mgr:/opt/rm/install_docker_host.py" >/dev/null
  docker cp "$repo_root/container/compose.yaml" "$mgr:/opt/rm/compose.yaml" >/dev/null
  docker cp "$run_dir/id_ed25519" "$mgr:/opt/rm/secrets/id_ed25519" >/dev/null
  # chown as well as chmod. `docker cp` carries the source file's numeric
  # owner across, so the key arrives owned by whichever uid this developer
  # happens to be, and the engine container runs with `cap_drop: ALL`:
  # root without CAP_DAC_OVERRIDE cannot read a 0600 file it does not own,
  # which presents as a flat "permission denied" on the key. That is the
  # container's hardening working, not a problem with it.
  docker exec "$mgr" chown 0:0 /opt/rm/secrets/id_ed25519
  docker exec "$mgr" chmod 600 /opt/rm/secrets/id_ed25519
  # A real file, and empty of trust on purpose: the per-backup-set
  # known_hosts the engine writes lives in its own known_hosts.d beside
  # config.yaml, which is where a set created through the API or the CLI
  # puts it. This mount is the hand-maintained one from docs/ssh-setup.md,
  # and this deployment does not use it.
  docker exec "$mgr" sh -c 'printf "# this deployment trusts host keys per backup set, in known_hosts.d\n" > /opt/rm/secrets/known_hosts'

  # ------------------------------------------------- the real installer
  step "  installing with scripts/install/install_docker_host.py"
  docker exec "$mgr" python3 /opt/rm/install_docker_host.py install \
    --prefix /opt/rm/deploy \
    --compose-file /opt/rm/compose.yaml \
    --ssh-key /opt/rm/secrets/id_ed25519 \
    --known-hosts /opt/rm/secrets/known_hosts \
    --image "$product_image" \
    --no-pull \
    --timeout 240 \
    || die "the installer refused or failed on the manager machine." \
           "That is the installer's own verdict on a machine with Docker, the canonical compose file and the image already loaded."

  # ------------------------------------------------ what got installed
  #
  # #342: a stale --image default installed 0.1.0 and the installer said
  # "Installed." So the version the engine actually reports has to be the
  # version that was asked for, and this asks it rather than trusting the
  # tag.
  local reported
  reported="$(mgr_compose "$mgr" exec -T rclone-manager /backup-manager version)"
  echo "$reported" | sed 's/^/       /'
  echo "$reported" | grep -q "^backup-manager $version\$" \
    || die "the installed engine reports a different version from the one that was installed." \
           "asked for: $version" \
           "reported:  $(echo "$reported" | head -1)" \
           "This is #342's shape: a tag that installs something other than what was requested, while reporting success."
  echo "$reported" | grep -q "^commit $commit\$" \
    || die "the installed engine reports a different commit from the one that was built." \
           "asked for: $commit" \
           "reported:  $(echo "$reported" | grep '^commit ' || true)"
  note "the engine reports the version and commit that were installed"

  # ------------------------------------ create the set, through the CLI
  step "  creating a backup set through the CLI"
  mgr_compose "$mgr" exec -T rclone-manager /backup-manager backup-set create e2e/source \
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
  mgr_compose "$mgr" exec -T rclone-manager /backup-manager run \
    --config /etc/backup-manager/config \
    || die "the backup cycle exited non-zero." \
           "$( [ "$case_name" = connection-cap ] && echo "This is the case that pins #264: the source refuses a third simultaneous connection, and a manager that leaks a connection pool per operation cannot get past it." || true )"

  # --------------------------------------------------- assert the bytes
  step "  checking the artifact against the source, by digest"
  local backups="/opt/rm/deploy/backups/source"
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
  mgr_compose "$mgr" exec -T rclone-manager /backup-manager status \
    --config /etc/backup-manager/config \
    || die "the engine's own status says this backup set is not healthy, even though the bytes match."

  step "  case $case_name passed"
}

# mgr_compose runs a compose command on the manager machine's own daemon,
# against the deployment the installer staged there. The argument list is
# the installer's own (scripts/install/install_docker_host.py's
# compose_argv), so this drives exactly the deployment it produced rather
# than a second idea of one.
mgr_compose() {
  local mgr="$1"; shift
  docker exec "$mgr" docker compose \
    -p rclone-manager \
    --env-file /opt/rm/deploy/.env \
    -f /opt/rm/deploy/compose.yaml \
    -f /opt/rm/deploy/compose.image.yaml \
    "$@"
}

for one in $case_list; do
  run_case "$one"
done

step "every case passed"
echo "    A fresh install on a throwaway machine pulled a backup off another throwaway"
echo "    machine over a temporary network, and the bytes match."
