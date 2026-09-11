#!/usr/bin/env python3
"""The Go machine tier, run from inside a manager machine (#451). --help below."""
# HELP-START
# The Go machine tier, run from inside a manager machine (issue #451).
#
# Rom's ask for #447 was two containers on a dedicated network, one of them
# the backupd machine. scripts/e2e/two-machine-backup.sh makes that
# literally true for the installer. For the Go machine tier it was not: the
# test process runs on the host and reaches the source over a published
# loopback port, because on Docker Desktop for macOS a host process cannot
# sit on a bridge network. core/tests/machines was written with a seam for
# this (Source.Addr answers 127.0.0.1:<published> on the host and
# source:22 inside the network, chosen by RCLONE_MANAGER_MACHINES_NETWORK),
# and this script is the other side of it.
#
# What it stands up:
#
#   * a MANAGER machine: a Go toolchain with a docker client, this
#     repository mounted, and the Go build and module caches mounted, on
#     the dedicated network, playing the NAS;
#   * a dedicated NETWORK, labelled the way core/tests/dockerlease labels
#     everything so a killed run is reclaimed;
#   * the SOURCE machines, created by the harness inside the manager, one
#     per test, from scripts/e2e/source-machine.Dockerfile.
#
# Then it runs the machine-tier packages inside the manager with
# RCLONE_MANAGER_MACHINES_NETWORK set, so nothing publishes a port and every
# address a test uses is the address a real manager would use.
#
# It runs them under core/cmd/gotestwatch rather than under a bare
# `go test`, which is the same wrapper scripts/ci-local.sh puts them under
# and for the same reason (#256): a machine-tier package's wall clock tracks
# real machine load, and a fixed -timeout chosen on a quiet machine kills a
# run that is still making progress. Being a drop-in for that step is the
# point, so it takes --race too.
#
# # The docker socket question, answered
#
# The manager container gets the docker socket. There were two ways to do
# this and the other one does not work: the harness creates its machines
# per test, with per-test key material and per-test host keys, so a driver
# cannot pre-create them and hand them over. Anything short of the socket
# would mean rewriting the harness to take machines it did not make, which
# is the opposite of #447.
#
# two-machine-backup.sh refuses the socket for its own manager, and that
# refusal is still right there and does not apply here. Its manager runs
# the real INSTALLER, so a mounted socket would put the product's own
# containers on the developer's host and the installer would be installing
# onto the machine the script is running on, which is the one thing that
# test exists not to do. This manager runs the test harness. The harness is
# orchestration, not the product, and what this script is proving is where
# the manager sits on the network, not what its daemon can see.
#
# Two consequences worth knowing before reading a failure here. The
# containers the harness creates are SIBLINGS of the manager on the host's
# daemon rather than children of it, which is fine because they join the
# same network by name. And a bind mount the harness asks for is resolved
# by the host's daemon against the HOST's filesystem, which is why this
# script mounts the repository at the same absolute path inside the manager
# as it has outside: core/tests/.run/<test> then means one directory to
# both sides. Mount it anywhere else and the source machines come up with
# empty key directories and refuse every login.
#
# # Not root, and not opted out of
#
# core/internal/testenv refuses to run as root rather than skipping the
# permission-bit tests, which is deliberate (#456's shape: a skip deletes
# coverage from a run that goes on saying ok). So the manager runs as the
# invoking user's uid and gid. The docker socket is root:root 0660, so the
# container is given gid 0 as a SUPPLEMENTARY group, which is how
# docker-outside-of-docker has always granted socket access. euid is still
# not zero, so the refusal is satisfied honestly rather than opted out of.
#
# # Cost, measured on this machine rather than guessed
#
# On the 4 CPU / 4 GB Docker Desktop VM this gate runs against, arm64
# native, running every machine package:
#
#   warm, --race, under gotestwatch:  169s wall, 164s inside, compile 3s
#   the compile alone, both caches empty, --race:  45s
#
#   cold, --race, under gotestwatch: 217s wall, 169s inside, compile 45s
#
# and, measured before this took --race and gotestwatch, on four packages
# under a plain `go test`: 210s wall cold and 120s warm, so about 76s of
# compile.
#
# #451 asked for the cold figure because a cold compile of rclone's module
# graph took over six minutes in CI with no cache, and that is what the
# "run every test on two machines" option was rejected on. It does not
# reproduce here, for two reasons worth knowing before either number is
# quoted again: only the packages the tier imports get compiled, not the
# whole product, and this builds arm64 natively. DOCKER_DEFAULT_PLATFORM is
# linux/amd64 on this machine, and with it in force the manager was built
# emulated and the compile was measuring qemu, which is why the platform is
# named from the daemon's own architecture below.
#
# The 45 seconds is worth its own line, because it is exactly gotestwatch's
# unmeasured floor. A cold compile inside the watched window does not merely
# risk tripping the watchdog, it lands on the boundary, and under any load
# at all it goes over. That is why the compile is its own step above the
# watched run rather than inside it.
#
# The caches are volumes rather than being thrown away with the container
# precisely so the warm number is the one a gate pays.
#
# # Hygiene
#
# The manager and the network are torn down on success, on failure and on
# interrupt. Everything carries a per-run id, nothing publishes a host
# port, and the harness reclaims its own machines. Two of these can run at
# once.
# HELP-END
# Ported from scripts/e2e/run-machine-tier.sh, 449 lines of bash, under
# #672 (EPIC I / #662). The help block above is that file's header block,
# byte for byte, which is why it is a comment block rather than the module
# docstring: harness.render_help strips one leading "# " from every line
# inside the markers, and six of these lines legitimately start with a '#'
# of their own ("# Hygiene", "#451 asked for the cold figure"). In a
# docstring those would have to be written "# # Hygiene" to render the
# same, and scripts/tests/testdata/run-machine-tier.help.txt would move
# for a reason that has nothing to do with what it says. It does not move
# in this change.
#
# Two paragraphs of that header are about bash and are answered in the
# PORTED-CHECK HAZARD NOTE at the bottom rather than deleted: the brace
# group that made the file parse before it ran, and the trailing `exit 0`
# that stopped bash reading past it.

