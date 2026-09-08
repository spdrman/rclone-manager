#!/usr/bin/env bash
# HELP-START
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
#   activity-diagnostic
#                   #598, and the only case here whose subject is a
#                   FAILURE rather than a backup. Three claims, in the
#                   order they matter.
#
#                   First, that the lifecycle feed is readable at all
#                   against a real deployment: `backup-manager activity`
#                   with the route configured announces
#                   `mode: engine-attached`, which means it asked the
#                   engine over HTTP, and it lists the transitions the
#                   cycle above actually produced, named and timestamped.
#                   That is the assertion that would have caught the
#                   reported bug, and no suite had it: the browser suite
#                   drives a mock through a dev server, so it proves a
#                   component renders rather than that the product works.
#
#                   Second, that a read which reaches no service says so.
#                   With the engine container stopped, the same command
#                   must never claim `engine-attached`, and must name the
#                   world it answered from instead.
#
#                   Third, and this is the whole point of #598's server
#                   half: a 500 the service cannot explain to its caller
#                   has to explain itself in its own log. The
#                   configuration directory is made read-only, a routed
#                   `settings patch` is refused with an INTERNAL and a
#                   correlation id, and `docker logs` on the manager
#                   machine has to carry a line with THAT id and the
#                   underlying error on it. Before this, thirty 500 sites
#                   in the web host discarded the error and there was
#                   nowhere to write it, so the id an operator was handed
#                   matched nothing anywhere.
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
#   retention-apply #602, and the only place in this repository where a
#                   retention plan is applied against restore points a
#                   real cycle produced on a real machine. It backs up,
#                   narrows the chain so today's three artifacts do not
#                   all survive it, fingerprints the whole backup
#                   directory, applies the plan through
#                   `retention apply --acknowledge`, and requires the
#                   difference between the two fingerprints to be exactly
#                   the set the plan printed as DELETE.
#
#                   The file called unmanaged-by-anything.txt is the
#                   point. No journal row mentions it, so it has to
#                   survive, and the comparison is then run twice more
#                   against deliberately perturbed listings and required
#                   to complain about each: one where that file went
#                   missing anyway, and one where a previewed DELETE is
#                   still there. Without those, an apply that deleted
#                   nothing at all passes every other assertion here.
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
# HELP-END
set -euo pipefail

# --help reads this file, and the run cd's to the repository root two lines
# below, so a $0 the shell left relative stops resolving once it does. Both
# paths are settled here, from the same dirname, before that happens.
self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
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
# pass either. The caller reads that verdict as exit 3, the same status
# scripts/lib/ci-local-gate.sh uses for INCOMPLETE, so it can tell "could
# not run it" from "ran it and it failed" without parsing prose.
#
# Which is exactly why the verdict does not travel through this script AS
# 3. The CLI under test now has its own meaning for that status (#551:
# another process is already serving this deployment), and every `bm` call
# below runs that CLI under `set -euo pipefail`. One unguarded call
# meeting that refusal would end this script with the number that means
# "this machine cannot perform the proof": ci-local.sh would ledger it,
# the run would end INCOMPLETE, and .husky/pre-commit lets INCOMPLETE
# commit. The one test anywhere that proves a backup can be pulled off a
# real machine would have failed and been read as a machine that never
# tried.
#
# Nothing reaches that today. Every `bm` that can meet the refusal is
# guarded with `|| die`, and the one unguarded command substitution runs
# `version`, which cannot return 3. But that is safety by review, where it
# used to be safety by construction, because the CLI had no 3 at all. So
# the verdict gets a number nothing else here produces, and `finish` below
# is the single place either number is spoken to the caller.
EXIT_CANNOT_RUN=97
cannot_run() {
  echo "" >&2
  echo "==> two-machine: CANNOT RUN. $1" >&2
  shift
  for line in "$@"; do echo "    $line" >&2; done
  exit "$EXIT_CANNOT_RUN"
}

# ------------------------------------------------------------------ help
#
# --help prints the header block between the two markers at the top of
# this file, and deliberately not a range of line numbers (#514). The old
# form was `sed -n '2,110p' "$0"`, so the help an operator read was a set of
# coordinates rather than a piece of text: inserting a comment above the
# boundary rewrote it, deleting one truncated it, and nothing anywhere
# would have noticed either. Both had already happened by the time #514
# was written. Markers move with the text they delimit, and
# scripts/tests/e2e-help.test.sh pins the rendered result, so a reword is
# a decision now rather than an accident.
render_help() {
  awk '
    /^# HELP-END$/   { closed = 1; inside = 0; next }
    /^# HELP-START$/ { opened = 1; inside = 1; next }
    inside           { sub(/^# ?/, ""); print }
    END              { if (!opened || !closed) exit 1 }
  ' "$self" || die "the help block is missing from $self" \
    "--help renders the lines between the HELP-START and HELP-END markers," \
    "and this file has lost one or both of them."
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
      render_help
      exit 0 ;;
    *) die "unknown option $1" "Usage: $0 [--case plain|no-arguments|connection-cap|activity-diagnostic|lifecycle|retention-apply|all] [--keep-on-failure]" ;;
  esac
done

case "$cases" in
  all) case_list="plain no-arguments connection-cap activity-diagnostic lifecycle retention-apply" ;;
  plain|no-arguments|connection-cap|activity-diagnostic|lifecycle|retention-apply) case_list="$cases" ;;
  *) die "unknown case $cases" "Cases are: plain, no-arguments, connection-cap, activity-diagnostic, lifecycle, retention-apply, all." ;;
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

# The source machine's Dockerfile, shared with the Go machine tier. Its
# FROM line is $source_base; the constant above is kept because a couple of
# steps below run a throwaway container from the bare base image rather
# than from the built one.
source_dockerfile="$repo_root/scripts/e2e/source-machine.Dockerfile"
[ -r "$source_dockerfile" ] || die "the source machine Dockerfile is missing at $source_dockerfile." \
  "It is the one definition of the simulated VPS, shared with core/tests/machines, so there is nothing to build a source machine from without it."

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