from __future__ import annotations

import os
import random
import re
import sys
import time
from pathlib import Path

# scripts/, so `bdtools` is importable from a run started anywhere.
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "machine-tier"

# Every package that reaches a machine, which is the gate's own
# gotestwatch list plus tests/machines. The harness package is not on the
# gate's list because it is a harness rather than a machine-tier package
# and runs in the plain `go test` step, but it is exactly the package whose
# own #161, #243 and #456 proofs are worth running in this placement too.
DEFAULT_PACKAGES = (
    "./tests/machines/... ./tests/machinegate/... ./tests/sftpintegration/... "
    "./tests/miniointegration/... ./tests/conformance/... ./tests/crashmatrix/..."
)

USAGE = (
    "Usage: run_machine_tier.py [--packages '<go test patterns>'] [--run <regexp>] "
    "[-v] [--race] [--keep-on-failure]"
)


class Options:
    """What the command line asked for."""

    def __init__(self) -> None:
        self.packages = DEFAULT_PACKAGES
        self.run_filter = ""
        self.verbose = False
        self.race = False
        self.keep = False
        self.help = False


def parse_options(argv: list[str]) -> Options:
    """The bash `while [ $# -gt 0 ]` loop, option for option.

    Hand-rolled rather than argparse, and not for nostalgia: argparse
    would answer an unknown option with its own usage text and exit 2,
    print a --help this file does not own, and split `--packages` on
    nothing. The refusals here are the ones the bash made, with the one
    improvement named in the hazard note: an option whose value is missing
    used to be a bare `shift 2` failing under `set -e`, which exited 1
    saying nothing at all.
    """
    options = Options()
    rest = list(argv)
    while rest:
        arg = rest.pop(0)
        if arg == "--packages":
            options.packages = _value_for(arg, rest)
        elif arg.startswith("--packages="):
            options.packages = arg[len("--packages=") :]
        elif arg == "--run":
            options.run_filter = _value_for(arg, rest)
        elif arg.startswith("--run="):
            options.run_filter = arg[len("--run=") :]
        # `go test -v` inside the manager. Worth having because the numbers
        # these tests measure are printed with t.Logf, and t.Logf on a PASSING
        # test is invisible without it: reading a measurement out of this
        # placement is otherwise only possible by making it fail.
        elif arg in ("-v", "--verbose"):
            options.verbose = True
        # `go test -race`, which is what the gate runs the machine tier with.
        # Off by default here because a race build costs compile time and the
        # cost figures in this header were measured without it; on when this
        # driver is standing in for the gate's own step.
        elif arg == "--race":
            options.race = True
        # Leaves the manager container and the network up after a FAILING run,
        # for reading. Never the default.
        elif arg == "--keep-on-failure":
            options.keep = True
        elif arg in ("-h", "--help"):
            options.help = True
            return options
        else:
            harness.die(f"unknown option {arg}", USAGE)
    return options


def _value_for(option: str, rest: list[str]) -> str:
    if not rest:
        harness.die(f"{option} needs a value", USAGE)
    return rest.pop(0)


class Run:
    """The identities of one run, and whether its teardown is armed yet."""

    def __init__(self, root: Path) -> None:
        run_id = os.environ.get("MACHINE_TIER_RUN_ID") or (
            f"{os.getpid()}-{int(time.time())}-{random.SystemRandom().randint(0, 32767)}"
        )
        self.label_key = "backupd-test"
        self.label = self.label_key + "=1"
        self.net = "backupd-machines-driver-" + run_id
        self.manager = "backupd-machine-tier-" + run_id
        self.manager_image = "backupd-machine-tier:1"

        self.source_dockerfile = root / "scripts" / "e2e" / "source-machine.Dockerfile"
        self.manager_dockerfile = root / "scripts" / "e2e" / "manager-machine.Dockerfile"

        # The caches are named volumes rather than host directories on purpose. A
        # host directory would be written by the container's uid and then be in the
        # way of an ordinary `go build` on the host; a volume is the manager
        # machine's own disk, which is what it would be on a real one.
        self.build_cache = "backupd-machine-tier-gocache"
        self.mod_cache = "backupd-machine-tier-gomodcache"

        self.platform = ""
        # Both read off the daemon and core/go.mod in check_capability,
        # which is the only place that knows how to refuse each answer.
        self.go_version = ""
        self.uid = os.getuid()
        self.gid = os.getgid()

        # `trap cleanup EXIT INT TERM` in the bash was one statement at one
        # point in the file, and everything above it -- option parsing,
        # --help, a usage refusal -- ran without a teardown because there
        # was nothing yet to tear down. This flag is that statement's
        # position, kept rather than approximated: finish() always calls
        # teardown, so teardown asks whether it was armed.
        self.armed = False
        self.keep = False


def teardown(run: Run, status: int) -> None:
    """The bash cleanup(), which ran from the EXIT trap.

    It has to work on a run that died anywhere, including before the
    things it removes existed. Everything is forced and every failure is
    swallowed for that reason, and nothing in here may raise: finish()
    calls it from a `finally`, so an exception here would replace the
    verdict the run had already reached with a traceback.

    The straggler sweep is the part that is not obvious. A network cannot be
    removed while an endpoint is still attached, and a killed run leaves the
    harness's own machines behind, so removing the manager alone leaves a
    network that never goes away. Anything still attached is ours by
    construction, because the network name carries this run's id.
    """
    if not run.armed:
        return
    if status != harness.EXIT_OK and run.keep:
        print("", file=sys.stderr, flush=True)
        print(
            f"    --keep-on-failure: leaving {run.manager} and {run.net} up.",
            file=sys.stderr,
            flush=True,
        )
        print(f"    docker exec -it {run.manager} bash", file=sys.stderr, flush=True)
        print(
            f"    docker rm -f {run.manager} && docker network rm {run.net}",
            file=sys.stderr,
            flush=True,
        )
        return
    harness.sh_ok(["docker", "rm", "-f", run.manager])
    # The harness removes its own machines, but a killed run may not have,
    # and a network with an endpoint left on it cannot be removed. Anything
    # still on this network is ours by construction: the name carries this
    # run's id.
    stragglers: list[str] = []
    try:
        listed = harness.sh(
            ["docker", "ps", "-aq", "--filter", "network=" + run.net], check=False
        )
        stragglers = listed.stdout.split()
    except harness.CommandFailed:
        # No docker on PATH at all, which is one of the ways this run ends.
        pass
    if stragglers:
        harness.sh_ok(["docker", "rm", "-f", *stragglers])
    harness.sh_ok(["docker", "network", "rm", run.net])