# The administrator two cases create: the lifecycle case needs a user for
# the upgrade to preserve and the factory reset to destroy, and the
# activity-diagnostic case needs credentials for the CLI's route to the
# engine, which are the Web UI's own. A throwaway password for a container
# that is deleted minutes later. It leaves this script's process down a
# pipe into stdin when the account is created, and as an environment
# variable on the exec'd commands that use the route, which is what the
# CLI's own documentation prescribes; it is never written to a file.
admin_user="e2e-operator"
admin_pass="e2e-$run_id-not-a-real-password"

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
  local status="${1:-0}"
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
    [ -n "$c" ] && docker rm -fv "$c" >/dev/null 2>&1 || true
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
# Three traps, not one, and the difference is what makes the interrupt
# claim true rather than intended. A bash trap on INT or TERM runs the
# handler and then RESUMES the script, so `trap teardown EXIT INT TERM`
# would tear everything down and then carry on running against containers
# that are no longer there. These exit, which also fires the EXIT trap;
# teardown_done makes the second call a no-op.
#
# The status is passed in rather than read from $?, because inside a
# signal handler $? is the status of whatever command the signal
# interrupted and says nothing about why the script is ending.
#
# finish, rather than teardown itself, is what EXIT runs, because the
# status this script leaves with is a translation now and a translation
# belongs in one place (see EXIT_CANNOT_RUN above for why there is one).
#
#   EXIT_CANNOT_RUN  ->  3, the verdict ci-local.sh ledgers.
#   3                ->  1, because nothing here means 3, so a 3 arriving
#                        at this trap came from a command with its own
#                        meaning for it (the CLI's is "another process is
#                        already serving this deployment"). That is a
#                        failed proof, and it is reported as one instead
#                        of borrowing a verdict about the machine.
#   anything else    ->  itself, untouched.
finish() {
  local status="${1:-0}"
  teardown "$status"
  case "$status" in
    "$EXIT_CANNOT_RUN") exit 3 ;;
    3)
      echo "" >&2
      echo "==> two-machine: FAILED. A command exited 3." >&2
      echo "    This script reserves 3 for the gate's \"this machine could not perform the proof\" verdict and never" >&2
      echo "    produces it itself, so a 3 here came from something with its own meaning for that status: most" >&2
      echo "    likely the CLI refusing because another process is already serving the deployment (#551), from a" >&2
      echo "    bm call with no || die on it. Reported as a failure (1), which is what a proof that did not finish" >&2
      echo "    is. The run above says which step it died on." >&2
      exit 1 ;;
    *) exit "$status" ;;
  esac
}
trap 'finish $?' EXIT
trap 'teardown 130; exit 130' INT
trap 'teardown 143; exit 143' TERM

# -v on every removal, and it is not decoration. docker:28-dind declares
# VOLUME /var/lib/docker, so every manager machine this script starts
# creates an anonymous host volume holding that machine's whole inner
# Docker state, including the image under test after it is loaded in.
# `docker rm -f` leaves that volume behind: measured directly, starting one
# manager machine takes the host from 44 volumes to 45 and `docker rm -f`
# leaves it at 45, while `docker rm -fv` returns it to 44. Four cases a run
# across several lanes is how a shared Docker disk fills, and it fills
# invisibly, because nothing references the leftovers. This script found
# that out the honest way: the installer's own preflight refused a case
# with "the filesystem holding /opt/rm/deploy/backups has 912 MiB free,
# and this refuses below 2048 MiB", which was the correct verdict on a
# Docker VM at 98%.
#
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
    "$run_dir/product-image.tar" "$run_dir/probe-image.tar" 2>/dev/null || true
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

# bm runs the real CLI inside the engine container, so its exit status is
# the CLI's own. That includes the 3 issue #551 gave it (another process
# is already serving this deployment), which is the status this script
# reserves for the gate's "could not run" verdict: see EXIT_CANNOT_RUN
# above for what keeps the two apart, and scripts/tests/two-machine-exit-status.test.sh
# for the proof that it does.
bm() {  # bm <mgr> <prefix> <backup-manager args...>
  local mgr="$1" prefix="$2"; shift 2
  mgr_compose "$mgr" "$prefix" exec -T rclone-manager /backup-manager "$@"
}

# ------------------------------------------------------------- preflight

step "preflight"
for tool in docker ssh-keygen openssl; do
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

# The same treatment for the installer's network probe (#271), and for a
# sharper reason. That probe needs a container with a shell, ping and nc,
# and the installer PULLS one when the host has none. A manager machine is
# a fresh daemon holding nothing, so without this every case reaches for
# Docker Hub in the middle of an install, and a rate limit or a dropped
# connection there fails a run that has nothing to do with either. Not
# hypothetical: a full gate run died on
# `Get "https://registry-1.docker.io/...": EOF` with everything else green.
#
# Read rather than restated, like the reference above, so the tag this
# preloads cannot drift from the tag the installer would ask for.
probe_base="$(python3 - <<'PY'
import re, sys
src = open("scripts/install/install_docker_host.py", encoding="utf-8").read()
m = re.search(r'"--probe-image",\s*default="([^"]+)"', src)
if not m:
    sys.exit(1)
print(m.group(1))
PY
)" || die "could not read the installer's own --probe-image default out of scripts/install/install_docker_host.py." \
         "Without it this cannot preload the probe, and every case would pull it from a registry mid-install."

# Checked here rather than at save time, so a host with no copy and no
# route to a registry finds out in seconds instead of after the image
# under test has been built. cannot_run, not die: that host cannot perform
# this proof, which is the same verdict as no Docker and no privileged
# containers, and the gate ledgers it rather than reporting a pass for a
# run that never happened.
if ! docker image inspect "$probe_base" >/dev/null 2>&1; then
  docker pull "$probe_base" >/dev/null 2>&1 \
    || cannot_run "the installer's network probe needs $probe_base, this host has no copy, and it could not be pulled." \
                  "Pull it once by hand and re-run, or every case will try to reach a registry from inside its own install."
fi
note "the installer's --probe-image default is $probe_base, and this host has it"

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
# The source machine comes from scripts/e2e/source-machine.Dockerfile,
# which is the one definition of the simulated VPS (#451). The Go machine
# tier builds the same file through core/tests/machines, so "the source
# machine" means one thing in this repository rather than two that agree
# until they do not.
docker build -q -t "$source_image" -f "$source_dockerfile" "$(dirname "$source_dockerfile")" >/dev/null \
  || die "could not build the source machine image from $source_dockerfile."
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