def check_capability(root: Path, run: Run) -> None:
    harness.step("checking this machine can run the tier")
    # A capability this machine does not have is not a failure and is not a
    # pass either: harness.cannot_run, which finish() turns into the 3
    # scripts/lib/ci-local-gate.sh ledgers as INCOMPLETE and
    # two-machine-backup gives for the same thing, so a caller can tell
    # "could not run it" from "ran it and it failed" without parsing prose.
    if not harness.have_tool("docker"):
        harness.cannot_run(
            "docker is not on PATH.",
            "The machine tier is two containers on a network; there is nothing to stand them up with.",
        )
    # The daemon half goes through the harness's own docker gate rather
    # than a second copy of it here. Its wording is not this script's old
    # wording ("the docker daemon is not answering. Start Docker and run
    # this again."); it says the same thing at more length, and one gate
    # with one sentence is what #672 is for.
    harness.require_docker()

    if not os.access(str(run.source_dockerfile), os.R_OK):
        harness.die(
            f"the source machine Dockerfile is missing at {run.source_dockerfile}.",
            "It is the one definition of the simulated VPS, shared with core/tests/machines and two-machine-backup.sh.",
        )
    if not os.access(str(run.manager_dockerfile), os.R_OK):
        harness.die(f"the manager machine Dockerfile is missing at {run.manager_dockerfile}.")

    # The Go version comes from core/go.mod rather than from a constant here,
    # because a bumped go directive that this script did not follow would
    # compile the tier against an older toolchain and say nothing.
    go_version = read_go_directive(root / "core" / "go.mod")
    if not go_version:
        harness.die(
            "could not read the go directive out of core/go.mod, so there is no toolchain"
            " version to build the manager machine with."
        )
    harness.note(f"toolchain: go {go_version} (from core/go.mod)")

    # The manager machine has to be the host daemon's own architecture. On this
    # Mac the build picked amd64 unprompted and every compile inside it ran
    # under emulation, which turns "measure the cost" into measuring qemu.
    daemon_arch = harness.sh(
        ["docker", "info", "--format", "{{.Architecture}}"], check=False
    ).stdout.strip()
    if daemon_arch in ("aarch64", "arm64"):
        run.platform = "linux/arm64"
    elif daemon_arch in ("x86_64", "amd64"):
        run.platform = "linux/amd64"
    else:
        harness.die(
            "the docker daemon reports an architecture this script does not know how to"
            f" name for --platform: {daemon_arch or 'unknown'}.",
            "Without an explicit platform the manager machine can be built for the wrong"
            " one and every compile inside it runs emulated.",
        )
    harness.note(f"platform: {run.platform} (daemon reports {daemon_arch})")

    if run.uid == 0:
        harness.die(
            "this script is running as root, and the manager machine would be too.",
            "core/internal/testenv refuses to run the permission-bit tests as root rather"
            " than skipping them, which is deliberate: a skip there deletes coverage from a"
            " run that goes on saying ok. Run this as an ordinary user.",
        )
    run.go_version = go_version


def read_go_directive(go_mod: Path) -> str | None:
    """The `go 1.x` line's version, or None.

    The awk this replaces printed nothing and exited 0 for a go.mod with no
    such line, which is why the caller checks the answer rather than the
    status. Same shape here: a file that does not carry the directive
    returns None and the refusal above says what is missing.
    """
    for line in go_mod.read_text(encoding="utf-8").splitlines():
        match = re.match(r"^go ([0-9].*)$", line)
        if match:
            return match.group(1).split()[0]
    return None


def stand_up_the_machines(root: Path, run: Run) -> None:
    harness.step("building the manager machine image")
    built = harness.sh(
        [
            "docker",
            "build",
            "-q",
            "-t",
            run.manager_image,
            "--platform",
            run.platform,
            "--build-arg",
            f"GO_VERSION={run.go_version}",
            "-f",
            str(run.manager_dockerfile),
            str(run.manager_dockerfile.parent),
        ],
        check=False,
    )
    if built.returncode != 0:
        harness.die(
            f"could not build the manager machine image from {run.manager_dockerfile}.",
            *(built.stderr or "").strip().splitlines()[-10:],
        )
    harness.note(f"manager machine: {run.manager_image}")

    harness.step("creating the dedicated network")
    created = harness.sh(
        ["docker", "network", "create", "--label", run.label, run.net], check=False
    )
    if created.returncode != 0:
        harness.cannot_run(
            f"could not create the network {run.net}.",
            "Docker's default address pool is about thirty networks wide; if it is full,"
            " reclaim the leaked ones (docker network prune) and try again.",
        )
    harness.note(f"network: {run.net}")

    harness.step("preparing the manager machine's caches")
    # A named volume is created root-owned, and the manager runs as an ordinary
    # user, so the first run inside it dies at "failed to initialize build
    # cache: permission denied" before it compiles a line. One rootful
    # throwaway container fixes the ownership once; it is not the manager and
    # it runs nothing of this repository's.
    chowned = harness.sh(
        [
            "docker",
            "run",
            "--rm",
            "--platform",
            run.platform,
            "-v",
            f"{run.build_cache}:/gocache",
            "-v",
            f"{run.mod_cache}:/gomodcache",
            "alpine:3.20",
            "chown",
            "-R",
            f"{run.uid}:{run.gid}",
            "/gocache",
            "/gomodcache",
        ],
        check=False,
    )
    if chowned.returncode != 0:
        harness.die(
            f"could not give the manager machine's caches to uid {run.uid}.",
            "They are named volumes and docker creates them root-owned; without this the"
            " toolchain inside cannot write a single object.",
        )
    harness.note(f"caches: {run.build_cache} and {run.mod_cache}, owned by {run.uid}:{run.gid}")

    harness.step("starting the manager machine on it")
    # The repository at the SAME absolute path inside as out, because the bind
    # mounts the harness asks for are resolved by the host's daemon against the
    # host's filesystem. See the header.
    started = harness.sh(
        [
            "docker",
            "run",
            "-d",
            "--name",
            run.manager,
            "--platform",
            run.platform,
            "--label",
            run.label,
            "--network",
            run.net,
            "--network-alias",
            "manager",
            "--user",
            f"{run.uid}:{run.gid}",
            "--group-add",
            "0",
            "-v",
            "/var/run/docker.sock:/var/run/docker.sock",
            "-v",
            f"{root}:{root}",
            "-v",
            f"{run.build_cache}:/gocache",
            "-v",
            f"{run.mod_cache}:/gomodcache",
            "-w",
            f"{root}/core",
            "-e",
            "GOCACHE=/gocache",
            "-e",
            "GOMODCACHE=/gomodcache",
            "-e",
            "GOFLAGS=-mod=mod",
            "-e",
            "GOWORK=off",
            "-e",
            "HOME=/tmp",
            "-e",
            f"RCLONE_MANAGER_MACHINES_NETWORK={run.net}",
            "-e",
            "CI_LOCAL=" + os.environ.get("CI_LOCAL", ""),
            "-e",
            "CI_LOCAL_SKIP_DOCKER=" + os.environ.get("CI_LOCAL_SKIP_DOCKER", ""),
            run.manager_image,
            "sleep",
            "infinity",
        ],
        check=False,
    )
    if started.returncode != 0:
        harness.die(
            "could not start the manager machine.",
            *(started.stderr or "").strip().splitlines()[-10:],
        )

    # An /etc/passwd entry for the uid the manager runs as. Without one,
    # ssh-keygen exits 255 with "No user exists for uid <n>" and the first
    # machine the harness tries to stand up has no host key. The uid is the
    # invoking user's, chosen so the repository bind mount and the caches are
    # writable, and no base image can be expected to have an entry for it.
    entries = harness.sh(
        [
            "docker",
            "exec",
            "-u",
            "0:0",
            run.manager,
            "sh",
            "-c",
            f"getent group {run.gid} >/dev/null || echo 'machinetier:x:{run.gid}:' >> /etc/group; "
            f"getent passwd {run.uid} >/dev/null || echo"
            f" 'machinetier:x:{run.uid}:{run.gid}:machine tier:/tmp:/bin/sh' >> /etc/passwd",
        ],
        check=False,
    )
    if entries.returncode != 0:
        harness.die(
            f"could not give uid {run.uid} a passwd entry inside the manager machine.",
            "ssh-keygen refuses to run for a uid with no entry, so no machine would get a host key.",
        )

    # The manager has to be able to reach the daemon before anything is worth
    # running, and it is the one thing that is not obvious from a `go test`
    # failure ten minutes later.
    if not harness.sh_ok(["docker", "exec", run.manager, "docker", "version"]):
        harness.die(
            "the manager machine cannot talk to the docker daemon through the mounted socket.",
            f"The socket is root:root 0660 and the container runs as {run.uid}:{run.gid}"
            " with gid 0 as a supplementary group; if that has changed, the harness inside"
            " cannot create a single machine.",
        )
    # And buildx, separately, because a CLI without it silently falls back to
    # the legacy builder and the harness's own image build is watched through
    # --progress=plain, which the legacy builder rejects. The failure that
    # produces reads as "unknown flag" from inside a Go test, twenty lines deep.
    if not harness.sh_ok(["docker", "exec", run.manager, "docker", "buildx", "version"]):
        harness.die(
            "the manager machine's docker client has no buildx plugin.",
            "Without it the CLI falls back to the legacy builder, which does not understand"
            " --progress=plain, and the harness cannot build a source machine.",
        )
    harness.note(f"manager: {run.manager} (uid {run.uid}, on {run.net}, socket reachable)")