step "saving the images to move into the manager machine's own daemon"
mkdir -p "$run_dir"
docker save "$product_image" -o "$run_dir/product-image.tar" \
  || die "docker save $product_image failed."
note "$(du -h "$run_dir/product-image.tar" | awk '{print $1}') of image under test to move per case"

docker save "$probe_base" -o "$run_dir/probe-image.tar" \
  || die "docker save $probe_base failed."
note "the probe image travels with the run, so no case reaches a registry"

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
# </dev/null on both, so a run_dir that somehow already holds these keys
# (an explicit E2E_RUN_ID reused after a --keep-on-failure run) fails
# rather than sitting on ssh-keygen's "Overwrite (y/n)?" prompt forever.
# Every wait in this script is bounded; a prompt is an unbounded one.
ssh-keygen -q -t ed25519 -N '' -C 'e2e host key' -f "$run_dir/ssh_host_ed25519_key" </dev/null \
  || die "could not generate the source machine's ed25519 host key at $run_dir." \
         "If this run reused an E2E_RUN_ID, that directory already has one: choose another id."
ssh-keygen -q -t rsa -b 2048 -N '' -C 'e2e host key' -f "$run_dir/ssh_host_rsa_key" </dev/null \
  || die "could not generate the source machine's RSA host key at $run_dir."

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