def run_the_tier(run: Run, options: Options) -> None:
    packages = options.packages.split()
    race = ["-race"] if options.race else []

    harness.step("compiling the tier inside the manager machine")
    # Before the watched run, not inside it.
    #
    # gotestwatch bounds a run by the pace of its own test events, and its
    # unmeasured floor is 45 seconds: nothing has been observed yet, so that is
    # all it has to go on. A cold compile inside this container takes longer
    # than that and emits no test event while it works, so the first thing the
    # watchdog sees is 45 seconds of silence and it kills a run that was
    # building normally. That is not a gotestwatch bug. In scripts/ci-local.sh
    # the gotestwatch step is preceded by a whole `go test ./...` step, so the
    # build cache is warm by the time anything is watched; this driver had no
    # such step, and this is it.
    #
    # `-run ^$` compiles and links every test binary and runs no test in it, so
    # nothing here starts a container.
    compile_started = int(time.time())
    compiled = harness.sh(
        ["docker", "exec", run.manager, "go", "test", *race, "-count=1", "-run", "^$", *packages],
        check=False,
    )
    if compiled.returncode != 0:
        harness.die(
            "the machine-tier packages do not compile inside the manager machine.",
            "Nothing below can run, and this is a build failure rather than a test one.",
            *(compiled.stderr or "").strip().splitlines()[-20:],
        )
    harness.note(f"compiled in {int(time.time()) - compile_started}s")

    harness.step("running the machine tier inside the manager machine, under gotestwatch")
    harness.note(f"packages: {options.packages}")
    if options.run_filter:
        harness.note(f"filter:   -run {options.run_filter}")
    harness.note(
        f"no port is published by any source or medium: RCLONE_MANAGER_MACHINES_NETWORK={run.net}"
    )

    started = int(time.time())
    # `set +e` around this one command in the bash, because the status is
    # READ rather than propagated: a machine-tier failure is this script's
    # own refusal, with the elapsed time in it, and not a bare non-zero.
    # check=False is that same decision, said in one argument.
    watched = harness.sh(
        ["docker", "exec", run.manager, "go", "run", "./cmd/gotestwatch"]
        + race
        + ["-count=1"]
        + (["-v"] if options.verbose else [])
        + (["-run", options.run_filter] if options.run_filter else [])
        + packages,
        check=False,
        capture=False,
    )
    elapsed = int(time.time()) - started

    if watched.returncode != 0:
        harness.die(
            f"the machine tier failed inside the manager machine after {elapsed}s (exit {watched.returncode}).",
            "That is a product or harness failure, not a capability one: the manager was up,"
            " on the network, and talking to the daemon.",
        )

    harness.step(f"PASSED in {elapsed}s")
    harness.note("every source and medium was reached by its network alias, with nothing published")


def body(root: Path, run: Run, options: Options) -> int:
    check_capability(root, run)
    stand_up_the_machines(root, run)
    run_the_tier(run, options)
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    self_path = Path(__file__).resolve()
    # --help reads this file, and the run cd's to the repository root
    # below, so a path the shell left relative stops resolving once it
    # does. Both are settled here, from the same file, before that
    # happens.
    root = harness.repo_root(self_path)
    run = Run(root)

    def parse_and_run() -> int:
        options = parse_options(sys.argv[1:])
        if options.help:
            # --help prints the header block between the two markers at the
            # top of this file, and deliberately not a range of line
            # numbers (#514). Markers move with the text they delimit, and
            # scripts/tests/e2e-help.test.sh pins the rendered result, so a
            # reword is a decision now rather than an accident.
            print(harness.render_help(self_path))
            return harness.EXIT_OK
        run.keep = options.keep
        os.chdir(root)
        # Everything above ran without a teardown, exactly as it did in
        # bash, where `trap cleanup EXIT INT TERM` is written here.
        run.armed = True
        return body(root, run, options)

    return harness.finish(parse_and_run, teardown=lambda status: teardown(run, status))


if __name__ == "__main__":
    sys.exit(main())