# run_source_verification is issue #624 end to end: an SSH source is
# proven before this manager relies on it, or it is marked as never having
# been proven.
#
# It runs against the two machines this script already has standing, in the
# window where the engine is stopped, so every command below takes the
# direct path. It has to: a configuration write beside a serving engine is
# refused with exit 3 (#571), and `backup-set test-connection` goes through
# the same door because a check that PASSES clears the mark, which is a
# configuration write.
#
# The failing source is the real machine under a username its sshd has
# never heard of, rather than an address nothing answers. Both fail, and
# only one of them fails the way the issue is about: the host resolves, the
# TCP connect succeeds, the host key matches what the set trusts, and then
# the account cannot authenticate. An unreachable address would prove the
# check runs and would not prove it can tell those apart.
#
# The order is load bearing, and it cost a run to learn. OpenSSH penalises
# a source address for a few seconds after an authentication failure, so
# the check that has to SUCCEED goes first and every deliberate failure
# after it. For the same reason nothing here probes for a host key: the
# anchor the create above established is reused, so this block makes
# exactly the connections it is asserting about and no others.
#
# Everything this creates is removed before it returns, because the rest of
# the case asserts on artifacts, health and retention for e2e/source alone
# and a second set left behind would be a second set in every one of those
# answers.
run_source_verification() {  # <mgr> <prefix> <source-ip> <sftp-user>
  local mgr="$1" prefix="$2" source_ip="$3" sftp_user="$4"

  # The configuration file, read off the manager machine rather than
  # through the CLI, because the mark is a durable fact about that file and
  # a command's own report of it is the thing under test.
  config_yaml() { docker exec "$mgr" sh -c "cat '$prefix'/config/*.yaml"; }

  # The trust anchor the main create already established, reused rather
  # than re-probed. Not a shortcut: probing is a real SSH connection, and
  # OpenSSH penalises a source address for a few seconds after an
  # authentication failure, so a probe standing between two deliberate
  # auth failures fails with a bare EOF and this block would be flaky
  # about something it is not testing. Everything below therefore makes
  # exactly the connections it is asserting about and no others.
  #
  # Globbed rather than named, because the filename is core/service's own
  # (source and set folded into one token) and encoding that rule here
  # would be a second copy of it. Exactly one backup set exists at this
  # point, so exactly one file is there, and the count is asserted rather
  # than assumed.
  local trusted lines
  trusted="$(docker exec "$mgr" sh -c "cat '$prefix'/config/known_hosts.d/*" | grep -v '^$' || true)"
  lines="$(printf '%s\n' "$trusted" | grep -c . || true)"
  [ "$lines" = "1" ] \
    || die "expected exactly one trusted host-key line beside the configuration, found $lines." \
           "This block reuses the anchor the create above established rather than probing again," \
           "so it needs to know which line that is."

  local base=(--config /etc/backup-manager/config
    --host "$source_ip"
    --ssh-key-file /etc/backup-manager/id_ed25519
    --known-hosts-line "$trusted"
    --remote-path /upload
    --completion-strategy rename
    --state-database /data/state/state.db)

  # The working source first, and every failing one after it. Order, not
  # taste: an authentication failure earns the manager's address a short
  # penalty on the source, and a check that has to SUCCEED must not be
  # standing behind one.
  step "  #624: --no-verify writes a set without proving it, and marks it"
  mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
    /backup-manager backup-set create e2e/offline "${base[@]}" \
    --user "$sftp_user" --local-path /data/backups/offline --no-verify \
    || die "\`backup-set create --no-verify\` failed against a source it was told not to check."
  config_yaml | grep -q 'connection_unverified: true' \
    || die "a --no-verify create left no mark, so it is indistinguishable from one that was proven." \
           "the configuration is: $(config_yaml)"
  note "written, and the configuration says its connection was never proven"

  step "  #624: a check that passes clears the mark"
  local out=""
  out="$(bm_stopped "$mgr" "$prefix" backup-set test-connection e2e/offline --config /etc/backup-manager/config 2>&1)" \
    || die "\`backup-set test-connection\` against the source this script has been backing up all along failed." \
           "the command said: $out"
  for want in credentials resolve connect host_key authenticate list; do
    case "$out" in
      *"$want"*) : ;;
      *) die "the passing check does not report the $want step: $out" ;;
    esac
  done
  if config_yaml | grep -q 'connection_unverified: true'; then
    die "a passing check left the set marked as never proven." \
        "the configuration is: $(config_yaml)"
  fi
  note "six steps reported, and the mark is gone"

  step "  #624: a create against a source that cannot authenticate is refused"
  local refused=0
  out="$(mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
    /backup-manager backup-set create e2e/nobody "${base[@]}" \
    --user "nobody-$run_id" --local-path /data/backups/nobody 2>&1)" || refused=$?
  [ "$refused" = "1" ] \
    || die "a create against a source that cannot authenticate exited $refused, want 1." \
           "Before #624 this exited 0 and wrote the set: nothing on the create path ran a check at all." \
           "the command said: $out"
  case "$out" in
    *authenticate*) : ;;
    *) die "the refusal does not report the authenticate step, so it is a verdict rather than a diagnosis (#596)." \
           "the command said: $out" ;;
  esac
  if config_yaml | grep -q 'id: nobody'; then
    die "a refused create still wrote the backup set into the configuration."
  fi
  note "refused with exit 1, and the configuration does not carry it"

  step "  #624: a check that fails leaves the mark where it was"
  mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
    /backup-manager backup-set create e2e/nobody "${base[@]}" \
    --user "nobody-$run_id" --local-path /data/backups/nobody --no-verify \
    || die "\`backup-set create --no-verify\` failed for the unreachable set."
  refused=0
  out="$(bm_stopped "$mgr" "$prefix" backup-set test-connection e2e/nobody --config /etc/backup-manager/config 2>&1)" || refused=$?
  [ "$refused" = "1" ] \
    || die "\`backup-set test-connection\` against a source that cannot authenticate exited $refused, want 1." \
           "the command said: $out"
  # Exactly one mark, not "the bad set still has one": the proven set has
  # to have LOST its mark and the unproven one has to have kept it, and a
  # build that cleared every mark it could find would pass a check that
  # only looked at one of them.
  local marks
  marks="$(config_yaml | grep -c 'connection_unverified: true' || true)"
  [ "$marks" = "1" ] \
    || die "the configuration carries $marks unverified marks, want exactly 1." \
           "The proven set has to lose its mark and the unproven one has to keep it." \
           "the configuration is: $(config_yaml)"
  note "still marked, which is what stops the mark meaning \"somebody pressed the button\""

  # Removed before the engine comes back, so the rest of this case still
  # asserts about one backup set.
  local id
  for id in e2e/nobody e2e/offline; do
    mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
      /backup-manager backup-set remove "$id" --config /etc/backup-manager/config >/dev/null \
      || die "could not remove $id, so the rest of this case would be asserting about three backup sets."
  done
}

# bm_stopped runs the CLI while the engine is stopped, which is the world
# every configuration write in this script performs its writes in. `bm`
# above uses `compose exec`, which needs a running container; this uses
# `compose run --rm`, which starts one for the command and takes it away
# again.
bm_stopped() {  # bm_stopped <mgr> <prefix> <backup-manager args...>
  local mgr="$1" prefix="$2"; shift 2
  mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager /backup-manager "$@"
}

# run_case is one whole proof, from a machine with nothing on it to a
# byte-for-byte comparison of what landed.
#
# Everything a case needs is created inside it and named with this run's
# id, so two cases never share a network, a container or a directory, and
# a case is released at the end rather than at the exit trap. That is not
# tidiness: a manager machine is a Docker daemon with the image under test
# loaded into it and two product containers running, and holding four of
# those at once is how a run starts failing for reasons that are about the
# host rather than about the product.
#
# The order of what it asserts is the argument. The machine is checked
# empty before anything installs, because "installed on a fresh machine"
# is the claim. The version the ENGINE reports is checked against the
# version that was asked for, because a stale default once installed
# something else and said "Installed." The bytes are compared against
# digests read from the SOURCE rather than from what this script wrote,
# because what has to match is what the machine being backed up is
# actually serving. And the source is re-hashed afterwards, because the
# set was created read-only and a backup that modifies what it pulls from
# is the failure nobody would look for.
#
# The case name selects the variation rather than a separate function,
# and each variation says in its own branch what it is for.
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
  # The probe image with it, so the install below never reaches for a
  # registry. Asserted present rather than assumed loaded, because a
  # missing one does not fail here: it fails several minutes later, inside
  # the installer, as a pull that times out.
  docker exec -i "$mgr" docker load < "$run_dir/probe-image.tar" \
    || die "could not move $probe_base into the manager machine's daemon."
  docker exec "$mgr" docker image inspect "$probe_base" >/dev/null \
    || die "$probe_base is not on the manager machine after the load, so the installer's network probe would pull it."
  note "$product_image and $probe_base are on the manager machine, and no registry was involved"

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
  #
  # Two steps rather than one, and issue #571 is the reason. A packaged
  # install that has never been configured is still SERVING: the engine
  # container announces the journal `--state-database` names before it
  # puts up the first-run setup flow, so a create typed beside it is
  # refused with nothing written, exactly as the second create on this
  # machine would be. This used to be the one configuration write that got
  # through, and what it left behind was a CLI holding a backup set the
  # running engine had never heard of, which is #535 on a fresh install.
  #
  # So the refusal is asserted first, because it is the property, and then
  # the create is performed the way the refusal tells an operator to
  # perform it: with the engine down. That is the same shape `auth
  # create-admin` needs in run_lifecycle below, for the same kind of
  # reason, and it is the honest worked example for a packaged install.
  local create_argv=(backup-set create e2e/source
    --config /etc/backup-manager/config
    --host "$source_ip"
    --user "$sftp_user"
    --ssh-key-file /etc/backup-manager/id_ed25519
    --trust-host-key
    --remote-path /upload
    --local-path /data/backups/source
    --completion-strategy rename
    --read-only
    --state-database /data/state/state.db)

  step "  a create typed beside the serving engine is refused (#571)"
  local refusal="" refused=0
  refusal="$(bm "$mgr" "$prefix" "${create_argv[@]}" 2>&1)" || refused=$?
  [ "$refused" = "3" ] \
    || die "a first \`backup-set create\` typed beside the engine exited $refused, want 3." \
           "3 is the status #551 reserves for a write refused because another process is serving this deployment," \
           "and #571 is the case where that process is a fresh install still on its setup flow. A 0 here means the" \
           "configuration was written underneath an engine that will never read it, which is #535." \
           "the command said: $refusal"
  # And nothing was written. `sources` needs a configuration to read, so on
  # an installation that still has none it refuses, and a zero here would
  # mean the refused create left one behind after all.
  if bm "$mgr" "$prefix" sources --config /etc/backup-manager/config >/dev/null 2>&1; then
    die "the refused create left a configuration behind, so \"nothing was written\" is not true on a real install."
  fi
  note "refused with exit 3, and this installation still has no configuration"

  step "  creating a backup set through the CLI, with the engine stopped"
  mgr_compose "$mgr" "$prefix" stop rclone-manager >/dev/null 2>&1
  mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
    /backup-manager "${create_argv[@]}" \
    || die "creating the backup set through the CLI failed."
  # Issue #624, in the one window where it can be proven: the engine is
  # stopped, so every configuration write and every check below takes the
  # direct path, which is the same path the create above just took.
  if [ "$case_name" = "plain" ]; then
    run_source_verification "$mgr" "$prefix" "$source_ip" "$sftp_user"
  fi

  mgr_compose "$mgr" "$prefix" start rclone-manager >/dev/null
  # On the engine answering, not on the file existing: `run --rm` wrote it
  # before this line was reached, so waiting on the file would wait for
  # nothing and the next step would race the restart.
  wait_or_die 180 "the engine to answer again after the backup set was created" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' exec -T rclone-manager /backup-manager version"

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

  if [ "$case_name" = "retention-apply" ]; then
    run_retention_apply "$mgr" "$prefix"
  fi
  if [ "$case_name" = "activity-diagnostic" ]; then
    run_activity_diagnostic "$mgr" "$prefix"
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
    docker rm -fv "$c" >/dev/null 2>&1 || true
  done
  # After the containers, never before: a network with an endpoint on it
  # cannot be removed.
  docker network rm "$net" >/dev/null 2>&1 || true
}

# ============================= #598: the feed reads, and a 500 says why
#
# Everything above this point is about a backup arriving. This is about
# what an operator is told when something does not, and it is here rather
# than in a unit test because both halves of #598 are about wiring that
# only exists in a real deployment.
#
# The browser suite over in rclone-manager-tests drives createMockApi
# through a Vite dev server. That is worth having and it is structurally
# incapable of catching what was reported: the mock resolved every read
# cleanly, so every case in it is a claim about a component rendering. The
# claims below are made against the image built from this tree, running as
# two containers on a machine that had nothing on it, over the same HTTP
# route the browser uses.
#
# The route is the reason there is an administrator here at all. The CLI
# reaches the engine through three environment variables (see `usage()`),
# and the two credential ones are the Web UI's own, so a routed read is
# an authenticated HTTP request to /api/v1/activity: the same request,
# through the same middleware, answered by the same handler. That makes
# the CLI the honest stand-in for the browser on a machine with no
# browser on it, and it is why the mode line is asserted first. A command
# that quietly answered from the journal would satisfy every assertion
# about the rows and prove nothing about the route.
run_activity_diagnostic() {
  local mgr="$1" prefix="$2"

  # ---------------------------------------- somebody to authenticate as
  #
  # Same dance as run_lifecycle, for the same reason: `auth create-admin`
  # holds the credential store under a process-lifetime advisory flock, so
  # the engine comes down for the length of it. The password never leaves
  # this script except down a pipe into stdin here, and as an environment
  # variable on the exec'd commands below, which is what the CLI's own
  # documentation prescribes for the route; the container it lives in is
  # deleted minutes from now.
  step "  creating an administrator, so the CLI has a route to authenticate on"
  mgr_compose "$mgr" "$prefix" stop rclone-manager >/dev/null 2>&1
  printf '%s' "$admin_pass" | docker exec -i "$mgr" docker compose \
    -p rclone-manager --env-file "$prefix/.env" \
    -f "$prefix/compose.yaml" -f "$prefix/compose.image.yaml" \
    run --rm --no-deps -T rclone-manager \
    /backup-manager-web auth create-admin --username "$admin_user" --password-stdin \
    || die "could not create an administrator on the installed instance."
  mgr_compose "$mgr" "$prefix" start rclone-manager >/dev/null
  wait_or_die 180 "the engine to answer again after the administrator was created" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' exec -T rclone-manager /backup-manager version"

  # ------------------------------------------------- the feed, routed
  step "  the lifecycle feed, read from the engine over its own API (#598)"
  local feed_out feed_err
  feed_err="$case_dir/activity.err"
  feed_out="$(bm_routed "$mgr" "$prefix" activity --config /etc/backup-manager/config 2>"$feed_err")" \
    || die "\`backup-manager activity\` failed against a deployment that has just completed a backup." \
           "stderr: $(cat "$feed_err")"

  grep -q '^mode: engine-attached' "$feed_err" \
    || die "the activity read did not go to the engine." \
           "It announced: $(head -1 "$feed_err")" \
           "A read that answered from this host's own journal would satisfy every assertion below about the rows" \
           "and prove nothing about the route the browser uses, which is what #598 is about."
  note "the read announced engine-attached, so it came back over /api/v1/activity"

  # The transitions the cycle above actually produced, named. Not "some
  # rows": these three files are the ones whose bytes were compared
  # against the source a moment ago, so a feed that listed anything else
  # would be describing a different deployment.
  local name
  for name in payload.bin schema.sql notes.txt; do
    echo "$feed_out" | grep -q "$name" \
      || die "the activity feed does not mention $name, which this run backed up and verified by digest." \
             "the feed said:" "$feed_out"
  done
  echo "$feed_out" | grep -q 'COMMITTED' \
    || die "the activity feed carries no COMMITTED transition, and this run committed three artifacts." \
           "the feed said:" "$feed_out"
  # Timestamped, which is half of what the page shows and the half a
  # projection silently drops.
  echo "$feed_out" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} ' \
    || die "no row in the activity feed carries a timestamp." \
           "the feed said:" "$feed_out"
  note "the feed names all three artifacts, their COMMITTED transitions, and when each happened"

  # --json is what a support conversation or a cron job parses, so it is
  # asserted on the contract's field names rather than on the table.
  local json_out
  json_out="$(bm_routed "$mgr" "$prefix" activity --config /etc/backup-manager/config --limit 1 --json 2>/dev/null)" \
    || die "\`backup-manager activity --json\` failed against the running engine."
  case "$json_out" in
    *'"events"'*'"artifact_id"'*'"occurred_at"'*) : ;;
    *) die "--json did not emit the contract's own ListActivityResponse shape." "it emitted: $json_out" ;;
  esac
  note "--json emits the wire objects, field names and all"

  # -------------------------------- a read that reaches no service
  #
  # #598's second browser claim, in the form a machine with no browser can
  # make it: with the engine down, the command must never CLAIM the world
  # it could not reach. Run through `run --rm --no-deps` because there is
  # no engine container left to exec into, which is itself the situation
  # being modelled.
  step "  a read with no engine to reach never claims it reached one"
  mgr_compose "$mgr" "$prefix" stop rclone-manager >/dev/null 2>&1
  local down_err="$case_dir/activity-engine-down.err"
  docker exec "$mgr" docker compose -p rclone-manager --env-file "$prefix/.env" \
    -f "$prefix/compose.yaml" -f "$prefix/compose.image.yaml" \
    run --rm --no-deps -T \
    -e BACKUP_MANAGER_API_URL=http://127.0.0.1:8080 \
    -e BACKUP_MANAGER_API_USERNAME="$admin_user" \
    -e BACKUP_MANAGER_API_PASSWORD="$admin_pass" \
    rclone-manager \
    /backup-manager activity --config /etc/backup-manager/config >/dev/null 2>"$down_err" \
    || true
  if grep -q 'mode: engine-attached' "$down_err"; then
    die "the read announced engine-attached with the engine container stopped." \
        "it said: $(head -1 "$down_err")" \
        "Claiming a world it could not reach is the defect #536 gave the mode line to prevent, and #598 asks for the same honesty one surface over."
  fi
  grep -q '^mode: ' "$down_err" \
    || die "the read with no engine to reach announced no mode at all, so nothing on screen says which world the answer is about." \
           "it said: $(cat "$down_err")"
  note "it announced: $(grep -m1 '^mode: ' "$down_err" | cut -c1-60)..."
  mgr_compose "$mgr" "$prefix" start rclone-manager >/dev/null
  wait_or_die 180 "the engine to answer again" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' exec -T rclone-manager /backup-manager version"

  # ------------------- a 500 an operator can quote, and a log that has it
  #
  # The clause the server half of #598 exists for. There were thirty
  # `writeError(w, 500, "INTERNAL", ...)` sites in the web host, every one
  # of them binding the error, testing it and dropping it, with nowhere to
  # write it even if it had wanted to. So the frontend's own words for an
  # INTERNAL refusal, "its own log holds the detail, under this
  # correlation id", were untrue of every route in the product.
  #
  # A read-only configuration directory is the cheapest deterministic way
  # to make the engine refuse something it cannot explain: the write fails
  # in the filesystem, well below anything that could produce a typed
  # reason, which is exactly the shape those thirty sites are for.
  step "  a 500 the engine cannot explain still explains itself in the log (#598)"
  docker exec "$mgr" chmod 0555 "$prefix/config" \
    || die "could not make the configuration directory read-only on the manager machine."

  local refusal="" refused=0
  refusal="$(bm_routed "$mgr" "$prefix" settings patch --timezone Europe/Berlin --config /etc/backup-manager/config 2>&1)" || refused=$?
  docker exec "$mgr" chmod 0755 "$prefix/config" \
    || die "could not restore the configuration directory's permissions on the manager machine."

  [ "$refused" != "0" ] \
    || die "a configuration write against a read-only configuration directory succeeded, so there is no refusal to check." \
           "it said: $refusal"
  case "$refusal" in
    *INTERNAL*) : ;;
    *) die "the refusal is not the INTERNAL one this case is about, so the log assertion below would be about a different failure." \
           "it said: $refusal" ;;
  esac

  local cid
  cid="$(printf '%s' "$refusal" | grep -oE 'cid_[A-Za-z0-9_-]+' | head -1 || true)"
  [ -n "$cid" ] \
    || die "the refusal quoted no correlation id, so an operator has nothing to give anybody." \
           "it said: $refusal"
  note "the engine refused with correlation id $cid"

  local logs
  logs="$(mgr_compose "$mgr" "$prefix" logs rclone-manager 2>/dev/null || true)"
  printf '%s' "$logs" | grep -q "$cid" \
    || die "the correlation id $cid the operator was handed appears nowhere in the engine's own log." \
           "That is the whole defect #598's server half is about: an id that matches nothing sends whoever quotes it" \
           "grepping for a string that was never written."
  local line
  line="$(printf '%s' "$logs" | grep -m1 "$cid")"
  case "$line" in
    *http_refusal*) : ;;
    *) die "the line carrying $cid is not the refusal event, so something else happens to mention that id." "the line: $line" ;;
  esac
  case "$line" in
    *'"error"'*) : ;;
    *) die "the logged refusal carries the correlation id and not the error it refused over, which is the half that makes the id worth quoting." \
           "the line: $line" ;;
  esac
  case "$line" in
    *'/api/v1/settings'*) : ;;
    *) die "the logged refusal does not name the route it refused, so an operator holding the id still cannot say what failed." "the line: $line" ;;
  esac
  note "the engine's own log carries that id, the route, and the error underneath it"

  step "  activity-diagnostic passed"
}

# bm_routed is `bm` with the three environment variables that give the CLI
# a route to the running engine (see `usage()`'s own paragraph on them).
# Without these the same command answers from this host's journal and
# announces `direct`, which is a different claim entirely, so every routed
# assertion above checks the mode line before it checks anything else.
bm_routed() {  # bm_routed <mgr> <prefix> <backup-manager args...>
  local mgr="$1" prefix="$2"; shift 2
  # The -e flags go on `compose exec`, not on the `docker exec` around it:
  # the outer one would set them for the compose CLI running on the
  # manager machine, which is not the process that needs them.
  docker exec "$mgr" docker compose -p rclone-manager --env-file "$prefix/.env" \
    -f "$prefix/compose.yaml" -f "$prefix/compose.image.yaml" \
    exec -T \
    -e BACKUP_MANAGER_API_URL=http://127.0.0.1:8080 \
    -e BACKUP_MANAGER_API_USERNAME="$admin_user" \
    -e BACKUP_MANAGER_API_PASSWORD="$admin_pass" \
    rclone-manager /backup-manager "$@"
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
  printf '%s' "$admin_pass" | docker exec -i "$mgr" docker compose \
    -p rclone-manager --env-file "$prefix/.env" \
    -f "$prefix/compose.yaml" -f "$prefix/compose.image.yaml" \
    run --rm --no-deps -T rclone-manager \
    /backup-manager-web auth create-admin --username "$admin_user" --password-stdin \
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


# run_retention_apply is issue #602's whole claim, on restore points a real
# cycle produced on a real machine: an apply removes exactly the set the
# plan printed as DELETE, and nothing else.
#
# # Why the chain gets narrowed first
#
# One cycle lands three artifacts on one day, and every GFS tier keeps a
# representative of its newest bucket, so under the default chain all
# three are KEEP and the plan selects nothing. A plan that selects nothing
# makes every assertion below vacuously true, which is the exact shape
# this case exists to rule out, so the chain is narrowed to one per bucket
# through `backup-set retention` and the emptiness of the DELETE set is
# then checked rather than assumed.
#
# # Why the engine is stopped for it
#
# Not because the apply would be refused: it reads the configuration and
# writes the journal, so it is allowed beside a serving engine exactly as
# `restore` is. It is stopped because the poll loop can finish a cycle in
# the window between the preview and the apply, and a cycle writes the
# very journal rows the staleness comparison is computed over, so the
# apply would correctly refuse with RETENTION_PLAN_STALE and this case
# would be asserting on that refusal instead. That refusal is pinned where
# it belongs, at both boundaries, in core/service and apps/common/webhost.
# The engine comes back up afterwards and has to still call the set
# healthy.
#
# # The positive control
#
# unmanaged-by-anything.txt is planted in the backup directory and no
# journal row mentions it. FR-20 never lists a directory to find something
# to delete, so it has to survive. It is also what the comparison is
# proven on: after the real apply, retention_apply_complaints is run twice
# more against listings perturbed by hand, one with that file missing and
# one with a previewed DELETE still present, and it has to complain about
# each by name. Without that, an apply that deleted nothing at all passes
# every other line here.
run_retention_apply() {
  local mgr="$1" prefix="$2"
  local backups="$prefix/backups/source"

  step "  planting a file no journal row mentions"
  docker exec "$mgr" sh -c "printf 'no journal row mentions this\n' > $backups/unmanaged-by-anything.txt" \
    || die "could not plant the unmanaged file in $backups."

  # A whole policy, through the CLI, with the engine down: this is a
  # configuration write and #538 refuses one beside a serving engine, so
  # it follows the same stop/run --rm/start shape the create above does.
  step "  narrowing the chain so today's restore points do not all survive it"
  mgr_compose "$mgr" "$prefix" stop rclone-manager >/dev/null 2>&1
  mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
    /backup-manager backup-set retention e2e/source \
    --config /etc/backup-manager/config \
    --daily-days 1 --weekly-months 1 --monthly-months 1 >/dev/null \
    || die "giving the backup set its own retention policy failed."

  local before after apply_out want_deleted did_delete removed complaints
  before="$(retention_apply_listing "$mgr" "$backups")"
  note "before the apply: $(printf '%s\n' "$before" | awk '{print $1}' | tr '\n' ' ')"

  step "  applying the plan"
  apply_out="$(mgr_compose "$mgr" "$prefix" run --rm --no-deps -T rclone-manager \
    /backup-manager retention apply e2e/source \
    --config /etc/backup-manager/config --acknowledge 2>/dev/null)" \
    || die "the retention apply exited non-zero." "It said: $apply_out"
  printf '%s\n' "$apply_out" | sed 's/^/    | /'

  # The plan the apply printed BEFORE it did anything, which is the plan
  # it applied. Read off its own output rather than from a second preview:
  # a second preview is a second decision, and comparing the disk against
  # one nothing acted on would be comparing two different plans.
  want_deleted="$(printf '%s\n' "$apply_out" | awk '/: applied plan /{exit} $1 == "DELETE" { print $2 }' | sort)"
  did_delete="$(printf '%s\n' "$apply_out" | awk '/: applied plan /{on=1;next} on && $1 == "DELETE" { print $2 }' | sort)"

  [ -n "$want_deleted" ] \
    || die "the plan selected nothing for deletion, so this case certifies nothing." \
           "Every assertion below is vacuously true of an apply that removed nothing at all." \
           "The chain narrowed above is supposed to leave one representative per bucket out of three same-day artifacts." \
           "The apply said: $apply_out"
  [ "$want_deleted" = "$did_delete" ] \
    || die "the apply's own receipt names a different set from the plan it printed first." \
           "planned: $(echo "$want_deleted" | tr '\n' ' ')" \
           "applied: $(echo "$did_delete" | tr '\n' ' ')"
  note "the plan selected: $(echo "$want_deleted" | tr '\n' ' ')"

  after="$(retention_apply_listing "$mgr" "$backups")"
  note "after the apply:  $(printf '%s\n' "$after" | awk '{print $1}' | tr '\n' ' ')"

  complaints="$(retention_apply_complaints "$before" "$after" "$want_deleted")"
  [ -z "$complaints" ] \
    || die "the apply did not remove exactly the set the plan named:" "$complaints"

  removed="$(comm -23 <(printf '%s\n' "$before" | awk '{print $1}') <(printf '%s\n' "$after" | awk '{print $1}'))"
  note "removed exactly: $(echo "$removed" | tr '\n' ' ')"

  # The positive control. Everything above is a comparison, and a
  # comparison nobody has watched fail is indistinguishable from one that
  # cannot. Both perturbations are applied to the LISTING rather than to
  # the machine, because what is under test here is the assertion and a
  # control that deleted a real file would be testing rm.
  step "  proving that comparison would have noticed"
  local perturbed
  # `|| true` because grep exits 1 on an empty result and this whole
  # script runs under `set -e`. An empty listing here is not a silent
  # pass: the comparison below would then complain about every file in
  # the tree, the planted one included, so the control still fires.
  perturbed="$(printf '%s\n' "$after" | grep -v '^unmanaged-by-anything.txt ' || true)"
  retention_apply_complaints "$before" "$perturbed" "$want_deleted" \
    | grep -q 'unmanaged-by-anything.txt' \
    || die "the comparison reported nothing wrong about a file no verdict named going missing." \
           "It would therefore have passed against an apply that removed a file the journal never knew about," \
           "which makes every assertion in this case worthless."
  note "a file no verdict named going missing: caught"

  local survivor
  survivor="$(printf '%s\n' "$want_deleted" | head -1)"
  perturbed="$(printf '%s\n%s\n' "$after" "$(printf '%s\n' "$before" | grep "^$survivor ")" | grep -v '^$' | sort)"
  retention_apply_complaints "$before" "$perturbed" "$want_deleted" \
    | grep -q "$survivor" \
    || die "the comparison reported nothing wrong about a previewed DELETE still sitting on disk." \
           "It would therefore have passed against an apply that confirmed a plan and carried none of it out."
  note "a previewed DELETE still on disk: caught"

  retention_apply_complaints "$before" "$after" "" | grep -q 'certifies nothing' \
    || die "the comparison answered true for a plan that named nothing to delete, rather than refusing it."
  note "an empty plan: refused rather than answered true"

  # What the deployment looks like afterwards, and the one thing about it
  # that has to hold.
  #
  # It is not "healthy", and this case found that out rather than assuming
  # it: an apply removes the local file and leaves the journal row exactly
  # as it was, so the next reconciliation finds a REMOTE_RETAINED artifact
  # whose durable local copy is missing and quarantines it. The set then
  # reports DEGRADED and `status` exits non-zero, for a run that did
  # precisely what its plan said. That is issue #608 and it is not this
  # case's to fix: a pruned artifact needs an end state of its own, which
  # changes internal/state's schema.
  #
  # So the exit code is not gated on, and one thing is: nothing may be
  # reported as UNRECOVERABLE. The source here is read-only, so these
  # route to the ordinary recoverable QUARANTINED; the same code path
  # sends a COMPLETE artifact to QUARANTINED_LOST, which is the
  # unrecoverable one, and retention's own deliberate deletion being
  # recorded as unrecoverable data loss is the version of #608 that would
  # be an emergency rather than a defect. Pinning it here is what makes
  # that distinction something a run can lose rather than something a
  # reader has to remember.
  step "  bringing the engine back up"
  mgr_compose "$mgr" "$prefix" start rclone-manager >/dev/null
  wait_or_die 180 "the engine to answer again after the retention apply" \
    bash -c "docker exec '$mgr' docker compose -p rclone-manager --env-file '$prefix/.env' -f '$prefix/compose.yaml' -f '$prefix/compose.image.yaml' exec -T rclone-manager /backup-manager version"

  local health
  health="$(bm "$mgr" "$prefix" status --config /etc/backup-manager/config 2>/dev/null || true)"
  printf '%s\n' "$health" | sed 's/^/    | /'
  printf '%s\n' "$health" | grep -q "^e2e/source:" \
    || die "the engine's status says nothing about e2e/source after the retention apply." \
           "It said: $health"
  printf '%s\n' "$health" | grep -qE "unrecoverable: [1-9]" \
    && die "the engine reports an UNRECOVERABLE artifact after a retention apply that removed only what its own plan named." \
           "A deletion this product decided on, previewed and confirmed must never be recorded as data it has lost." \
           "It said: $health"
  note "nothing is reported as unrecoverable; the DEGRADED reading itself is issue #608"
}

# retention_apply_listing fingerprints one directory inside a container as
# "<name> <sha256>" lines, sorted.
#
# Names AND digests, because "present afterwards" is a weaker claim than
# "present and the same file", and the difference is the whole of what a
# restore point is for.
retention_apply_listing() {  # retention_apply_listing <container> <dir>
  docker exec "$1" sh -c "cd $2 && sha256sum * 2>/dev/null" | awk '{print $2, $1}' | sort
}

# retention_apply_complaints is the assertion, as a function that prints
# what is wrong rather than one that dies.
#
# That shape is the point, and it is the same one core/service's own
# retentionEvidenceCompare takes: a helper that called die could only ever
# be exercised by a run that was already failing, so nothing could ask it
# "would you notice". This one can be handed a deliberately perturbed
# listing and required to complain, which is what the control above does.
#
# An empty <previewed-delete-set> is refused rather than answered true
# about: with nothing named for deletion every clause here is vacuously
# satisfied by an apply that did nothing at all.
retention_apply_complaints() {  # retention_apply_complaints <before> <after> <previewed-delete-set>
  local before="$1" after="$2" want="$3"
  local name digest now_digest

  if [ -z "$want" ]; then
    echo "the plan named nothing for deletion, so this comparison certifies nothing"
    return 0
  fi

  while read -r name digest; do
    [ -z "$name" ] && continue
    now_digest="$(printf '%s\n' "$after" | awk -v n="$name" '$1 == n { print $2 }')"
    if [ -z "$now_digest" ]; then
      printf '%s\n' "$want" | grep -qxF -- "$name" \
        || echo "$name was removed and no verdict in the plan named it"
    elif printf '%s\n' "$want" | grep -qxF -- "$name"; then
      echo "$name is still on disk and the plan marked it DELETE"
    elif [ "$now_digest" != "$digest" ]; then
      echo "$name survived the apply but is not the same file"
    fi
  done <<LISTING
$before
LISTING

  while read -r name digest; do
    [ -z "$name" ] && continue
    printf '%s\n' "$before" | awk '{print $1}' | grep -qxF -- "$name" \
      || echo "$name appeared during the apply, and a retention apply creates nothing"
  done <<LISTING
$after
LISTING

  # Explicit, and not tidiness. A `while read` loop ends on the read that
  # found nothing, so this function would otherwise return 1 whenever it
  # had no complaint to make, which under `set -euo pipefail` kills the
  # run at the assignment above and fires every `|| die` in the control
  # below on a comparison that was perfectly happy.
  return 0
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

# engine_log is best-effort by design: it is only ever called to enrich a
# failure message, so a compose invocation that cannot reach the stack
# must not replace the real failure with its own.
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