# PORTED-CHECK HAZARD NOTE
#
# One row per assertion in scripts/e2e/run-machine-tier.sh, and one for
# each of its `set -e`, `pipefail`, `trap`, subshell and `$(...)`
# propagations. harness.py's rule: a port can silently convert a check
# into one that cannot fail, and that is worse than deleting it because
# nothing says it stopped watching.
#
# a machine with no docker gets a verdict, not a failure
#   hazard in bash:   `command -v docker || cannot_run` with
#                     EXIT_CANNOT_RUN=3 written in the file. A `die` there
#                     would have reported a product failure for a machine
#                     that never tried, and a bare non-zero would have
#                     been indistinguishable from a red tier.
#   hazard in python: STILL EXISTS, and it moved: 3 is no longer written
#                     here at all. harness.cannot_run raises, finish()
#                     translates EXIT_CANNOT_RUN (97) to 3, and a 3
#                     arriving from any command becomes 1. So the verdict
#                     is reachable and unforgeable, where the bash could
#                     have had `exit 3` typed anywhere.
#   held by:          check_capability's have_tool branch, finish()'s
#                     translation table, and
#                     core/tests/machines/topology_test.go, which reads
#                     this file and requires the cannot-run path to be
#                     present and the literal 3 to be absent.
#
# the daemon is answering before anything is built
#   hazard in bash:   `docker info >/dev/null 2>&1 || cannot_run`, a
#                     second copy of the docker gate with its own wording.
#   hazard in python: STILL EXISTS, through harness.require_docker, which
#                     is the one copy. The wording changed and that is the
#                     visible cost of having one gate; the verdict did
#                     not.
#   held by:          harness.require_docker, whose own refusal is
#                     cannot_run.
#
# the toolchain version is read off core/go.mod rather than assumed
#   hazard in bash:   `go_version="$(awk ...)"` -- awk EXITS 0 having
#                     matched nothing, so the status says nothing and only
#                     the `[ -n ... ]` test catches a go.mod whose
#                     directive moved. A port that trusted the status
#                     would have compiled the tier against whatever the
#                     Dockerfile defaults to and said nothing.
#   hazard in python: STILL EXISTS in the same shape: read_go_directive
#                     returns None for a file with no directive and the
#                     caller refuses on the ANSWER, never on a status.
#   held by:          check_capability's `if not go_version` refusal, and
#                     read_go_directive's docstring saying why the status
#                     is not the check.
#
# the manager machine is built for the daemon's own architecture
#   hazard in bash:   `$(docker info --format ... || true)` tolerates a
#                     daemon that will not answer, and the `case` refuses
#                     an architecture it cannot name rather than letting
#                     --platform default and measuring qemu.
#   hazard in python: STILL EXISTS: check=False keeps the tolerance
#                     explicit, an unrecognised or empty answer reaches
#                     the same `die`.
#   held by:          check_capability's arch branch, whose else-clause is
#                     a refusal rather than a default.
#
# the tier does not run as root
#   hazard in bash:   `[ "$uid" != "0" ] || die`. core/internal/testenv
#                     refuses as root rather than skipping the
#                     permission-bit tests, and a rootful manager turns
#                     that refusal into a red gate or an opt-out.
#   hazard in python: STILL EXISTS: os.getuid() and the same refusal.
#   held by:          check_capability's uid branch, and
#                     core/tests/machines/topology_test.go, which reads
#                     this file and requires it to run the manager with
#                     --user and to contain no use of core/internal/
#                     testenv's root opt-out variable. That check is a
#                     text scan and cannot tell a mention from a use, so
#                     the variable is deliberately not spelled anywhere in
#                     this file -- naming it here made the guard go red on
#                     its own documentation, which is how it was found.
#
# both Dockerfiles exist before a container is asked for
#   hazard in bash:   `[ -r "$file" ] || die`, naming the shared
#                     definition so a missing one is not read as a docker
#                     problem.
#   hazard in python: STILL EXISTS: os.access(..., os.R_OK), same two
#                     refusals, same sentences.
#   held by:          check_capability's two access branches.
#
# every docker step that fails, refuses
#   hazard in bash:   `docker build ... || die`, `docker network create
#                     ... || cannot_run`, `docker run ... || die`, `docker
#                     exec ... || die` -- eight guarded statuses, each
#                     with its own sentence, and the network one is a
#                     VERDICT rather than a failure because a full address
#                     pool is a machine condition.
#   hazard in python: STILL EXISTS: check=False plus a returncode branch
#                     at every one of them, keeping cannot_run exactly
#                     where it was.
#   held by:          stand_up_the_machines, which also quotes docker's
#                     last stderr lines: with capture the diagnosis would
#                     otherwise be dropped, and dropping it is how a
#                     refusal becomes unactionable.
#
# the manager can reach the daemon, and its client has buildx
#   hazard in bash:   two `docker exec ... >/dev/null 2>&1 || die` probes,
#                     both about a failure that otherwise surfaces twenty
#                     lines deep inside a Go test.
#   hazard in python: STILL EXISTS: harness.sh_ok, which is exactly this
#                     shape (both streams dropped, only the status asked
#                     for).
#   held by:          the two sh_ok branches in stand_up_the_machines.
#
# a compile failure is told apart from a test failure
#   hazard in bash:   `docker exec ... go test -run '^$' ... >/dev/null ||
#                     die`. Its own step, outside the watched run, because
#                     gotestwatch's unmeasured floor is 45 seconds and a
#                     cold compile lands on it.
#   hazard in python: STILL EXISTS: check=False, a returncode branch, and
#                     the same two sentences.
#   held by:          run_the_tier's compile branch.
#
# the watched run's status is READ rather than propagated
#   hazard in bash:   `set +e; docker exec ...; status=$?; set -e`. The
#                     hazard is subtle and live: gotestwatch has its own
#                     exit 3 for "a control could not be measured"
#                     (scripts/tests/ci-local-gate.test.sh group M), and
#                     an UNGUARDED docker exec would have made that 3 this
#                     script's status -- the machine verdict, from a run
#                     that stood every container up.
#   hazard in python: STILL EXISTS, and it is now impossible to reach by
#                     accident from two directions: check=False makes the
#                     status data, and finish() would turn a stray 3 into
#                     1 even if somebody deleted check=False.
#   held by:          run_the_tier's `check=False` plus its `die` on a
#                     non-zero, and harness.finish's 3 -> 1 translation.
#
# teardown runs on success, on failure and on interrupt
#   hazard in bash:   `trap cleanup EXIT INT TERM`, and the EXIT trap is
#                     the only reason a killed run does not leave a
#                     network nothing can remove.
#   hazard in python: STILL EXISTS, in a different shape and with a
#                     failure mode bash did not have: a Python signal
#                     handler that only set a flag would let the
#                     interrupted call finish and the script carry on
#                     against containers about to be removed.
#                     harness.install_signal_handlers RAISES, so the
#                     unwind reaches finish()'s `finally`.
#   held by:          harness.install_signal_handlers,
#                     finish(teardown=...), and Run.armed, which is where
#                     the `trap` statement used to be: --help and a usage
#                     refusal still tear nothing down.
#
# teardown cannot itself become the verdict
#   hazard in bash:   every command in cleanup() is `|| true`, because it
#                     runs on a run that died anywhere, including before
#                     the things it removes existed.
#   hazard in python: STILL EXISTS, and it is sharper here: finish() calls
#                     teardown from a `finally`, so an exception would
#                     REPLACE the run's verdict with a traceback. Note
#                     that harness.sh raises CommandFailed(127) for a
#                     missing binary even with check=False, which is
#                     exactly the docker-is-absent case teardown reaches.
#   held by:          teardown using sh_ok for the removals and catching
#                     CommandFailed around the one call whose stdout it
#                     needs.
#
# the straggler sweep removes what is on the network, and only that
#   hazard in bash:   `docker rm -f $stragglers` unquoted, with
#                     SC2086 disabled on purpose: word splitting was the
#                     mechanism. An empty variable there would have run
#                     `docker rm -f` with no arguments, which is why the
#                     `[ -n ... ]` test is in front of it.
#   hazard in python: GONE, replaced by a list: `.split()` of docker's
#                     output is the argv, so an empty answer produces no
#                     call at all rather than a call with no arguments,
#                     and a name with a space in it cannot become two
#                     arguments.
#   held by:          teardown's `if stragglers` and the list
#                     concatenation, which cannot re-split.
#
# --keep-on-failure keeps a FAILING run and never a passing one
#   hazard in bash:   `local status=$?` as cleanup's first line, which is
#                     the only place that status was available.
#   hazard in python: STILL EXISTS: finish() passes the pre-translation
#                     status to teardown, so the branch reads the same
#                     status it read in bash (97 for a capability verdict,
#                     not the 3 the caller ends up seeing).
#   held by:          teardown's `status != harness.EXIT_OK and run.keep`
#                     branch.
#
# an option with a missing value refuses out loud
#   hazard in bash:   GONE WRONG, and this is the one place the port is
#                     deliberately better: `--packages` as the last
#                     argument set the value to empty and then ran
#                     `shift 2`, which fails with one argument left, which
#                     under `set -e` exited 1 having printed NOTHING.
#   hazard in python: GONE, replaced by _value_for, which refuses by name
#                     and prints the usage line. Same exit status (1),
#                     with the reason on it.
#   held by:          _value_for, called from both option branches.
#
# --help renders the block between the markers, or refuses
#   hazard in bash:   `sed -n '2,84p' "$0"` was the old form: the help an
#                     operator read was a set of coordinates, and
#                     inserting a comment above the boundary rewrote it
#                     while deleting one truncated it (#514). Both had
#                     happened.
#   hazard in python: STILL EXISTS, held in the same place: the markers
#                     are read out of the FILE, so a file that lost one
#                     refuses rather than printing an empty help and
#                     exiting 0.
#   held by:          harness.render_help, whose refusal is a `die`, and
#                     scripts/tests/e2e-help.test.sh cases A, B, C and D3.
#
# the file cannot be rewritten underneath a running run
#   hazard in bash:   the whole script was one brace group, and it ended
#                     with `exit 0` inside it, because bash reads a script
#                     incrementally BY BYTE OFFSET: this driver ran 159
#                     seconds, passed every package, and died on a syntax
#                     error in a branch never taken because the file had
#                     been edited underneath it. The `exit 0` was the
#                     other half: after the group, bash goes back to the
#                     file at the offset it saved.
#   hazard in python: GONE, and there is nothing to replace it with
#                     because the interpreter closes the hole: CPython
#                     compiles the whole module to bytecode before
#                     executing any of it, and never returns to the file.
#                     A 169-second run is exactly the kind somebody edits
#                     while it works, and now that is a no-op rather than
#                     a syntax error at minute three.
#   held by:          the interpreter. The brace group and the trailing
#                     `exit 0` are deleted rather than transliterated, and
#                     this row is why.
#
# pipefail
#   hazard in bash:   `set -o pipefail` was on. This script builds no
#                     pipeline, so it never bit; the one place it would
#                     have mattered is the straggler sweep, which is a
#                     command substitution rather than a pipe.
#   hazard in python: not applicable. harness.sh takes an argv list and
#                     never a shell string, so a pipeline cannot appear
#                     without somebody writing one on purpose.
#   held by:          harness.sh's signature.
