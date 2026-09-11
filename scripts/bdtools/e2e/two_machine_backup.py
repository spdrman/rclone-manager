#!/usr/bin/env python3
"""The two-machine end-to-end backup proof (#356), EPIC I / I1.6's first port.

The operator-facing description is the comment block below, between the
HELP-START and HELP-END markers, which is what `--help` renders. It is a
comment block and not this docstring on purpose: see
`bdtools.harness.render_help` for why one renderer with one rule beats a
renderer that has to guess whether the block it is reading was prefixed.
"""
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
# Then it installs backupd onto the manager machine with
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
#                   address with a TCP reset. backupd failed
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
#                   against a real deployment: `backupd activity`
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
#   empty-record    #662, and the case that reproduced it. It was written
#                   RED, against a build where it could not pass, and it
#                   is GREEN as of the fix in this same repository. It
#                   joins `all` and scripts/ci-local.sh expects it to
#                   PASS: a failure here is a REGRESSION of #662, not an
#                   expected result, and must not be dismissed as one.
#                   Run it alone with:
#
#                       scripts/e2e/two-machine-backup.sh --case empty-record
#
#                   Two parts. The first is a FENCE: for every artifact
#                   the set produced, a fresh sha256 of the committed
#                   file, computed inside the manager machine, has to
#                   equal what the real `backupd` reports for it AND what the
#                   sidecar recovery manifest records. Nothing in this
#                   repository could say that before -- every other test
#                   compares a record against another record, or against
#                   a file the test itself wrote -- and it is the
#                   assertion that would have caught #662 at the moment it
#                   happened. The fence is then proven able to fail: a
#                   record that disagrees with the bytes is planted, the
#                   comparison is required to go red about that artifact
#                   by name, and the record is put back.
#
#                   The second is the dead end, and it is the half the fix
#                   inverted. The deployment is put into the exact state
#                   the field produced -- an empty record (size_bytes 0,
#                   and the sha256 of no bytes at all, carrying
#                   verification_class "content") over a good file at its
#                   final name -- the product's own `backupd reconcile` is run
#                   over it, and three things are then required. The copy
#                   is NOT condemned: reconciliation finds nothing
#                   unresolved and leaves the artifact at a durable
#                   restore point. The recorded fault still REACHES AN
#                   OPERATOR, in words, so a fix that settled the
#                   contradiction in silence does not pass here. And the
#                   file is untouched by all of it.
#
#                   Before the fix, reconciliation condemned the copy at
#                   that point by comparing two of its own records without
#                   ever opening the file, and this part then walked every
#                   documented verb an operator has -- revalidate,
#                   reinstate, validate, retry, a cycle, reconcile -- to
#                   prove that none of them ended at a durable restore
#                   point. That walk is KEPT and stays reachable: if a
#                   plant ever opens the dead end again, the walk is what
#                   runs, and it fails naming every verb that refused. A
#                   fence deleted the day it went green cannot measure the
#                   regression it was built for.
#
#                   The FR-12 collision refusal is CORRECT and is not
#                   weakened by any of this; that fence is held in
#                   core/internal/lifecycle.
#
#                   The state is PLANTED, not raced for, and the case says
#                   so where it does it. The field fault is transient (the
#                   same artifacts re-transferred first try with nothing
#                   about the host changed), so provoking it is not
#                   reproducible -- and it does not need to be, because
#                   the defect being reproduced is not that a read-back
#                   can be wrong. It is that nothing ever checked the
#                   record against the bytes, and that once the record was
#                   wrong nothing could recover. Both are properties of
#                   the state, not of how the state arose. What the plant
#                   must not do is fake the evidence, so the file's own
#                   bytes are measured and asserted good before anything
#                   is written, and every mutation reports how many rows it
#                   changed and refuses if that is none.
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
# once the image under test is built. Measured on an M5 with a warm image
# cache: plain 1m40s, empty-record 1m49s, so the #662 case costs about ten
# seconds more than the install-and-back-up it shares with plain. The plant
# itself is milliseconds; the ten seconds are the two backup cycles the
# dead-end walk has to actually perform against a real source, and the
# nine `backupd` invocations it walks.
#
# The image build is the expensive part on a cold Docker cache (minutes:
# it compiles the Go binaries and builds the UI bundle) and is done once
# for the whole run.
# HELP-END
from __future__ import annotations

import json
import os
import random
import re
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness
from bdtools.harness import cannot_run, die, note, step, wait_or_die

# ---------------------------------------------------------------------
# What this run is made of
# ---------------------------------------------------------------------

# The cases, in the order `--case all` runs them. `empty-record` is last
# because it is the most expensive and the newest, so a run that dies in
# it has already reported on everything cheaper. It used to be last for a
# different reason -- it was the only case that was red on purpose (#662)
# -- and that reason is gone with the fix.
CASES = [
    "plain",
    "no-arguments",
    "connection-cap",
    "activity-diagnostic",
    "lifecycle",
    "retention-apply",
    "empty-record",
]

USAGE = (
    "Usage: two-machine-backup [--case "
    + "|".join(CASES)
    + "|all] [--keep-on-failure]"
)

# What the fake machines are made of. Both are pinned to a major version
# rather than a digest, for the same reason core/tests/sftpfixture pins
# atmoz/sftp:alpine by tag: this script never trusts either image's
# contents, it verifies what it needs directly, so a moved tag surfaces as
# a red run rather than as a silent change of behaviour.
SOURCE_BASE = "atmoz/sftp:alpine"
MACHINE_BASE = "docker:28-dind"

SFTP_USER = "backupuser"
SFTP_UID = 1001

# The cap the connection-cap case imposes. Two, so the THIRD simultaneous
# connection from the manager machine is refused, which is the production
# rule this models exactly.
CONNECTION_CAP = 2

# The two tags the lifecycle case installs and then upgrades to. Same
# bytes, two references: what makes it an upgrade rather than a converge
# is the TAG the installer compares (installed_image_tag against
# image_tag), and this case is about the mode's own bookkeeping, not about
# two different builds.
LIFECYCLE_FROM = "backupd-e2e-lifecycle:0.2.0"
LIFECYCLE_TO = "backupd-e2e-lifecycle:0.3.0"

# Deterministic payload bytes rather than /dev/urandom, so a failure is
# reproducible and a digest mismatch can be reasoned about.
PAYLOAD_BYTES = 3145728
PAYLOAD_PASS = "backupd-e2e-356"

# The sha256 of no bytes at all. #662's whole shape in one constant: this
# is what the field deployment recorded as the content hash of a good
# 294-byte backup, with verification_class "content" beside it.
EMPTY_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

# The three artifacts every case backs up, and the states a durable
# restore point can be in (internal/app.ValidateArtifact's own list).
ARTIFACTS = ["payload.bin", "schema.sql", "notes.txt"]
DURABLE_STATES = ["COMMITTED", "REMOTE_DELETE_PENDING", "COMPLETE", "REMOTE_RETAINED"]

# The two substrings that prove #662's recorded fault reached an operator
# rather than being settled in silence.
#
# Substrings and not the whole sentence, on purpose. The reconciler emits
# one line per finding whose tail varies with the verification tier it
# could reach (whether a remote digest was recorded at discovery), and
# with the state handler that produced it. These two phrases are the part
# that is invariant across every one of those variants, so this asserts
# the fault was reported without pinning the fixture's own tier.
FAULT_REPORTED_PHRASES = [
    "journal row needs repair",
    "disagrees with recorded transfer size",
]


class Proof:
    """One whole run: the images, the machines, and every case asked for.

    Everything this run creates is named from `run_id`, so two runs on one
    machine never collide over a container name or a network name, and
    nothing published to a host port means they cannot collide over one of
    those either. The manager machine publishes the Web UI inside its own
    dind daemon, which is a network namespace of its own, so 8080 there is
    not 8080 here.
    """

    def __init__(self, repo_root: Path, case_list: list[str], keep: bool) -> None:
        self.repo_root = repo_root
        self.case_list = case_list
        self.keep = keep

        self.run_id = os.environ.get(
            "E2E_RUN_ID",
            f"{os.getpid()}-{int(time.time())}-{random.randint(0, 32767)}",
        )
        self.label_key = "backupd-e2e"
        self.label = self.label_key + "=two-machine"

        # Where this run's throwaway host keys and payload live. Inside the
        # working tree, gitignored, for the reason
        # scripts/bdtools/e2e/run_tests_repo_gate.py gives for its own
        # scratch: nothing this repository's tooling creates should need a
        # recursive delete outside the workspace to clean up after itself.
        # The files below are removed by name at teardown.
        self.run_dir = repo_root / ".e2e-two-machine" / self.run_id

        self.product_image = "backupd-e2e:" + self.run_id
        self.source_image = "backupd-e2e-source:1"
        self.machine_image = "backupd-e2e-machine:1"

        self.source_dockerfile = repo_root / "scripts" / "e2e" / "source-machine.Dockerfile"

        # The administrator two cases create: the lifecycle case needs a
        # user for the upgrade to preserve and the factory reset to
        # destroy, and the activity-diagnostic case needs credentials for
        # the CLI's route to the engine, which are the Web UI's own. A
        # throwaway password for a container that is deleted minutes
        # later. It leaves this process down a pipe into stdin when the
        # account is created, and as an environment variable on the exec'd
        # commands that use the route, which is what the CLI's own
        # documentation prescribes; it is never written to a file.
        self.admin_user = "e2e-operator"
        self.admin_pass = f"e2e-{self.run_id}-not-a-real-password"

        # Everything created is registered the moment it exists, so an
        # interrupt between two steps still tears down what the earlier
        # one made.
        self.created_containers: list[str] = []
        self.created_networks: list[str] = []
        self.created_case_dirs: list[Path] = []
        self.teardown_done = False

        # Filled in by the build step, read by every case.
        self.version = ""
        self.commit = ""
        self.default_image = ""
        self.probe_base = ""

        # Per-case state. The bash this replaces relied on dynamic scoping
        # to let the per-case helpers see these; naming them here is the
        # same thing said out loud.
        self.case_dir = self.run_dir
        self.want: dict[str, str] = {}

    # -----------------------------------------------------------------
    # Teardown
    # -----------------------------------------------------------------

    def teardown(self, status: int) -> None:
        """Remove everything this run created, on success, failure or signal."""
        if self.teardown_done:
            return
        self.teardown_done = True

        if self.keep and status != 0:
            print("", file=sys.stderr, flush=True)
            print(
                "==> two-machine: --keep-on-failure, so these are left up for reading:",
                file=sys.stderr,
                flush=True,
            )
            for container in self.created_containers:
                print("        container " + container, file=sys.stderr, flush=True)
            for network in self.created_networks:
                print("        network   " + network, file=sys.stderr, flush=True)
            print(f"        run dir   {self.run_dir}", file=sys.stderr, flush=True)
            return

        print("", flush=True)
        print("==> two-machine: tearing down", flush=True)
        for container in self.created_containers:
            harness.sh_ok(["docker", "rm", "-fv", container])
        # After the containers, never before: a network with an endpoint on
        # it cannot be removed, and a "network is in use" error at teardown
        # time is how a network survives a run.
        for network in self.created_networks:
            harness.sh_ok(["docker", "network", "rm", network])
        harness.sh_ok(["docker", "image", "rm", "-f", self.product_image])
        self.remove_run_dir()

    def remove_run_dir(self) -> None:
        """Delete what this run wrote, by name, then remove the directories.

        Deliberately not a recursive delete: the files are a known, short
        list, and `rm -rf` on a path built from variables is how a script
        eventually deletes the wrong thing. An unexpected extra file leaves
        the directory behind, which is visible rather than silent.

        -v on every container removal above is not decoration either.
        docker:28-dind declares VOLUME /var/lib/docker, so every manager
        machine this script starts creates an anonymous host volume holding
        that machine's whole inner Docker state, including the image under
        test after it is loaded in. `docker rm -f` leaves that volume
        behind: measured directly, starting one manager machine takes the
        host from 44 volumes to 45 and `docker rm -f` leaves it at 45,
        while `docker rm -fv` returns it to 44.
        """
        if not self.run_dir.is_dir():
            return
        for case_dir in self.created_case_dirs:
            _unlink(case_dir / "authorized_keys" / "installer.pub")
            # The two files run_activity_diagnostic redirects a command's
            # stderr into. The bash this replaces did not name them here,
            # so an activity-diagnostic run left its case directory behind
            # for nothing but an rmdir that could never succeed. Found by
            # the port, fixed here rather than carried across.
            _unlink(case_dir / "activity.err")
            _unlink(case_dir / "activity-engine-down.err")
            _rmdir(case_dir / "authorized_keys")
            _rmdir(case_dir)
        for name in (
            "ssh_host_ed25519_key",
            "ssh_host_ed25519_key.pub",
            "ssh_host_rsa_key",
            "ssh_host_rsa_key.pub",
            "upload/payload.bin",
            "upload/schema.sql",
            "upload/notes.txt",
            "product-image.tar",
            "probe-image.tar",
        ):
            _unlink(self.run_dir / name)
        _rmdir(self.run_dir / "upload")
        _rmdir(self.run_dir)
        _rmdir(self.repo_root / ".e2e-two-machine")

    # -----------------------------------------------------------------
    # Talking to the machines
    # -----------------------------------------------------------------

    def sha256_of(self, container: str, path: str) -> str:
        """The sha256 of a file, computed INSIDE the container holding it."""
        return harness.sh_out(["docker", "exec", container, "sha256sum", path]).split()[0]

    def size_of(self, container: str, path: str) -> int:
        """The size in bytes of a file, measured inside the container."""
        return int(harness.sh_out(["docker", "exec", container, "stat", "-c", "%s", path]))

    def mgr_sh(self, mgr: str, script: str, *, check: bool = True) -> str:
        """Run a shell fragment on the manager machine and return its stdout."""
        out: str = harness.sh(["docker", "exec", mgr, "sh", "-c", script], check=check).stdout
        return out.rstrip("\n")

    def mgr_install(self, mgr: str, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        """Run the real installer on the manager machine.

        Every case goes through this one function, so the only difference
        between them is the argument list, which is the thing under test.
        """
        return harness.sh(
            ["docker", "exec", mgr, "python3", "/opt/rm/install_docker_host.py", "install", *args],
            check=check,
        )

    def compose_argv(self, mgr: str, prefix: str, *args: str, interactive: bool = False) -> list[str]:
        """The installer's own compose argument list (compose_argv in install_docker_host.py).

        This drives exactly the deployment the installer produced rather
        than a second idea of one.
        """
        head = ["docker", "exec"]
        if interactive:
            head.append("-i")
        return [
            *head,
            mgr,
            "docker",
            "compose",
            "-p",
            "backupd",
            "--env-file",
            prefix + "/.env",
            "-f",
            prefix + "/compose.yaml",
            "-f",
            prefix + "/compose.image.yaml",
            *args,
        ]

    def mgr_compose(
        self,
        mgr: str,
        prefix: str,
        *args: str,
        check: bool = True,
        capture: bool = True,
        stdin_text: str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        return harness.sh(
            self.compose_argv(mgr, prefix, *args, interactive=stdin_text is not None),
            check=check,
            capture=capture,
            stdin_text=stdin_text,
        )

    def bm(
        self,
        mgr: str,
        prefix: str,
        *args: str,
        check: bool = True,
        capture: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        """The real CLI, inside the running engine container.

        Its exit status is the CLI's own. That includes the 3 issue #551
        gave it (another process is already serving this deployment), which
        is the status this proof reserves for the gate's "could not run"
        verdict: see bdtools.harness.EXIT_CANNOT_RUN for what keeps the two
        apart, and scripts/tests/two-machine-exit-status.test.sh for the
        proof that it does.
        """
        return self.mgr_compose(
            mgr, prefix, "exec", "-T", "backupd", "/backupd", *args, check=check, capture=capture
        )

    def bm_stopped(
        self, mgr: str, prefix: str, *args: str, check: bool = True, capture: bool = True
    ) -> subprocess.CompletedProcess[str]:
        """The CLI with the engine stopped, which is the world every
        configuration write in this script performs its writes in.

        `bm` above uses `compose exec`, which needs a running container;
        this uses `compose run --rm`, which starts one for the command and
        takes it away again.
        """
        return self.mgr_compose(
            mgr,
            prefix,
            "run",
            "--rm",
            "--no-deps",
            "-T",
            "backupd",
            "/backupd",
            *args,
            check=check,
            capture=capture,
        )

    def bm_routed(
        self, mgr: str, prefix: str, *args: str, check: bool = True
    ) -> subprocess.CompletedProcess[str]:
        """`bm` with the three environment variables that give the CLI a
        route to the running engine (see the CLI's own `usage()`).

        Without these the same command answers from this host's journal and
        announces `direct`, which is a different claim entirely, so every
        routed assertion checks the mode line before it checks anything
        else.

        The -e flags go on `compose exec`, not on the `docker exec` around
        it: the outer one would set them for the compose CLI running on the
        manager machine, which is not the process that needs them.
        """
        return self.mgr_compose(
            mgr,
            prefix,
            "exec",
            "-T",
            "-e",
            "BACKUP_MANAGER_API_URL=http://127.0.0.1:8080",
            "-e",
            "BACKUP_MANAGER_API_USERNAME=" + self.admin_user,
            "-e",
            "BACKUP_MANAGER_API_PASSWORD=" + self.admin_pass,
            "backupd",
            "/backupd",
            *args,
            check=check,
        )

    def engine_answers(self, mgr: str, prefix: str) -> bool:
        """Readiness probe: the engine answers `version` through compose exec."""
        return harness.sh_ok(self.compose_argv(mgr, prefix, "exec", "-T", "backupd", "/backupd", "version"))

    def engine_log(self, mgr: str, prefix: str) -> str:
        """Best-effort by design: only ever called to enrich a failure
        message, so a compose invocation that cannot reach the stack must
        not replace the real failure with its own.
        """
        out: str = self.mgr_compose(mgr, prefix, "logs", "backupd", check=False).stdout
        return out

    # -----------------------------------------------------------------
    # The run
    # -----------------------------------------------------------------

    def run(self) -> int:
        self.preflight()
        self.build_images()
        self.save_images()
        self.generate_host_keys()
        self.seed_payload()
        for case_name in self.case_list:
            self.run_case(case_name)
        step("every case passed")
        note("A fresh install on a throwaway machine pulled a backup off another throwaway")
        note("machine over a temporary network, and the bytes match.")
        return 0

    def preflight(self) -> None:
        step("preflight")
        harness.require_tools("docker", "ssh-keygen", "openssl")
        harness.require_docker()
        note("docker daemon reachable")

        # Docker-in-docker needs a privileged container, which some hosts
        # and most hardened CI runners refuse. Find that out in two seconds
        # rather than after building an image.
        if not harness.sh_ok(["docker", "run", "--rm", "--privileged", MACHINE_BASE, "true"]):
            cannot_run(
                "this Docker daemon will not run a privileged container, so docker-in-docker cannot start.",
                "The manager machine has to be a machine, not a sibling of the containers under test:",
                "mounting the host socket instead would install onto THIS host, which is the thing this "
                "test exists not to do.",
            )
        note("privileged containers are allowed, so docker-in-docker can start")

        if not self.source_dockerfile.is_file():
            die(
                f"the source machine Dockerfile is missing at {self.source_dockerfile}.",
                "It is the one definition of the simulated VPS, shared with core/tests/machines, so there is "
                "nothing to build a source machine from without it.",
            )

        # What `--image` defaults to, read out of the installer itself
        # rather than restated here. The no-arguments case passes no
        # --image at all, so the only way it can install this working
        # tree's build instead of reaching for a registry is if the image
        # is already on the machine under exactly that reference. Reading
        # the default keeps the two from drifting: if the installer's
        # default moves, this moves with it, and if its shape changes this
        # fails by name rather than silently installing a published
        # release.
        self.default_image = self._installer_default("--image")
        if not self.default_image:
            die(
                "could not read the installer's own --image default out of scripts/install/install_docker_host.py.",
                "The no-arguments case (#347) has to pre-load the image under exactly that "
                "reference, or it would either",
                "reach for a registry or test a published release instead of this working tree.",
            )
        note("the installer's --image default is " + self.default_image)

        # The same treatment for the installer's network probe (#271), and
        # for a sharper reason. That probe needs a container with a shell,
        # ping and nc, and the installer PULLS one when the host has none.
        # A manager machine is a fresh daemon holding nothing, so without
        # this every case reaches for Docker Hub in the middle of an
        # install, and a rate limit or a dropped connection there fails a
        # run that has nothing to do with either. Not hypothetical: a full
        # gate run died on `Get "https://registry-1.docker.io/...": EOF`
        # with everything else green.
        self.probe_base = self._installer_default("--probe-image")
        if not self.probe_base:
            die(
                "could not read the installer's own --probe-image default out of "
                "scripts/install/install_docker_host.py.",
                "Without it this cannot preload the probe, and every case would pull it from a registry mid-install.",
            )

        # Checked here rather than at save time, so a host with no copy and
        # no route to a registry finds out in seconds instead of after the
        # image under test has been built. cannot_run, not die: that host
        # cannot perform this proof, which is the same verdict as no Docker
        # and no privileged containers, and the gate ledgers it rather than
        # reporting a pass for a run that never happened.
        if not harness.sh_ok(["docker", "image", "inspect", self.probe_base]):
            if not harness.sh_ok(["docker", "pull", self.probe_base]):
                cannot_run(
                    f"the installer's network probe needs {self.probe_base}, this host has no copy, and it "
                    "could not be pulled.",
                    "Pull it once by hand and re-run, or every case will try to reach a registry from "
                    "inside its own install.",
                )
        note(
            f"the installer's --probe-image default is {self.probe_base}, and this host has it"
        )

    def _installer_default(self, flag: str) -> str:
        """One flag's `default=` out of the installer's own argument table.

        Read rather than restated, so the reference this preloads cannot
        drift from the reference the installer would ask for. A regex over
        the source rather than an import, because importing the installer
        would give this run a second opinion about its own defaults and
        because that file is deliberately importable by nothing (it travels
        to a NAS alone).
        """
        source = (self.repo_root / "scripts" / "install" / "install_docker_host.py").read_text(
            encoding="utf-8"
        )
        found = re.search(r'"' + re.escape(flag) + r'",\s*default="([^"]+)"', source)
        return found.group(1) if found else ""

    def build_images(self) -> None:
        step("building the image under test from this working tree")
        self.version = harness.sh_out(["git", "rev-parse", "--short", "HEAD"])
        self.commit = harness.sh_out(["git", "rev-parse", "HEAD"])
        dirty = (
            harness.sh(["git", "diff", "--quiet", "HEAD"], check=False).returncode != 0
            or harness.sh(["git", "diff", "--cached", "--quiet"], check=False).returncode != 0
        )
        if dirty:
            # Same convention as scripts/bdtools/e2e/run_tests_repo_gate.py:
            # under a pre-commit hook HEAD is the parent commit and the tree
            # carries the staged change, so the build genuinely is HEAD plus
            # something and says so.
            self.commit = self.commit + "-dirty"
        note(f"VERSION={self.version} COMMIT={self.commit}")

        built = harness.sh(
            [
                "docker",
                "build",
                "-f",
                "container/Dockerfile",
                "--build-arg",
                "VERSION=" + self.version,
                "--build-arg",
                "COMMIT=" + self.commit,
                "-t",
                self.product_image,
                ".",
            ],
            check=False,
            capture=False,
            cwd=self.repo_root,
        )
        if built.returncode != 0:
            die(
                "could not build the image under test from this working tree.",
                "Everything below tests that image, so there is nothing to fall back to: a published tag would "
                "test somebody else's build, which is #342.",
            )

        step("building the two throwaway machine images")
        # The source machine comes from scripts/e2e/source-machine.Dockerfile,
        # which is the one definition of the simulated VPS (#451). The Go
        # machine tier builds the same file through core/tests/machines, so
        # "the source machine" means one thing in this repository rather
        # than two that agree until they do not.
        source_built = harness.sh(
            [
                "docker",
                "build",
                "-q",
                "-t",
                self.source_image,
                "-f",
                str(self.source_dockerfile),
                str(self.source_dockerfile.parent),
            ],
            check=False,
        )
        if source_built.returncode != 0:
            die(
                f"could not build the source machine image from {self.source_dockerfile}.",
                source_built.stderr.rstrip(),
            )

        # The manager machine is docker-in-docker plus python3, because the
        # real installer is a Python script and this test runs the real
        # installer. Nothing else: it is meant to be a box with Docker and
        # nothing on it, and in particular no checkout of this repository
        # (#346).
        #
        # python3 is also what the empty-record case plants #662's journal
        # row with: alpine's python3 carries the sqlite3 module, and the
        # engine's own image is distroless and has no shell at all.
        dockerfile = f"FROM {MACHINE_BASE}\nRUN apk add --no-cache python3\n"
        machine_built = harness.sh(
            ["docker", "build", "-q", "-t", self.machine_image, "-"],
            check=False,
            stdin_text=dockerfile,
        )
        if machine_built.returncode != 0:
            die(
                f"could not build the manager machine image from {MACHINE_BASE}.",
                machine_built.stderr.rstrip(),
            )
        note("source machine:  " + self.source_image)
        note("manager machine: " + self.machine_image)

    def save_images(self) -> None:
        step("saving the images to move into the manager machine's own daemon")
        self.run_dir.mkdir(parents=True, exist_ok=True)
        product_tar = self.run_dir / "product-image.tar"
        if harness.sh(
            ["docker", "save", self.product_image, "-o", str(product_tar)], check=False
        ).returncode != 0:
            die(f"docker save {self.product_image} failed.")
        note(
            f"{product_tar.stat().st_size // (1024 * 1024)} MiB of image under test to move per case"
        )

        probe_tar = self.run_dir / "probe-image.tar"
        if harness.sh(
            ["docker", "save", self.probe_base, "-o", str(probe_tar)], check=False
        ).returncode != 0:
            die(f"docker save {self.probe_base} failed.")
        note("the probe image travels with the run, so no case reaches a registry")

    def generate_host_keys(self) -> None:
        """The SOURCE machine's own host keys, fresh per run.

        The CLIENT key is not here: the installer generates that one now
        (#347) and this script takes the public half from its output, which
        is the sequence a real operator follows.

        Two host keys, not one, for the reason core/tests/sftpfixture spells
        out: an SSH client negotiates a host-key algorithm by its OWN
        preference order, so pinning only ed25519 against a server offering
        both can still end up negotiating RSA and failing verification.

        stdin comes from /dev/null on both, so a run_dir that somehow
        already holds these keys (an explicit E2E_RUN_ID reused after a
        --keep-on-failure run) fails rather than sitting on ssh-keygen's
        "Overwrite (y/n)?" prompt forever. Every wait in this script is
        bounded; a prompt is an unbounded one.
        """
        step("generating the source machine's host keys")
        (self.run_dir / "upload").mkdir(parents=True, exist_ok=True)
        ed = harness.sh(
            [
                "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e host key",
                "-f", str(self.run_dir / "ssh_host_ed25519_key"),
            ],
            check=False,
            stdin_file=Path(os.devnull),
        )
        if ed.returncode != 0:
            die(
                f"could not generate the source machine's ed25519 host key at {self.run_dir}.",
                "If this run reused an E2E_RUN_ID, that directory already has one: choose another id.",
            )
        rsa = harness.sh(
            [
                "ssh-keygen", "-q", "-t", "rsa", "-b", "2048", "-N", "", "-C", "e2e host key",
                "-f", str(self.run_dir / "ssh_host_rsa_key"),
            ],
            check=False,
            stdin_file=Path(os.devnull),
        )
        if rsa.returncode != 0:
            die(f"could not generate the source machine's RSA host key at {self.run_dir}.")

    def seed_payload(self) -> None:
        """Three artifacts rather than one, deliberately.

        The leak the connection-cap case pins is per OPERATION, so one
        artifact's worth of work does not reach the ceiling: it takes
        several stats and copies before the third pool exists to be
        refused.
        """
        step("seeding the source machine's payload")
        upload = self.run_dir / "upload"
        # Deliberately still openssl over 3 MiB of zeroes, which is what
        # the shell this replaces piped from /dev/zero: identical bytes,
        # so a digest recorded against an older run still means something.
        # subprocess directly rather than harness.sh, because this is the
        # one call in the file that needs BINARY stdin and a file for
        # stdout, and widening the shared harness for a single caller is
        # how a shared harness becomes a junk drawer. The status is checked
        # here instead, which is what harness.sh would have done.
        payload = upload / "payload.bin"
        with open(payload, "wb") as out:
            enc = subprocess.run(
                [
                    "openssl", "enc", "-aes-256-ctr", "-pbkdf2",
                    "-pass", "pass:" + PAYLOAD_PASS, "-nosalt",
                ],
                input=b"\x00" * PAYLOAD_BYTES,
                stdout=out,
                stderr=subprocess.PIPE,
            )
        if enc.returncode != 0:
            die("could not generate the payload.", enc.stderr.decode("utf-8", "replace").rstrip())

        (upload / "schema.sql").write_text(
            "CREATE TABLE artifacts (id text primary key);\n", encoding="utf-8"
        )
        (upload / "notes.txt").write_text(
            "issue 356: two machines, one temporary network, one real backup.\n", encoding="utf-8"
        )
        for name in ARTIFACTS:
            (upload / name).chmod(0o644)
        upload.chmod(0o777)
        note("3 artifacts: payload.bin (3 MiB), schema.sql, notes.txt")

    # -----------------------------------------------------------------
    # One case
    # -----------------------------------------------------------------

    def run_case(self, case_name: str) -> None:
        """One whole proof, from a machine with nothing on it to a
        byte-for-byte comparison of what landed.

        Everything a case needs is created inside it and named with this
        run's id, so two cases never share a network, a container or a
        directory, and a case is released at the end rather than at the
        exit trap. That is not tidiness: a manager machine is a Docker
        daemon with the image under test loaded into it and two product
        containers running, and holding four of those at once is how a run
        starts failing for reasons that are about the host rather than
        about the product.

        The order of what it asserts is the argument. The machine is
        checked empty before anything installs, because "installed on a
        fresh machine" is the claim. The version the ENGINE reports is
        checked against the version that was asked for, because a stale
        default once installed something else and said "Installed." The
        bytes are compared against digests read from the SOURCE rather than
        from what this script wrote, because what has to match is what the
        machine being backed up is actually serving. And the source is
        re-hashed afterwards, because the set was created read-only and a
        backup that modifies what it pulls from is the failure nobody would
        look for.
        """
        net = f"rm-e2e-net-{self.run_id}-{case_name}"
        src = f"rm-e2e-source-{self.run_id}-{case_name}"
        mgr = f"rm-e2e-manager-{self.run_id}-{case_name}"
        self.case_dir = self.run_dir / case_name

        step("case: " + case_name)
        (self.case_dir / "authorized_keys").mkdir(parents=True, exist_ok=True)
        self.created_case_dirs.append(self.case_dir)

        # ------------------------------------------------ the network
        if harness.sh(["docker", "network", "create", "--label", self.label, net], check=False).returncode != 0:
            die(f"could not create the temporary network {net}.")
        self.created_networks.append(net)
        note("temporary network " + net)

        # --------------------------------------- the manager machine
        started = harness.sh(
            [
                "docker", "run", "-d",
                "--name", mgr,
                "--network", net,
                "--network-alias", "manager",
                "--label", self.label,
                "--privileged",
                "-e", "DOCKER_TLS_CERTDIR=",
                self.machine_image,
            ],
            check=False,
        )
        if started.returncode != 0:
            die("could not start the manager machine.", started.stderr.rstrip())
        self.created_containers.append(mgr)

        wait_or_die(
            180,
            "the manager machine's own Docker daemon to come up",
            lambda: harness.sh_ok(["docker", "exec", mgr, "docker", "info"]),
        )
        note(
            "manager machine {} has Docker {} and nothing else".format(
                mgr,
                harness.sh_out(["docker", "exec", mgr, "docker", "version", "--format", "{{.Server.Version}}"]),
            )
        )

        # Nothing is installed yet, and the test says so rather than
        # assuming it: the GIVEN is a machine with Docker and no
        # backupd.
        if harness.sh_out(["docker", "exec", mgr, "docker", "ps", "-aq"]).strip():
            die("the manager machine already has containers on it, so it is not the fresh machine this test needs.")

        # --------------------------- move the image under test across
        step("  moving the image under test into the manager machine")
        # `docker save` piped into the manager machine's own `docker load`.
        # Saved once, above, rather than re-serialised per case: the tar is
        # the same bytes either way and the pipe is the same mechanism.
        if harness.sh(
            ["docker", "exec", "-i", mgr, "docker", "load"],
            check=False,
            stdin_file=self.run_dir / "product-image.tar",
        ).returncode != 0:
            die(f"could not move {self.product_image} into the manager machine's daemon.")
        if not harness.sh_ok(["docker", "exec", mgr, "docker", "image", "inspect", self.product_image]):
            die(f"{self.product_image} is not on the manager machine after the load.")
        # The probe image with it, so the install below never reaches for a
        # registry. Asserted present rather than assumed loaded, because a
        # missing one does not fail here: it fails several minutes later,
        # inside the installer, as a pull that times out.
        if harness.sh(
            ["docker", "exec", "-i", mgr, "docker", "load"],
            check=False,
            stdin_file=self.run_dir / "probe-image.tar",
        ).returncode != 0:
            die(f"could not move {self.probe_base} into the manager machine's daemon.")
        if not harness.sh_ok(["docker", "exec", mgr, "docker", "image", "inspect", self.probe_base]):
            die(
                f"{self.probe_base} is not on the manager machine after the load, so the installer's network "
                "probe would pull it."
            )
        note(
            f"{self.product_image} and {self.probe_base} are on the manager machine, and no registry was involved"
        )

        # The installer, and NOTHING else. No checkout on this machine at
        # all, which is #346's actual criterion: the canonical compose has
        # to come from the copy embedded in the installer, because there is
        # no container/compose.yaml here to read.
        harness.sh(["docker", "exec", mgr, "mkdir", "-p", "/opt/rm"])
        harness.sh(
            [
                "docker", "cp",
                str(self.repo_root / "scripts" / "install" / "install_docker_host.py"),
                mgr + ":/opt/rm/install_docker_host.py",
            ]
        )

        prefix, install_out = self._install_for_case(case_name, mgr)
        print(_indent(install_out, 7), flush=True)

        self._assert_installed_version(mgr, prefix)
        pubkey = self._authorise_installer_key(install_out)

        source_ip = self._start_source_machine(case_name, net, src, mgr, pubkey)
        self._create_backup_set(case_name, mgr, prefix, source_ip)

        # ---------------------------------------------------- run it
        step("  running the backup set")
        cycled = self.bm(mgr, prefix, "run", "--config", "/etc/backupd/config", check=False, capture=False)
        if cycled.returncode != 0:
            die(
                "the backup cycle exited non-zero.",
                "This is the case that pins #264: the source refuses a third simultaneous connection, and a "
                "manager that leaks a connection pool per operation cannot get past it."
                if case_name == "connection-cap"
                else "",
            )

        backups = prefix + "/backups/source"
        self._assert_bytes(mgr, src, backups)

        # The product's own verdict, on top of the bytes: a set whose
        # artifacts all landed and verified is HEALTHY, and `status` exits
        # non-zero on anything else (FR-24).
        health = self.bm(
            mgr, prefix, "status", "--config", "/etc/backupd/config", check=False, capture=False
        )
        if health.returncode != 0:
            die("the engine's own status says this backup set is not healthy, even though the bytes match.")

        if case_name == "lifecycle":
            self.run_lifecycle(mgr, prefix)
        if case_name == "retention-apply":
            self.run_retention_apply(mgr, prefix)
        if case_name == "activity-diagnostic":
            self.run_activity_diagnostic(mgr, prefix)
        if case_name == "empty-record":
            self.run_empty_record(mgr, prefix, backups)

        step(f"  case {case_name} passed")

        # Released here rather than left to the exit trap. Each manager
        # machine is a whole Docker daemon with the image under test loaded
        # into it and two product containers running, and the Docker VM
        # this was written on has under 4 GiB: four cases holding on to all
        # of that at once is how a run starts failing for reasons that are
        # about the machine rather than about the product. The teardown
        # still names them, so a case that dies before reaching this line
        # is still cleaned up, and --keep-on-failure still keeps what
        # failed.
        self.release_case(net, src, mgr)

    def release_case(self, net: str, *containers: str) -> None:
        """Remove one finished case's containers and network.

        Safe to run twice: the teardown will try again on everything, and
        `docker rm` on something already gone is not an error worth
        reporting.
        """
        for container in containers:
            harness.sh_ok(["docker", "rm", "-fv", container])
        # After the containers, never before: a network with an endpoint on
        # it cannot be removed.
        harness.sh_ok(["docker", "network", "rm", net])

    def _install_for_case(self, case_name: str, mgr: str) -> tuple[str, str]:
        if case_name == "no-arguments":
            # #347: `install` with NO arguments. The only reason this can be
            # hermetic is that the image is already on the machine under the
            # installer's own default reference, so preflight finds it
            # present and never reaches for a registry. What the engine then
            # reports is what proves which build actually got installed.
            harness.sh(["docker", "exec", mgr, "docker", "tag", self.product_image, self.default_image])
            # #346: nothing but the installer is on this machine. Say so out
            # loud rather than trusting the setup above to have stayed true.
            listing = [
                line for line in self.mgr_sh(mgr, "ls /opt/rm").splitlines() if line.strip()
            ]
            if [line for line in listing if line != "install_docker_host.py"]:
                die("the manager machine has more than the installer on it, so this case is not proving #346's claim.")
            # A checkout would be a container/compose.yaml, a go.mod or a
            # scripts/ tree. Searched where one could plausibly be rather
            # than over the whole filesystem, because /var/lib/docker holds
            # the unpacked layers of the image under test and a find over
            # those would answer a different question.
            found = self.mgr_sh(
                mgr,
                r"find /opt /root /srv /home /usr/local -maxdepth 4 "
                r"\( -name compose.yaml -o -name go.mod \) 2>/dev/null | head -1",
                check=False,
            )
            if found.strip():
                die(
                    "there is a checkout of this repository on the manager machine, so a no-checkout "
                    "install is not what this case would be measuring.",
                )
            step("  installing with NO arguments at all (#347), from one copied file with no checkout (#346)")
            done = self.mgr_install(mgr, check=False)
            if done.returncode != 0:
                print(_combined(done), file=sys.stderr, flush=True)
                die(
                    "the installer refused or failed with no arguments on a machine with nothing pre-existing.",
                    "That is #347's whole criterion, and this is the run that was never performed before.",
                )
            return "/root/backupd", _combined(done)

        if case_name == "lifecycle":
            harness.sh(["docker", "exec", mgr, "docker", "tag", self.product_image, LIFECYCLE_FROM])
            harness.sh(["docker", "exec", mgr, "docker", "tag", self.product_image, LIFECYCLE_TO])
            step("  installing " + LIFECYCLE_FROM)
            done = self.mgr_install(mgr, "--image", LIFECYCLE_FROM, "--no-pull", "--timeout", "240", check=False)
            if done.returncode != 0:
                print(_combined(done), file=sys.stderr, flush=True)
                die("the installer refused or failed on the manager machine.")
            return "/root/backupd", _combined(done)

        # The ordinary route: an explicit image, and the canonical compose
        # file copied in from a checkout, which is the other half of #346's
        # --compose-file contract.
        harness.sh(
            ["docker", "cp", str(self.repo_root / "container" / "compose.yaml"), mgr + ":/opt/rm/compose.yaml"]
        )
        step("  installing with scripts/install/install_docker_host.py")
        done = self.mgr_install(
            mgr,
            "--prefix", "/opt/rm/deploy",
            "--compose-file", "/opt/rm/compose.yaml",
            "--image", self.product_image,
            "--no-pull",
            "--timeout", "240",
            check=False,
        )
        if done.returncode != 0:
            print(_combined(done), file=sys.stderr, flush=True)
            die(
                "the installer refused or failed on the manager machine.",
                "That is the installer's own verdict on a machine with Docker and the image already loaded.",
            )
        return "/opt/rm/deploy", _combined(done)

    def _assert_installed_version(self, mgr: str, prefix: str) -> None:
        """#342: a stale --image default installed 0.1.0 and the installer
        said "Installed." So the version the engine actually reports has to
        be the version that was asked for, and this asks it rather than
        trusting the tag. In the no-arguments case this is the ONLY thing
        standing between a hermetic run and a silently published release.
        """
        reported = self.bm(mgr, prefix, "version").stdout.rstrip("\n")
        print(_indent(reported, 7), flush=True)
        lines = reported.splitlines()
        # This line is about the VERSION, not the name, but it stays
        # anchored on the name so a banner that says something else still
        # fails here. 0.3.3 is a clean cut: the image carries /backupd and
        # nothing else, so there is no second spelling to allow for.
        if "backupd " + self.version not in lines:
            die(
                "the installed engine reports a different version from the one that was installed.",
                "asked for: " + self.version,
                "reported:  " + (lines[0] if lines else ""),
                "This is #342's shape: a reference that installs something other than what was requested, "
                "while reporting success.",
            )
        if "commit " + self.commit not in lines:
            die(
                "the installed engine reports a different commit from the one that was built.",
                "asked for: " + self.commit,
                "reported:  " + next((line for line in lines if line.startswith("commit ")), ""),
            )
        note("the engine reports the version and commit that were installed")

    def _authorise_installer_key(self, install_out: str) -> str:
        """#347 from the other end: the installer generates the keypair and
        prints the public half because nothing can be pulled until it is on
        the machine being backed up. So this takes it out of the installer's
        own output and puts it there, which is both the operator's next step
        and the proof that what was printed is usable.
        """
        found = re.search(r"ssh-ed25519 [A-Za-z0-9+/=]+( [^ ]*)?", install_out)
        if not found:
            # Only the cases that let the installer default --ssh-key get
            # one, and today that is all of them. If that ever stops being
            # true this says so rather than silently authorising nothing.
            die(
                "the installer did not print a generated public key, so there is nothing to authorise "
                "on the source machine.",
                "#347's own criterion is that a no-argument install produces a usable keypair and "
                "says what to do with it.",
            )
        pubkey = found.group(0)
        (self.case_dir / "authorized_keys" / "installer.pub").write_text(pubkey + "\n", encoding="utf-8")
        note(f"authorising the key the installer generated: {pubkey[:40]}...")
        return pubkey

    def _start_source_machine(self, case_name: str, net: str, src: str, mgr: str, pubkey: str) -> str:
        started = harness.sh(
            [
                "docker", "run", "-d",
                "--name", src,
                "--network", net,
                "--network-alias", "source",
                "--label", self.label,
                "--cap-add", "NET_ADMIN",
                "-v", "{}:/etc/ssh/ssh_host_ed25519_key:ro".format(self.run_dir / "ssh_host_ed25519_key"),
                "-v", "{}:/etc/ssh/ssh_host_ed25519_key.pub:ro".format(self.run_dir / "ssh_host_ed25519_key.pub"),
                "-v", "{}:/etc/ssh/ssh_host_rsa_key:ro".format(self.run_dir / "ssh_host_rsa_key"),
                "-v", "{}:/etc/ssh/ssh_host_rsa_key.pub:ro".format(self.run_dir / "ssh_host_rsa_key.pub"),
                "-v", "{}:/home/{}/.ssh/keys:ro".format(self.case_dir / "authorized_keys", SFTP_USER),
                "-v", "{}:/home/{}/upload".format(self.run_dir / "upload", SFTP_USER),
                self.source_image,
                f"{SFTP_USER}::{SFTP_UID}:{SFTP_UID}:upload",
            ],
            check=False,
        )
        if started.returncode != 0:
            die("could not start the source machine.", started.stderr.rstrip())
        self.created_containers.append(src)

        wait_or_die(
            120,
            "the source machine's sshd to start listening",
            lambda: harness.sh_ok(
                ["docker", "exec", src, "sh", "-c", "nc -w 2 127.0.0.1 22 </dev/null 2>/dev/null | grep -q ^SSH-"]
            ),
        )
        note(f"source machine {src} is serving SSH, authorising the installer's key")

        source_ip = harness.sh_out(
            [
                "docker", "inspect", "-f",
                '{{(index .NetworkSettings.Networks "' + net + '").IPAddress}}',
                src,
            ]
        ).strip()
        if not source_ip:
            die(f"the source machine has no address on {net}.")
        note("source machine address on the temporary network: " + source_ip)

        # The digests every assertion below is against, read from the
        # SOURCE, not from the copy this script wrote. What has to match is
        # the bytes the machine being backed up is actually serving.
        self.want = {
            name: self.sha256_of(src, f"/home/{SFTP_USER}/upload/{name}")
            for name in ARTIFACTS
        }
        note("source payload.bin sha256 " + self.want["payload.bin"])

        if case_name == "connection-cap":
            self._impose_connection_cap(net, src, source_ip)
        return source_ip

    def _impose_connection_cap(self, net: str, src: str, source_ip: str) -> None:
        # `! -i lo` so the readiness probe above (which dials 127.0.0.1 from
        # inside this same container) can never eat one of the two slots the
        # manager machine is meant to have.
        capped = harness.sh(
            [
                "docker", "exec", src, "iptables", "-A", "INPUT", "!", "-i", "lo",
                "-p", "tcp", "--syn", "--dport", "22",
                "-m", "connlimit", "--connlimit-above", str(CONNECTION_CAP), "--connlimit-mask", "32",
                "-j", "REJECT", "--reject-with", "tcp-reset",
            ],
            check=False,
        )
        if capped.returncode != 0:
            cannot_run(
                "this kernel will not install an iptables connlimit rule inside the source container.",
                "The connection-cap case models #264 by refusing a third simultaneous connection from one address,",
                "and without the rule it would silently become a second copy of the plain case: green, "
                "and proving nothing.",
                "Run with --case plain if this machine cannot carry the rule, and know that #264's "
                "shape is then untested.",
            )
        # Proven, not assumed. Hold two connections open from a throwaway
        # container on this network and check the third is refused, so a
        # rule that installs and does not bite cannot make the case vacuous.
        probe = (
            f"(sleep 20 | nc {source_ip} 22 >/dev/null 2>&1) &\n"
            f"(sleep 20 | nc {source_ip} 22 >/dev/null 2>&1) &\n"
            "sleep 3\n"
            f"if nc -w 3 {source_ip} 22 </dev/null >/dev/null 2>&1; then exit 1; fi\n"
            "exit 0\n"
        )
        bites = harness.sh(
            ["docker", "run", "--rm", "--network", net, "--label", self.label, SOURCE_BASE, "sh", "-c", probe],
            check=False,
        )
        if bites.returncode != 0:
            die(
                "the connection cap did not bite: a third simultaneous connection to the source machine was accepted.",
                "This case is only worth running if the cap is real. Without it, it is the plain case "
                "wearing a different name.",
            )
        note("connection cap proven: the source machine refuses a third simultaneous connection from one address")

    def _create_backup_set(self, case_name: str, mgr: str, prefix: str, source_ip: str) -> None:
        """Two steps rather than one, and issue #571 is the reason.

        A packaged install that has never been configured is still SERVING:
        the engine container announces the journal `--state-database` names
        before it puts up the first-run setup flow, so a create typed beside
        it is refused with nothing written, exactly as the second create on
        this machine would be. This used to be the one configuration write
        that got through, and what it left behind was a CLI holding a backup
        set the running engine had never heard of, which is #535 on a fresh
        install.

        So the refusal is asserted first, because it is the property, and
        then the create is performed the way the refusal tells an operator
        to perform it: with the engine down.
        """
        create_argv = [
            "backup-set", "create", "e2e/source",
            "--config", "/etc/backupd/config",
            "--host", source_ip,
            "--user", SFTP_USER,
            "--ssh-key-file", "/etc/backupd/id_ed25519",
            "--trust-host-key",
            "--remote-path", "/upload",
            "--local-path", "/data/backups/source",
            "--completion-strategy", "rename",
            "--read-only",
            "--state-database", "/data/state/state.db",
        ]

        step("  a create typed beside the serving engine is refused (#571)")
        refused = self.bm(mgr, prefix, *create_argv, check=False)
        if refused.returncode != 3:
            die(
                f"a first `backup-set create` typed beside the engine exited {refused.returncode}, want 3.",
                "3 is the status #551 reserves for a write refused because another process is serving this deployment,",
                "and #571 is the case where that process is a fresh install still on its setup "
                "flow. A 0 here means the",
                "configuration was written underneath an engine that will never read it, which is #535.",
                "the command said: " + _combined(refused),
            )
        # And nothing was written. `sources` needs a configuration to read,
        # so on an installation that still has none it refuses, and a zero
        # here would mean the refused create left one behind after all.
        if self.bm(mgr, prefix, "sources", "--config", "/etc/backupd/config", check=False).returncode == 0:
            die(
                'the refused create left a configuration behind, so "nothing was written" '
                "is not true on a real install.",
            )
        note("refused with exit 3, and this installation still has no configuration")

        step("  creating a backup set through the CLI, with the engine stopped")
        self.mgr_compose(mgr, prefix, "stop", "backupd", check=False)
        if self.bm_stopped(mgr, prefix, *create_argv, check=False, capture=False).returncode != 0:
            die("creating the backup set through the CLI failed.")

        # Issue #624, in the one window where it can be proven: the engine
        # is stopped, so every configuration write and every check below
        # takes the direct path, which is the same path the create above
        # just took.
        if case_name == "plain":
            self.run_source_verification(mgr, prefix, source_ip)

        self.mgr_compose(mgr, prefix, "start", "backupd")
        # On the engine answering, not on the file existing: `run --rm`
        # wrote it before this line was reached, so waiting on the file
        # would wait for nothing and the next step would race the restart.
        wait_or_die(
            180,
            "the engine to answer again after the backup set was created",
            lambda: self.engine_answers(mgr, prefix),
        )

    def _assert_bytes(self, mgr: str, src: str, backups: str) -> None:
        step("  checking the artifact against the source, by digest")
        landed = self.mgr_sh(mgr, f"ls -1 {backups} 2>/dev/null", check=False)
        note("in {}: {}".format(backups, " ".join(landed.split())))

        for name in ARTIFACTS:
            want = self.want[name]
            if not harness.sh_ok(["docker", "exec", mgr, "test", "-f", backups + "/" + name]):
                die(
                    f"{name} never landed on the manager machine.",
                    "The backup directory holds: " + " ".join(landed.split()),
                )
            got = self.sha256_of(mgr, backups + "/" + name)
            if got != want:
                die(
                    f"{name} landed with different bytes from the source's.",
                    "source:  " + want,
                    "manager: " + got,
                    "A file that exists is not a backup. This is the assertion #264 needed and no suite had.",
                )
            note(f"{name} matches the source: {want}")

        # --read-only was passed, so the source has to come out of this
        # untouched. It is the posture a VPS being backed up actually wants
        # (issue #282, "pull from here, never delete here"), and asserting
        # it here is what makes that a proven property of a real run rather
        # than a flag nobody watched work.
        for name in ARTIFACTS:
            got = self.sha256_of(src, f"/home/{SFTP_USER}/upload/{name}")
            if got != self.want[name]:
                die(f"the source machine's own {name} changed during the backup, and this set is read-only.")
        note("the source machine is untouched")

    # -----------------------------------------------------------------
    # #624: an SSH source is proven before this manager relies on it
    # -----------------------------------------------------------------

    def run_source_verification(self, mgr: str, prefix: str, source_ip: str) -> None:
        """Issue #624 end to end: an SSH source is proven before this
        manager relies on it, or it is marked as never having been proven.

        It runs against the two machines this script already has standing,
        in the window where the engine is stopped, so every command below
        takes the direct path. It has to: a configuration write beside a
        serving engine is refused with exit 3 (#571), and `backup-set
        test-connection` goes through the same door because a check that
        PASSES clears the mark, which is a configuration write.

        The failing source is the real machine under a username its sshd has
        never heard of, rather than an address nothing answers. Both fail,
        and only one of them fails the way the issue is about: the host
        resolves, the TCP connect succeeds, the host key matches what the
        set trusts, and then the account cannot authenticate.

        The order is load bearing, and it cost a run to learn. OpenSSH
        penalises a source address for a few seconds after an authentication
        failure, so the check that has to SUCCEED goes first and every
        deliberate failure after it. For the same reason nothing here probes
        for a host key: the anchor the create above established is reused,
        so this block makes exactly the connections it is asserting about
        and no others.

        Everything this creates is removed before it returns, because the
        rest of the case asserts on artifacts, health and retention for
        e2e/source alone and a second set left behind would be a second set
        in every one of those answers.
        """

        def config_yaml() -> str:
            # The configuration file, read off the manager machine rather
            # than through the CLI, because the mark is a durable fact about
            # that file and a command's own report of it is the thing under
            # test.
            return self.mgr_sh(mgr, f"cat '{prefix}'/config/*.yaml", check=False)

        # Globbed rather than named, because the filename is core/service's
        # own (source and set folded into one token) and encoding that rule
        # here would be a second copy of it. Exactly one backup set exists
        # at this point, so exactly one file is there, and the count is
        # asserted rather than assumed.
        trusted_raw = self.mgr_sh(mgr, f"cat '{prefix}'/config/known_hosts.d/*", check=False)
        trusted_lines = [line for line in trusted_raw.splitlines() if line.strip()]
        if len(trusted_lines) != 1:
            die(
                f"expected exactly one trusted host-key line beside the configuration, found {len(trusted_lines)}.",
                "This block reuses the anchor the create above established rather than probing again,",
                "so it needs to know which line that is.",
            )
        trusted = trusted_lines[0]

        base = [
            "--config", "/etc/backupd/config",
            "--host", source_ip,
            "--ssh-key-file", "/etc/backupd/id_ed25519",
            "--known-hosts-line", trusted,
            "--remote-path", "/upload",
            "--completion-strategy", "rename",
            "--state-database", "/data/state/state.db",
        ]

        step("  #624: --no-verify writes a set without proving it, and marks it")
        wrote = self.bm_stopped(
            mgr, prefix, "backup-set", "create", "e2e/offline", *base,
            "--user", SFTP_USER, "--local-path", "/data/backups/offline", "--no-verify",
            check=False,
        )
        if wrote.returncode != 0:
            die(
                "`backup-set create --no-verify` failed against a source it was told not to check.",
                _combined(wrote),
            )
        if "connection_unverified: true" not in config_yaml():
            die(
                "a --no-verify create left no mark, so it is indistinguishable from one that was proven.",
                "the configuration is: " + config_yaml(),
            )
        note("written, and the configuration says its connection was never proven")

        step("  #624: a check that passes clears the mark")
        checked = self.bm_stopped(
            mgr, prefix, "backup-set", "test-connection", "e2e/offline",
            "--config", "/etc/backupd/config", check=False,
        )
        out = _combined(checked)
        if checked.returncode != 0:
            die(
                "`backup-set test-connection` against the source this script has been backing up all along failed.",
                "the command said: " + out,
            )
        for want in ("credentials", "resolve", "connect", "host_key", "authenticate", "list"):
            if want not in out:
                die(f"the passing check does not report the {want} step: {out}")
        if "connection_unverified: true" in config_yaml():
            die(
                "a passing check left the set marked as never proven.",
                "the configuration is: " + config_yaml(),
            )
        note("six steps reported, and the mark is gone")

        step("  #624: a create against a source that cannot authenticate is refused")
        nobody = self.bm_stopped(
            mgr, prefix, "backup-set", "create", "e2e/nobody", *base,
            "--user", "nobody-" + self.run_id, "--local-path", "/data/backups/nobody",
            check=False,
        )
        out = _combined(nobody)
        if nobody.returncode != 1:
            die(
                f"a create against a source that cannot authenticate exited {nobody.returncode}, want 1.",
                "Before #624 this exited 0 and wrote the set: nothing on the create path ran a check at all.",
                "the command said: " + out,
            )
        if "authenticate" not in out:
            die(
                "the refusal does not report the authenticate step, so it is a verdict rather than a diagnosis (#596).",
                "the command said: " + out,
            )
        if "id: nobody" in config_yaml():
            die("a refused create still wrote the backup set into the configuration.")
        note("refused with exit 1, and the configuration does not carry it")

        step("  #624: a check that fails leaves the mark where it was")
        marked = self.bm_stopped(
            mgr, prefix, "backup-set", "create", "e2e/nobody", *base,
            "--user", "nobody-" + self.run_id, "--local-path", "/data/backups/nobody", "--no-verify",
            check=False,
        )
        if marked.returncode != 0:
            die("`backup-set create --no-verify` failed for the unreachable set.", _combined(marked))
        failed = self.bm_stopped(
            mgr, prefix, "backup-set", "test-connection", "e2e/nobody",
            "--config", "/etc/backupd/config", check=False,
        )
        if failed.returncode != 1:
            die(
                "`backup-set test-connection` against a source that cannot authenticate exited "
                f"{failed.returncode}, want 1.",
                "the command said: " + _combined(failed),
            )
        # Exactly one mark, not "the bad set still has one": the proven set
        # has to have LOST its mark and the unproven one has to have kept
        # it, and a build that cleared every mark it could find would pass a
        # check that only looked at one of them.
        marks = config_yaml().count("connection_unverified: true")
        if marks != 1:
            die(
                f"the configuration carries {marks} unverified marks, want exactly 1.",
                "The proven set has to lose its mark and the unproven one has to keep it.",
                "the configuration is: " + config_yaml(),
            )
        note('still marked, which is what stops the mark meaning "somebody pressed the button"')

        # Removed before the engine comes back, so the rest of this case
        # still asserts about one backup set.
        for set_id in ("e2e/nobody", "e2e/offline"):
            if self.bm_stopped(
                mgr, prefix, "backup-set", "remove", set_id, "--config", "/etc/backupd/config",
                check=False,
            ).returncode != 0:
                die(f"could not remove {set_id}, so the rest of this case would be asserting about three backup sets.")

    # -----------------------------------------------------------------
    # #598: the feed reads, and a 500 says why
    # -----------------------------------------------------------------

    def run_activity_diagnostic(self, mgr: str, prefix: str) -> None:
        """Everything above this point is about a backup arriving. This is
        about what an operator is told when something does not, and it is
        here rather than in a unit test because both halves of #598 are
        about wiring that only exists in a real deployment.

        The browser suite over in backupd-tests drives createMockApi
        through a Vite dev server. That is worth having and it is
        structurally incapable of catching what was reported: the mock
        resolved every read cleanly, so every case in it is a claim about a
        component rendering. The claims below are made against the image
        built from this tree, running as two containers on a machine that
        had nothing on it, over the same HTTP route the browser uses.
        """
        self._create_admin(mgr, prefix, "so the CLI has a route to authenticate on")

        step("  the lifecycle feed, read from the engine over its own API (#598)")
        feed = self.bm_routed(mgr, prefix, "activity", "--config", "/etc/backupd/config", check=False)
        feed_err = self.case_dir / "activity.err"
        feed_err.write_text(feed.stderr, encoding="utf-8")
        if feed.returncode != 0:
            die(
                "`backupd activity` failed against a deployment that has just completed a backup.",
                "stderr: " + feed.stderr,
            )
        if not any(line.startswith("mode: engine-attached") for line in feed.stderr.splitlines()):
            die(
                "the activity read did not go to the engine.",
                "It announced: " + (feed.stderr.splitlines() or [""])[0],
                "A read that answered from this host's own journal would satisfy every assertion below about the rows",
                "and prove nothing about the route the browser uses, which is what #598 is about.",
            )
        note("the read announced engine-attached, so it came back over /api/v1/activity")

        # The transitions the cycle above actually produced, named. Not
        # "some rows": these three files are the ones whose bytes were
        # compared against the source a moment ago, so a feed that listed
        # anything else would be describing a different deployment.
        for name in ARTIFACTS:
            if name not in feed.stdout:
                die(
                    f"the activity feed does not mention {name}, which this run backed up and verified by digest.",
                    "the feed said:", feed.stdout,
                )
        if "COMMITTED" not in feed.stdout:
            die(
                "the activity feed carries no COMMITTED transition, and this run committed three artifacts.",
                "the feed said:", feed.stdout,
            )
        # Timestamped, which is half of what the page shows and the half a
        # projection silently drops.
        if not any(
            re.match(r"^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} ", line)
            for line in feed.stdout.splitlines()
        ):
            die("no row in the activity feed carries a timestamp.", "the feed said:", feed.stdout)
        note("the feed names all three artifacts, their COMMITTED transitions, and when each happened")

        # --json is what a support conversation or a cron job parses, so it
        # is asserted on the contract's field names rather than on the table.
        as_json = self.bm_routed(
            mgr, prefix, "activity", "--config", "/etc/backupd/config", "--limit", "1", "--json",
            check=False,
        )
        if as_json.returncode != 0:
            die("`backupd activity --json` failed against the running engine.")
        if not all(field in as_json.stdout for field in ('"events"', '"artifact_id"', '"occurred_at"')):
            die("--json did not emit the contract's own ListActivityResponse shape.", "it emitted: " + as_json.stdout)
        note("--json emits the wire objects, field names and all")

        # #598's second browser claim, in the form a machine with no browser
        # can make it: with the engine down, the command must never CLAIM
        # the world it could not reach. Run through `run --rm --no-deps`
        # because there is no engine container left to exec into, which is
        # itself the situation being modelled.
        step("  a read with no engine to reach never claims it reached one")
        self.mgr_compose(mgr, prefix, "stop", "backupd", check=False)
        down = self.mgr_compose(
            mgr, prefix,
            "run", "--rm", "--no-deps", "-T",
            "-e", "BACKUP_MANAGER_API_URL=http://127.0.0.1:8080",
            "-e", "BACKUP_MANAGER_API_USERNAME=" + self.admin_user,
            "-e", "BACKUP_MANAGER_API_PASSWORD=" + self.admin_pass,
            "backupd", "/backupd", "activity", "--config", "/etc/backupd/config",
            check=False,
        )
        (self.case_dir / "activity-engine-down.err").write_text(down.stderr, encoding="utf-8")
        if "mode: engine-attached" in down.stderr:
            die(
                "the read announced engine-attached with the engine container stopped.",
                "it said: " + (down.stderr.splitlines() or [""])[0],
                "Claiming a world it could not reach is the defect #536 gave the mode line to prevent, and #598 asks "
                "for the same honesty one surface over.",
            )
        mode_lines = [line for line in down.stderr.splitlines() if line.startswith("mode: ")]
        if not mode_lines:
            die(
                "the read with no engine to reach announced no mode at all, so nothing on screen says which world "
                "the answer is about.",
                "it said: " + down.stderr,
            )
        note(f"it announced: {mode_lines[0][:60]}...")
        self.mgr_compose(mgr, prefix, "start", "backupd")
        wait_or_die(180, "the engine to answer again", lambda: self.engine_answers(mgr, prefix))

        self._assert_500_explains_itself(mgr, prefix)
        step("  activity-diagnostic passed")

    def _assert_500_explains_itself(self, mgr: str, prefix: str) -> None:
        """The clause the server half of #598 exists for.

        There were thirty `writeError(w, 500, "INTERNAL", ...)` sites in the
        web host, every one of them binding the error, testing it and
        dropping it, with nowhere to write it even if it had wanted to. So
        the frontend's own words for an INTERNAL refusal, "its own log holds
        the detail, under this correlation id", were untrue of every route
        in the product.

        A read-only configuration directory is the cheapest deterministic
        way to make the engine refuse something it cannot explain: the write
        fails in the filesystem, well below anything that could produce a
        typed reason, which is exactly the shape those thirty sites are for.
        """
        step("  a 500 the engine cannot explain still explains itself in the log (#598)")
        if harness.sh(["docker", "exec", mgr, "chmod", "0555", prefix + "/config"], check=False).returncode != 0:
            die("could not make the configuration directory read-only on the manager machine.")

        refused = self.bm_routed(
            mgr, prefix, "settings", "patch", "--timezone", "Europe/Berlin",
            "--config", "/etc/backupd/config", check=False,
        )
        if harness.sh(["docker", "exec", mgr, "chmod", "0755", prefix + "/config"], check=False).returncode != 0:
            die("could not restore the configuration directory's permissions on the manager machine.")

        refusal = _combined(refused)
        if refused.returncode == 0:
            die(
                "a configuration write against a read-only configuration directory succeeded, so there is "
                "no refusal to check.",
                "it said: " + refusal,
            )
        if "INTERNAL" not in refusal:
            die(
                "the refusal is not the INTERNAL one this case is about, so the log assertion below would be about "
                "a different failure.",
                "it said: " + refusal,
            )

        found = re.search(r"cid_[A-Za-z0-9_-]+", refusal)
        if not found:
            die(
                "the refusal quoted no correlation id, so an operator has nothing to give anybody.",
                "it said: " + refusal,
            )
        cid = found.group(0)
        note("the engine refused with correlation id " + cid)

        logs = self.engine_log(mgr, prefix)
        if cid not in logs:
            die(
                f"the correlation id {cid} the operator was handed appears nowhere in the engine's own log.",
                "That is the whole defect #598's server half is about: an id that matches nothing "
                "sends whoever quotes it",
                "grepping for a string that was never written.",
            )
        line = next(line for line in logs.splitlines() if cid in line)
        if "http_refusal" not in line:
            die(
                f"the line carrying {cid} is not the refusal event, so something else happens to mention that id.",
                "the line: " + line,
            )
        if '"error"' not in line:
            die(
                "the logged refusal carries the correlation id and not the error it refused over, which is the half "
                "that makes the id worth quoting.",
                "the line: " + line,
            )
        if "/api/v1/settings" not in line:
            die(
                "the logged refusal does not name the route it refused, so an operator holding the id "
                "still cannot say what failed.",
                "the line: " + line,
            )
        note("the engine's own log carries that id, the route, and the error underneath it")

    def _create_admin(self, mgr: str, prefix: str, why: str) -> None:
        """`auth create-admin` holds the credential store under a
        process-lifetime advisory flock, so the engine comes down for the
        length of it. The password never leaves this process except down a
        pipe into stdin here, and as an environment variable on the exec'd
        commands that use the route, which is what the CLI's own
        documentation prescribes; the container it lives in is deleted
        minutes from now.
        """
        step("  creating an administrator, " + why)
        self.mgr_compose(mgr, prefix, "stop", "backupd", check=False)
        made = self.mgr_compose(
            mgr, prefix,
            "run", "--rm", "--no-deps", "-T", "backupd",
            "/backupd-web", "auth", "create-admin", "--username", self.admin_user, "--password-stdin",
            check=False,
            stdin_text=self.admin_pass,
        )
        if made.returncode != 0:
            die("could not create an administrator on the installed instance.", _combined(made))
        self.mgr_compose(mgr, prefix, "start", "backupd")
        # On the engine answering, not on the record existing: `run --rm`
        # wrote that file before this line was reached, so waiting on it
        # would wait for nothing and the next command would race the
        # restart.
        wait_or_die(
            180,
            "the engine to answer again after the administrator was created",
            lambda: self.engine_answers(mgr, prefix),
        )

    # -----------------------------------------------------------------
    # #343: upgrade, then factory reset
    # -----------------------------------------------------------------

    def run_lifecycle(self, mgr: str, prefix: str) -> None:
        """Two of #343's acceptance criteria are written in deliberately
        anti-assertion language, and both are answered here rather than by a
        unit test: "proven by counting them before and after rather than by
        assertion", and "proven by the fresh install issuing an enrollment
        link". A mocked Docker cannot produce either, which is why the issue
        was reopened.
        """
        # The positive control for everything below, and #343's own
        # observable read in the one state where it is unambiguous: this
        # install has no administrator yet, so it MUST be issuing an
        # enrollment link. Without this, the assertion after the factory
        # reset would also pass against a log grep that never matches
        # anything.
        step("  a fresh install with no administrator issues an enrollment link")
        wait_or_die(
            60,
            "the fresh install to issue its enrollment link",
            lambda: "no administrator account exists yet" in self.engine_log(mgr, prefix),
        )
        note("it does")

        self._create_admin(mgr, prefix, "so there is a user to count")

        users_before = self.count_admins(mgr, prefix)
        sets_before = self.count_sets(mgr, prefix)
        artifacts_before = self.count_artifacts(mgr, prefix)
        note(
            f"before the upgrade: {users_before} administrator(s), {sets_before} backup set(s), {artifacts_before} "
            "catalogued artifact(s)"
        )
        if users_before != 1:
            die(f"the lifecycle case did not end up with exactly one administrator to preserve (got {users_before}).")
        if sets_before != 1:
            die(f"the lifecycle case did not end up with exactly one backup set to preserve (got {sets_before}).")
        if artifacts_before < 3:
            die(
                "the lifecycle case did not end up with the three catalogued artifacts to preserve "
                f"(got {artifacts_before}).",
            )

        step(f"  upgrading {LIFECYCLE_FROM} to {LIFECYCLE_TO} (#343)")
        upgraded = self.mgr_install(
            mgr, "--mode", "upgrade", "--image", LIFECYCLE_TO, "--no-pull", "--timeout", "240", check=False
        )
        upgrade_out = _combined(upgraded)
        if upgraded.returncode != 0:
            print(upgrade_out, file=sys.stderr, flush=True)
            die("--mode upgrade failed on a real install.")
        print(_indent(upgrade_out, 7), flush=True)
        if "0.2.0" not in upgrade_out:
            die(
                "the upgrade never reported the version that was already installed, "
                "which is one of #343's own criteria.",
            )

        users_after = self.count_admins(mgr, prefix)
        sets_after = self.count_sets(mgr, prefix)
        artifacts_after = self.count_artifacts(mgr, prefix)
        note(
            f"after the upgrade:  {users_after} administrator(s), {sets_after} backup set(s), {artifacts_after} "
            "catalogued artifact(s)"
        )
        if users_after != users_before:
            die(f"the upgrade did not preserve every user: {users_before} before, {users_after} after.")
        if sets_after != sets_before:
            die(f"the upgrade did not preserve every backup set: {sets_before} before, {sets_after} after.")
        if artifacts_after != artifacts_before:
            die(
                "the upgrade did not preserve every catalogued artifact: "
                f"{artifacts_before} before, {artifacts_after} after.",
            )
        note("the upgrade preserved every user, backup set and catalogued artifact, counted rather than asserted")

        # An upgrade that kept the administrator must NOT be issuing an
        # enrollment link. This is the control for the factory-reset
        # assertion below: without it, "the log has an enrollment link in
        # it" would pass against an install that always prints one.
        #
        # The log read here is the NEW container's: the upgrade changed the
        # image reference, so compose recreated the engine rather than
        # restarting it, and this log starts at the upgraded instance's own
        # first boot. That is what makes the absence meaningful rather than
        # an artefact of when the line was printed.
        if "no administrator account exists yet" in self.engine_log(mgr, prefix):
            die(
                "the upgraded instance is issuing an enrollment link, so the administrator record "
                "did not survive the upgrade.",
            )
        note("and it issues no enrollment link, because the administrator is still there")

        step("  factory-resetting (#343)")
        reset = self.mgr_install(
            mgr, "--mode", "factory-reset", "--confirm-factory-reset",
            "--image", LIFECYCLE_TO, "--no-pull", "--timeout", "240", check=False,
        )
        reset_out = _combined(reset)
        if reset.returncode != 0:
            print(reset_out, file=sys.stderr, flush=True)
            die("--mode factory-reset failed on a real install.")
        print(_indent(reset_out, 7), flush=True)
        if "1 administrator account" not in reset_out:
            die("factory-reset did not say it was about to destroy the administrator account, by name and count.")

        if harness.sh_ok(["docker", "exec", mgr, "test", "-f", prefix + "/state/local-auth.json"]):
            die("the administrator record is still on disk after a factory reset.")
        note("state/local-auth.json is gone")

        # The criterion itself: the resulting install has to ISSUE AN
        # ENROLLMENT LINK, which is the only observable that says the
        # administrator record went with the database rather than being left
        # behind for the engine to find.
        wait_or_die(
            120,
            "the reset instance to issue a fresh enrollment link (#343)",
            lambda: "no administrator account exists yet" in self.engine_log(mgr, prefix),
        )
        links = [word for word in self.engine_log(mgr, prefix).split() if word.startswith("http")]
        note("the fresh install issues an enrollment link: " + (links[-1] if links else ""))

        # And the retained backups are still on disk, because a factory
        # reset drops the catalog that describes them and never the files
        # themselves.
        still = self.sha256_of(mgr, prefix + "/backups/source/payload.bin")
        if still != self.want["payload.bin"]:
            die("the retained backup did not survive the factory reset, which destroys the catalog and not the files.")
        note("the retained backups are untouched")

    def count_admins(self, mgr: str, prefix: str) -> int:
        """1 when the administrator record exists, 0 otherwise."""
        return 1 if harness.sh_ok(["docker", "exec", mgr, "test", "-f", prefix + "/state/local-auth.json"]) else 0

    def count_sets(self, mgr: str, prefix: str) -> int:
        """Backup sets the engine reports, through its own read surface.

        `sources` prints one indented line per backup set, each carrying
        remote_path=. Counting that rather than the indentation, because the
        indentation is a format and the field is a fact.
        """
        out = self.bm(mgr, prefix, "sources", "--config", "/etc/backupd/config", check=False).stdout
        return sum(1 for line in out.splitlines() if "remote_path=" in line)

    def count_artifacts(self, mgr: str, prefix: str) -> int:
        """Catalogued artifacts, through the engine's own read surface.

        `artifacts` ends with its own "N artifact(s)" line, which is the
        engine counting its catalog rather than this script counting lines
        it happens to recognise.
        """
        out = self.bm(mgr, prefix, "artifacts", "--config", "/etc/backupd/config", check=False).stdout
        counts = [
            int(found.group(1))
            for found in (re.match(r"^([0-9]+) artifact\(s\)$", line) for line in out.splitlines())
            if found
        ]
        return counts[-1] if counts else 0

    # -----------------------------------------------------------------
    # #602: a retention plan applied against real restore points
    # -----------------------------------------------------------------

    def run_retention_apply(self, mgr: str, prefix: str) -> None:
        """Issue #602's whole claim, on restore points a real cycle produced
        on a real machine: an apply removes exactly the set the plan printed
        as DELETE, and nothing else.

        # Why the chain gets narrowed first

        One cycle lands three artifacts on one day, and every GFS tier keeps
        a representative of its newest bucket, so under the default chain
        all three are KEEP and the plan selects nothing. A plan that selects
        nothing makes every assertion below vacuously true, which is the
        exact shape this case exists to rule out, so the chain is narrowed
        to one per bucket through `backup-set retention` and the emptiness
        of the DELETE set is then checked rather than assumed.

        # Why the engine is stopped for it

        Not because the apply would be refused: it reads the configuration
        and writes the journal, so it is allowed beside a serving engine
        exactly as `restore` is. It is stopped because the poll loop can
        finish a cycle in the window between the preview and the apply, and
        a cycle writes the very journal rows the staleness comparison is
        computed over, so the apply would correctly refuse with
        RETENTION_PLAN_STALE and this case would be asserting on that
        refusal instead.

        # The positive control

        unmanaged-by-anything.txt is planted in the backup directory and no
        journal row mentions it. FR-20 never lists a directory to find
        something to delete, so it has to survive. It is also what the
        comparison is proven on: after the real apply, the comparison is run
        twice more against listings perturbed by hand, one with that file
        missing and one with a previewed DELETE still present, and it has to
        complain about each by name. Without that, an apply that deleted
        nothing at all passes every other line here.
        """
        backups = prefix + "/backups/source"

        step("  planting a file no journal row mentions")
        if harness.sh(
            ["docker", "exec", mgr, "sh", "-c",
             f"printf 'no journal row mentions this\\n' > {backups}/unmanaged-by-anything.txt"],
            check=False,
        ).returncode != 0:
            die(f"could not plant the unmanaged file in {backups}.")

        # A whole policy, through the CLI, with the engine down: this is a
        # configuration write and #538 refuses one beside a serving engine,
        # so it follows the same stop/run --rm/start shape the create above
        # does.
        step("  narrowing the chain so today's restore points do not all survive it")
        self.mgr_compose(mgr, prefix, "stop", "backupd", check=False)
        if self.bm_stopped(
            mgr, prefix, "backup-set", "retention", "e2e/source",
            "--config", "/etc/backupd/config",
            "--daily-days", "1", "--weekly-months", "1", "--monthly-months", "1",
            check=False,
        ).returncode != 0:
            die("giving the backup set its own retention policy failed.")

        before = self.retention_listing(mgr, backups)
        note("before the apply: " + " ".join(name for name, _ in before))

        step("  applying the plan")
        applied = self.bm_stopped(
            mgr, prefix, "retention", "apply", "e2e/source",
            "--config", "/etc/backupd/config", "--acknowledge",
            check=False,
        )
        apply_out = applied.stdout
        if applied.returncode != 0:
            die("the retention apply exited non-zero.", "It said: " + _combined(applied))
        print(_indent(apply_out, 4, bar=True), flush=True)

        # The plan the apply printed BEFORE it did anything, which is the
        # plan it applied. Read off its own output rather than from a second
        # preview: a second preview is a second decision, and comparing the
        # disk against one nothing acted on would be comparing two different
        # plans.
        want_deleted, did_delete = _split_apply_receipt(apply_out)

        if not want_deleted:
            die(
                "the plan selected nothing for deletion, so this case certifies nothing.",
                "Every assertion below is vacuously true of an apply that removed nothing at all.",
                "The chain narrowed above is supposed to leave one representative per bucket out of "
                "three same-day artifacts.",
                "The apply said: " + apply_out,
            )
        if want_deleted != did_delete:
            die(
                "the apply's own receipt names a different set from the plan it printed first.",
                "planned: " + " ".join(want_deleted),
                "applied: " + " ".join(did_delete),
            )
        note("the plan selected: " + " ".join(want_deleted))

        after = self.retention_listing(mgr, backups)
        note("after the apply:  " + " ".join(name for name, _ in after))

        complaints = retention_complaints(before, after, want_deleted)
        if complaints:
            die("the apply did not remove exactly the set the plan named:", *complaints)

        removed = sorted({name for name, _ in before} - {name for name, _ in after})
        note("removed exactly: " + " ".join(removed))

        # The positive control. Everything above is a comparison, and a
        # comparison nobody has watched fail is indistinguishable from one
        # that cannot. Both perturbations are applied to the LISTING rather
        # than to the machine, because what is under test here is the
        # assertion and a control that deleted a real file would be testing
        # rm.
        step("  proving that comparison would have noticed")
        perturbed = [row for row in after if row[0] != "unmanaged-by-anything.txt"]
        if not any("unmanaged-by-anything.txt" in c for c in retention_complaints(before, perturbed, want_deleted)):
            die(
                "the comparison reported nothing wrong about a file no verdict named going missing.",
                "It would therefore have passed against an apply that removed a file the journal never knew about,",
                "which makes every assertion in this case worthless.",
            )
        note("a file no verdict named going missing: caught")

        survivor = want_deleted[0]
        perturbed = sorted(after + [row for row in before if row[0] == survivor])
        if not any(survivor in c for c in retention_complaints(before, perturbed, want_deleted)):
            die(
                "the comparison reported nothing wrong about a previewed DELETE still sitting on disk.",
                "It would therefore have passed against an apply that confirmed a plan and carried none of it out.",
            )
        note("a previewed DELETE still on disk: caught")

        if not any("certifies nothing" in c for c in retention_complaints(before, after, [])):
            die("the comparison answered true for a plan that named nothing to delete, rather than refusing it.")
        note("an empty plan: refused rather than answered true")

        # What the deployment looks like afterwards, and the one thing about
        # it that has to hold.
        #
        # It is not "healthy", and this case found that out rather than
        # assuming it: an apply removes the local file and leaves the
        # journal row exactly as it was, so the next reconciliation finds a
        # REMOTE_RETAINED artifact whose durable local copy is missing and
        # quarantines it. The set then reports DEGRADED and `status` exits
        # non-zero, for a run that did precisely what its plan said. That is
        # issue #608 and it is not this case's to fix.
        #
        # So the exit code is not gated on, and one thing is: nothing may be
        # reported as UNRECOVERABLE. The source here is read-only, so these
        # route to the ordinary recoverable QUARANTINED; the same code path
        # sends a COMPLETE artifact to QUARANTINED_LOST, which is the
        # unrecoverable one, and retention's own deliberate deletion being
        # recorded as unrecoverable data loss is the version of #608 that
        # would be an emergency rather than a defect.
        step("  bringing the engine back up")
        self.mgr_compose(mgr, prefix, "start", "backupd")
        wait_or_die(
            180,
            "the engine to answer again after the retention apply",
            lambda: self.engine_answers(mgr, prefix),
        )

        health = self.bm(mgr, prefix, "status", "--config", "/etc/backupd/config", check=False).stdout
        print(_indent(health, 4, bar=True), flush=True)
        if not any(line.startswith("e2e/source:") for line in health.splitlines()):
            die("the engine's status says nothing about e2e/source after the retention apply.", "It said: " + health)
        if re.search(r"unrecoverable: [1-9]", health):
            die(
                "the engine reports an UNRECOVERABLE artifact after a retention apply that removed only "
                "what its own plan named.",
                "A deletion this product decided on, previewed and confirmed must never be recorded "
                "as data it has lost.",
                "It said: " + health,
            )
        note("nothing is reported as unrecoverable; the DEGRADED reading itself is issue #608")

    def retention_listing(self, mgr: str, directory: str) -> list[tuple[str, str]]:
        """Fingerprint one directory inside a container as (name, sha256), sorted.

        Names AND digests, because "present afterwards" is a weaker claim
        than "present and the same file", and the difference is the whole of
        what a restore point is for.
        """
        raw = self.mgr_sh(mgr, f"cd {directory} && sha256sum * 2>/dev/null", check=False)
        rows = []
        for line in raw.splitlines():
            parts = line.split()
            if len(parts) >= 2:
                rows.append((parts[1], parts[0]))
        return sorted(rows)

    # -----------------------------------------------------------------
    # #662: the record must equal the bytes, and the dead end
    # -----------------------------------------------------------------

    def run_empty_record(self, mgr: str, prefix: str, backups: str) -> None:
        """Issue #662, at the machine tier, in two parts.

        THIS CASE WAS RED ON PURPOSE UNTIL #662 WAS FIXED, AND IS NOW THE
        REGRESSION FENCE FOR THAT FIX. It joins `all` and
        `scripts/ci-local.sh` expects it to PASS: a failure here is a
        regression of #662, not an expected result. Run it alone with

            scripts/e2e/two-machine-backup.sh --case empty-record

        # What the field produced, and why it is PLANTED here

        On a real NAS at 0.3.3 a completed local copy was recorded as
        `size_bytes: 0` with the sha256 of no bytes at all and
        `verification_class: "content"`, in BOTH the journal and the sidecar
        recovery manifest, over a file that was a good 294 bytes.
        Reconciliation then quarantined it -- "recorded remote size 294
        disagrees with recorded transfer size 0", both operands records, the
        file never opened -- and no documented verb could undo it.

        The state below is PLANTED, deliberately, and this is not a
        provoked race. The field fault is transient: re-attempting the same
        two artifacts on the NAS transferred them correctly first try with
        nothing about the host changed. So provoking it is not reproducible
        -- and it does not need to be, because the product defect being
        reproduced is NOT that a read-back can be wrong. It is that nothing
        ever checks the record against the bytes, and that once the record
        is wrong nothing can recover. Both of those are properties of the
        state, not of how the state arose.

        What the plant must not do is fake the evidence. So the file's own
        bytes are measured and asserted non-empty before anything is
        written, every mutation reports how many rows it actually changed
        and refuses if that is zero, and the quarantine that follows is
        produced by the product's own `backupd reconcile` rather than written by
        hand.
        """
        step("  #662 part A: the record has to describe the bytes on disk")
        self._assert_records_match_disk(mgr, prefix, backups)

        step("  #662 part A control: proving that fence can fail")
        self._prove_fence_can_fail(mgr, prefix, backups)

        step("  #662 part B: the recorded fault is settled, and an operator is told")
        self._settle_the_empty_record(mgr, prefix, backups)

    def _artifact_id(self, name: str) -> str:
        return "e2e/source/" + name

    def _journal_record(self, mgr: str, prefix: str, name: str) -> dict[str, str]:
        """One artifact's record as the REAL `backupd` reports it.

        Through the shipped CLI rather than by reading the database,
        because "what the product says about this artifact" is the thing
        under test: an operator has no other window, and #662 was found by
        somebody reading exactly this output.

        Worth stating plainly, because it is part of the defect: `backupd
        artifacts <id>` does not print the local placement's own
        size_bytes at all. printArtifactCopies suppresses the block
        entirely when the only copy is an ordinary ACTIVE local one, which
        is every artifact in every deployment with no storage medium
        configured. So the zero that #662 wrote into the placement is
        invisible from the terminal, and the only half of it the CLI shows
        is the checksum. That is why the manifest is read too.
        """
        out = self.bm(
            mgr, prefix, "artifacts", self._artifact_id(name), "--config", "/etc/backupd/config"
        ).stdout
        record = {}
        for line in out.splitlines():
            if ":" in line:
                key, _, value = line.partition(":")
                record[key.strip()] = value.strip()
        return record

    def _manifest(self, mgr: str, backups: str, name: str) -> dict[str, Any]:
        """The EPIC-B section 19.3 sidecar recovery manifest, as JSON.

        This is the record a rebuild trusts when the journal is gone, which
        is why it is asserted separately rather than assumed to agree.
        """
        raw = self.mgr_sh(mgr, f"cat {backups}/{name}.manifest.json")
        parsed: dict[str, Any] = json.loads(raw)
        return parsed

    def _record_complaints(self, mgr: str, prefix: str, backups: str) -> list[str]:
        """Everything wrong between what is recorded and what is on disk.

        A function that PRINTS what is wrong rather than one that dies, the
        same shape run_retention_apply's own comparison takes and for the
        same reason: a helper that called die could only ever be exercised
        by a run that was already failing, so nothing could ask it "would
        you notice". This one can be handed a deliberately corrupted record
        and required to complain, which is what the control does.
        """
        complaints: list[str] = []
        for name in ARTIFACTS:
            path = backups + "/" + name
            disk_size = self.size_of(mgr, path)
            disk_sha = self.sha256_of(mgr, path)

            # The fixture guard. Every comparison below is between a
            # recorded number and a measured one, and a comparison of 0
            # against 0 passes for the wrong reason, which is precisely the
            # defect's own shape. So the bytes are asserted real first.
            if disk_size == 0:
                die(
                    f"{path} is zero bytes on the manager machine before anything was compared.",
                    "This case cannot tell a correct record from an empty one over an empty file, "
                    "so it refuses to try.",
                )

            record = self._journal_record(mgr, prefix, name)
            recorded_sum = record.get("checksum", "")
            recorded_size = record.get("size_bytes", "")
            if recorded_sum != disk_sha:
                complaints.append(
                    "#662: the journal records checksum {} for {}, which is {} bytes on disk and hashes to {}".format(
                        recorded_sum or "(none)", path, disk_size, disk_sha
                    )
                )
            if recorded_size != str(disk_size):
                complaints.append(
                    "#662: the journal records size_bytes {} for {}, which is {} bytes on disk".format(
                        recorded_size or "(none)", path, disk_size
                    )
                )

            manifest = self._manifest(mgr, backups, name)
            if manifest.get("checksum") != disk_sha:
                complaints.append(
                    "#662: the sidecar recovery manifest records checksum {} for {}, which hashes to {}; "
                    "the manifest is the record a rebuild trusts when the journal is gone".format(
                        manifest.get("checksum") or "(none)", path, disk_sha
                    )
                )
            if manifest.get("size_bytes") != disk_size:
                complaints.append(
                    "#662: the sidecar recovery manifest records size_bytes {} for {}, "
                    "which is {} bytes on disk".format(
                        manifest.get("size_bytes"), path, disk_size
                    )
                )
            for placement in manifest.get("placements", []):
                if placement.get("medium") != "local":
                    continue
                if placement.get("size_bytes") != disk_size:
                    complaints.append(
                        "#662: the manifest's local placement records size_bytes {} for {}, "
                        "which is {} bytes on disk".format(
                            placement.get("size_bytes"), path, disk_size
                        )
                    )
                if placement.get("checksum") != disk_sha:
                    complaints.append(
                        "#662: the manifest's local placement records checksum {} for {}, which hashes to {}".format(
                            placement.get("checksum") or "(none)", path, disk_sha
                        )
                    )
                if placement.get("verification_class") == "content" and placement.get("checksum") != disk_sha:
                    complaints.append(
                        '#662: the local placement claims verification_class="content" while its recorded checksum '
                        "{} does not describe the file it points at ({})".format(
                            placement.get("checksum") or "(none)", path
                        )
                    )
        return complaints

    def _assert_records_match_disk(self, mgr: str, prefix: str, backups: str) -> None:
        """Part A. Green today, and the assertion that would have caught
        #662 in the wild at the moment it happened.

        No test anywhere in this repository could say this before: every
        other one compares a record against another record, or against a
        file the test itself wrote. This compares the product's own two
        records -- the journal, through the shipped `backupd`, and the sidecar
        recovery manifest -- against a fresh sha256 of the committed file,
        computed inside the machine the file is on.
        """
        complaints = self._record_complaints(mgr, prefix, backups)
        if complaints:
            die(
                "what the product recorded does not describe the bytes it made durable (#662).",
                *complaints,
            )
        note("all three artifacts: journal, sidecar manifest and the bytes on disk agree")

    def _prove_fence_can_fail(self, mgr: str, prefix: str, backups: str) -> None:
        """A fence nobody has seen fail is not a fence.

        One record is corrupted to the value the field produced, the
        comparison above is required to go red about THAT artifact by name,
        and the record is then put back and the comparison required to go
        green again. Both halves matter: without the restore, part B would
        be measuring a deployment this control had already broken.
        """
        name = "schema.sql"
        path = backups + "/" + name
        disk_sha = self.sha256_of(mgr, path)
        if disk_sha == EMPTY_SHA256:
            die(
                f"{path} is empty, so corrupting its record to the sha256 of no bytes would change nothing.",
                "This control cannot prove the fence can fail against a file that is already what the defect writes.",
            )

        original = self._manifest(mgr, backups, name)
        note(f"planting a record that disagrees with {name}: checksum -> the sha256 of no bytes")
        self._plant_journal_hash(mgr, prefix, name, EMPTY_SHA256)
        self._plant_manifest(mgr, backups, name, size=None, checksum=EMPTY_SHA256)

        complaints = self._record_complaints(mgr, prefix, backups)
        named = [c for c in complaints if name in c]
        if not named:
            die(
                "NO FAILURE: the record/disk comparison did not complain about a record it had just "
                "been shown to be wrong.",
                f"{path} hashes to {disk_sha} on disk and both its journal row and its sidecar manifest were "
                f"rewritten to {EMPTY_SHA256}.",
                "Every assertion in part A is therefore incapable of failing, which makes them worth nothing at all.",
                "complaints the comparison did make: " + ("; ".join(complaints) if complaints else "(none)"),
            )
        note("the fence went red, as it must: " + named[0])

        note("putting the record back")
        self._plant_journal_hash(mgr, prefix, name, disk_sha)
        self._write_manifest(mgr, backups, name, original)
        restored = self._record_complaints(mgr, prefix, backups)
        if restored:
            die(
                "the control could not put the record back, so part B would run against a "
                "deployment this control broke.",
                *restored,
            )
        note("restored, and the fence is green again")

    def _plant_journal_hash(self, mgr: str, prefix: str, name: str, checksum: str) -> None:
        """Rewrite one artifact's recorded local hash in the journal.

        Through the manager machine's own python3 and its sqlite3 module.
        The state database is on the manager machine's filesystem through
        the compose volume (STATE_DIR:/data/state), and the manager is
        alpine-based dind with a shell, unlike the engine image, which is
        distroless and has none.
        """
        self._sqlite(
            mgr,
            prefix,
            "UPDATE artifacts SET local_hash = ?, local_hash_alg = 'sha256' WHERE artifact_name = ?",
            [checksum, name],
            what="the journal's recorded local hash for " + name,
        )

    def _sqlite(self, mgr: str, prefix: str, statement: str, params: Sequence[object], *, what: str) -> int:
        """Run one UPDATE against the engine's journal and REQUIRE it to bite.

        The rowcount is checked rather than the exit status, because a
        statement that matched nothing exits 0 and would turn every
        comparison after it into a measurement of the tree against itself:
        green, and about nothing.
        """
        script = (
            "import json, sqlite3, sys\n"
            "params = json.load(sys.stdin)\n"
            "db = sqlite3.connect({!r})\n"
            "cur = db.execute({!r}, params)\n"
            "db.commit()\n"
            "print(cur.rowcount)\n"
        ).format(prefix + "/state/state.db", statement)
        done = harness.sh(
            ["docker", "exec", "-i", mgr, "python3", "-c", script],
            check=False,
            stdin_text=json.dumps(list(params)),
        )
        if done.returncode != 0:
            die(f"could not rewrite {what} on the manager machine.", _combined(done))
        rows = int(done.stdout.strip() or "0")
        if rows < 1:
            die(
                f"the statement that was supposed to rewrite {what} matched no rows.",
                "A mutation that exits 0 having changed nothing turns the next comparison into a measurement of the",
                "tree against itself, which looks exactly like a pass. Refusing rather than continuing.",
                "statement: " + statement,
            )
        return rows

    def _write_manifest(self, mgr: str, backups: str, name: str, manifest: dict[str, Any]) -> None:
        harness.sh(
            ["docker", "exec", "-i", mgr, "sh", "-c", f"cat > {backups}/{name}.manifest.json"],
            stdin_text=json.dumps(manifest, indent=2) + "\n",
        )

    def _plant_manifest(
        self, mgr: str, backups: str, name: str, *, size: int | None, checksum: str
    ) -> None:
        """Rewrite the sidecar recovery manifest to what the field recorded."""
        manifest = self._manifest(mgr, backups, name)
        if size is not None:
            manifest["size_bytes"] = size
        manifest["checksum"] = checksum
        manifest["checksum_algorithm"] = "sha256"
        for placement in manifest.get("placements", []):
            if placement.get("medium") != "local":
                continue
            if size is not None:
                placement["size_bytes"] = size
            placement["checksum"] = checksum
            placement["checksum_algorithm"] = "sha256"
            # The field row carried this beside the empty hash, and it is
            # the part that makes the record a lie rather than a gap: the
            # placement claims content verification passed, on nothing.
            placement["verification_class"] = "content"
        self._write_manifest(mgr, backups, name, manifest)

    def _settle_the_empty_record(self, mgr: str, prefix: str, backups: str) -> None:
        """Part B, and the half the fix inverted.

        Put the deployment into the exact state the field produced, run the
        product's own reconciliation over it, and require three things: the
        good copy is not condemned, the recorded fault still reaches an
        operator in words, and the file is untouched.

        This was the red half. It walked every documented verb against the
        real distroless `backupd` inside the manager machine and proved that
        none of them ended at a durable restore point, which is #662's
        third defect. The walk is not deleted now that it passes -- it is
        kept, in `_walk_the_dead_end`, and runs if a plant ever opens the
        dead end again.

        Still remedy-agnostic about HOW the copy survives. Reading the file
        before condemning it, widening `validate`, or making `retry`
        non-destructive of `reinstate` would each satisfy this; which one
        is not pinned here.

        The FR-12 collision refusal itself is CORRECT and must not be
        weakened to achieve any of them. That fence is held in
        core/internal/lifecycle by
        TestIssue662_FR12StillRefusesAStrayFileAtTheFinalName.
        """
        name = "notes.txt"
        artifact = self._artifact_id(name)
        path = backups + "/" + name

        # The bytes, before anything is planted. Asserted rather than
        # assumed, so this case can neither pass nor fail on an empty
        # fixture: everything below is about a record that disagrees with a
        # file that is genuinely good.
        disk_size = self.size_of(mgr, path)
        disk_sha = self.sha256_of(mgr, path)
        if disk_size == 0 or disk_sha != self.want[name]:
            die(
                "the file this case is about is not the good copy it needs before the plant.",
                f"on disk: {path} is {disk_size} bytes, sha256 {disk_sha}",
                f"source:  sha256 {self.want[name]}",
            )
        note(f"{path} is {disk_size} bytes on disk, sha256 {disk_sha}, matching the source")

        # The plant, with the engine stopped. Stopped because that is the
        # field's own causality -- the empty record was already durable
        # before anything read it -- and because it keeps SQLite out of a
        # write race with the serving process, which would be a flake about
        # something this case is not testing.
        step("  planting the record the field produced (an empty record over a good file)")
        self.mgr_compose(mgr, prefix, "stop", "backupd", check=False)
        self._sqlite(
            mgr, prefix,
            "UPDATE artifacts SET transfer_bytes = 0, local_hash = ?, local_hash_alg = 'sha256' "
            "WHERE artifact_name = ?",
            [EMPTY_SHA256, name],
            what="the journal's transfer record for " + name,
        )
        self._sqlite(
            mgr, prefix,
            "UPDATE placements SET size_bytes = 0, hash = ?, hash_alg = 'sha256', "
            "verification_class = 'content' "
            "WHERE medium = 'local' AND artifact_id = "
            "(SELECT id FROM artifacts WHERE artifact_name = ?)",
            [EMPTY_SHA256, name],
            what="the journal's local placement for " + name,
        )
        self._plant_manifest(mgr, backups, name, size=0, checksum=EMPTY_SHA256)
        self.mgr_compose(mgr, prefix, "start", "backupd")
        wait_or_die(180, "the engine to answer again after the plant", lambda: self.engine_answers(mgr, prefix))

        # The file has to be untouched by all of that. If the plant moved a
        # byte, everything below would be about a different situation.
        if self.sha256_of(mgr, path) != disk_sha:
            die("the plant changed the file, and it must only ever change the record.")
        note(
            f"record planted: size_bytes 0, checksum {EMPTY_SHA256} (the sha256 of no bytes), "
            f"over {disk_size} good bytes",
        )

        # The product's own reconciliation is what decides this, exactly as
        # it did in the field. Not written by hand: the verdict this part
        # turns on has to be one the product reached for itself, or this is
        # about a situation the product cannot actually get into.
        #
        # #662 INVERTED THIS, AND IT IS NOW THE REGRESSION FENCE FOR THE
        # FIX. Before the fix, reconciliation condemned the copy right here
        # by comparing two of its own records without ever opening the
        # file, and the rest of part B was the walk below proving that no
        # documented verb could undo it. Rather than delete assertions that
        # went green, the same three facts are asserted with the verdict
        # the other way up:
        #
        #   1. the planted record no longer condemns the copy:
        #      reconciliation finds nothing unresolved and leaves the
        #      artifact at a durable restore point;
        #   2. the recorded fault still REACHES AN OPERATOR in words. A fix
        #      that quietly settled the contradiction would satisfy 1 on
        #      its own, and it would trade a dead end for a deployment
        #      carrying a journal nobody knows disagreed with itself;
        #   3. none of it touched the file.
        step("  the product's own reconciliation, over the planted record")
        reconciled = self.bm(mgr, prefix, "reconcile", "--config", "/etc/backupd/config", check=False)
        reconcile_out = _combined(reconciled)
        print(_indent(reconcile_out, 4, bar=True), flush=True)

        detail = self._journal_record(mgr, prefix, name)
        state_now = detail.get("state", "")
        recorded_reason = detail.get("reason", "")

        # 1a. If the dead end ever opens again, the dead end is the thing
        #     worth measuring, not something to assert away in one line.
        #     The walk reproduces #662's third defect in full and dies
        #     naming every verb that refused.
        if state_now not in DURABLE_STATES:
            self._walk_the_dead_end(
                mgr, prefix,
                artifact=artifact, name=name, path=path,
                disk_size=disk_size, disk_sha=disk_sha,
                state_now=state_now, reconcile_out=reconcile_out,
            )
            return

        # 1b. `reconcile` exits 0 exactly when it found nothing unresolved:
        #     cmd/backupd/reconcile.go prints "reconciliation
        #     complete; no unresolved findings" under `if exitCode == 0`,
        #     and no finding moves exitCode -- only r.Err and Report.Errors
        #     do. So the exit status IS that sentence, and it is the half of
        #     it scripts/ci-local.sh and .github/workflows/ci.yml read.
        #
        #     The status is asserted and the sentence deliberately is not.
        #     In this scenario that summary line is FALSE: there is a fault,
        #     and it is reported on the line above it. Somebody is right to
        #     come back and reword it, and pinning it here would make this
        #     case go red at that improvement instead of at a regression.
        if reconciled.returncode != 0:
            die(
                "reconciliation reported something unresolved over a record the product can now settle "
                "for itself (#662).",
                f"`backupd reconcile` exited {reconciled.returncode} and said: {reconcile_out}",
                "state: " + state_now,
            )
        note(
            f"reconciliation settled the planted record itself: {artifact} is {state_now}, a durable restore point"
        )

        # 2. The operator-visible half, and the one this case exists to
        #    hold. #662's complaint is not only that two records were
        #    believed over the bytes -- it is that the sentence an operator
        #    was handed was built from both of them and gave no way to act.
        #
        #    Two surfaces are searched, and that is not a relaxation. The
        #    engine reconciles on startup, so bringing it back up after the
        #    plant reconciles once before this script ever calls the verb.
        #    A fix that repaired the contradicted row would leave the
        #    explicit call with nothing to say, correctly, and the sentence
        #    would then only be on the transition the journal recorded,
        #    which `backupd artifacts <id>` prints as `reason:` (FR-17).
        #
        #    Measured, not assumed: today it lands on the reconcile stdout
        #    arm, every pass, forever. Reconciliation does NOT write the
        #    row when the verdict is valid, so nothing is consumed and the
        #    contradiction is re-derived and re-reported by every later
        #    pass -- which is what lets an operator who was not watching
        #    the terminal at second zero type `backupd reconcile` and be told.
        #    The `reason:` arm is unreachable BY CONSTRUCTION: the clause
        #    rides a no-action finding (From == To), a no-action finding
        #    calls no lifecycle.Advance, and only the quarantining branches
        #    write a Detail. It is asserted anyway, because if this ever
        #    starts landing on that arm somebody has taught reconciliation
        #    to write on a converged row, and that is worth knowing rather
        #    than worth failing over.
        told = reconcile_out + "\n" + recorded_reason
        missing = [phrase for phrase in FAULT_REPORTED_PHRASES if phrase not in told]
        if missing:
            die(
                "#662: the record was settled without the fault ever reaching an operator.",
                "The copy is at a durable restore point, which is half the fix. But nothing said WHY, so "
                "this deployment carries a journal that disagreed with itself and no operator was told.",
                "absent from both operator-visible surfaces: " + ", ".join(repr(p) for p in missing),
                "`backupd reconcile` said: " + reconcile_out,
                "the journal records this reason: " + (recorded_reason or "(none)"),
                "state: " + state_now,
            )
        note("the fault reached an operator, in words: " + _line_containing(told, FAULT_REPORTED_PHRASES[0]))

        # 3. And none of it touched the file. A fix that resolved the
        #    contradiction by rewriting or re-fetching the bytes would
        #    satisfy everything above and still have lost the only thing
        #    #662 is arguing about.
        still_size = self.size_of(mgr, path)
        still_sha = self.sha256_of(mgr, path)
        if still_sha != disk_sha or still_size != disk_size:
            die(
                "reconciliation changed the file it was supposed to be deciding about.",
                f"before: {disk_size} bytes, sha256 {disk_sha}",
                f"after:  {still_size} bytes, sha256 {still_sha}",
            )
        note(f"the file is untouched: {path} is still {disk_size} bytes, sha256 {still_sha}")

    def _walk_the_dead_end(
        self,
        mgr: str,
        prefix: str,
        *,
        artifact: str,
        name: str,
        path: str,
        disk_size: int,
        disk_sha: str,
        state_now: str,
        reconcile_out: str,
    ) -> None:
        """#662's third defect, kept as a fence and reached only if it returns.

        This was part B's whole body and it was RED ON PURPOSE: every
        documented verb an operator has, walked against the real distroless
        `backupd` inside the manager machine, with the requirement that one of
        them end at a durable restore point. None did.

        It is kept rather than deleted because a fence deleted the day it
        goes green cannot measure the regression it was built for. It runs
        when reconciliation leaves the planted artifact somewhere that is
        not a durable restore point -- which is the dead end opening again
        -- and it dies naming every verb that refused.
        """
        note(
            f"the dead end opened again: reconciliation left {artifact} at {state_now}, which is not a durable restore "
            "point. Walking every verb an operator has, as #662 did."
        )
        note("`backupd reconcile` said: " + reconcile_out)

        # ------------------------------------------------ the walk
        walk: list[str] = []
        accepted_before = self._verbs_that_engage(mgr, prefix, artifact, walk)

        walk.append("validate: " + _verdict(self.bm(
            mgr, prefix, "validate", artifact, "--config", "/etc/backupd/config", check=False)))

        retried = self.bm(
            mgr, prefix, "quarantine", "retry", artifact, "--config", "/etc/backupd/config", check=False
        )
        walk.append("quarantine retry: " + _verdict(retried))

        # Two cycles, with a `retry` between them and none after: that is the
        # loop the issue describes ("FAILED -> DISCOVERED, then the cycle hits
        # the collision -> FAILED. Loops forever"), and stopping on the second
        # FAILED leaves the artifact where the field left it rather than
        # mid-stride in DISCOVERED. The verbs walked after this are therefore
        # asked the same question an operator asks: about an artifact that is
        # FAILED, out of quarantine, with a good file at its final name.
        for cycle in (1, 2):
            self.bm(mgr, prefix, "run", "--config", "/etc/backupd/config", check=False)
            detail = self._journal_record(mgr, prefix, name)
            walk.append("cycle {}: artifact is {}; last failure: {}".format(
                cycle, detail.get("state", ""), detail.get("reason", "(none recorded)")))
            if cycle < 2 and detail.get("state") == "FAILED":
                again = self.bm(mgr, prefix, "retry", artifact, "--config", "/etc/backupd/config", check=False)
                walk.append("retry: " + _verdict(again))

        # The one-way door, measured rather than argued: the verbs that
        # engaged with this artifact before `retry` have to still engage
        # with it afterwards, unless the retry resolved it.
        accepted_after = self._verbs_that_engage(mgr, prefix, artifact, walk, suffix=" (again)")

        walk.append("validate (again): " + _verdict(self.bm(
            mgr, prefix, "validate", artifact, "--config", "/etc/backupd/config", check=False)))
        walk.append("reconcile (again): " + _verdict(self.bm(
            mgr, prefix, "reconcile", "--config", "/etc/backupd/config", check=False)))

        final = self._journal_record(mgr, prefix, name)
        final_state = final.get("state", "")
        still_sha = self.sha256_of(mgr, path)

        forfeited = [verb for verb in accepted_before if verb not in accepted_after]
        if forfeited and final_state not in DURABLE_STATES:
            walk.append(
                "the one-way door: `retry` took away {} and resolved nothing".format(", ".join(forfeited))
            )

        if final_state not in DURABLE_STATES:
            die(
                "#662 defect 3: no documented verb resolves an empty record written over a good file.",
                f"artifact: {artifact}, now {final_state}",
                f"on disk:  {path} is {disk_size} bytes, sha256 {still_sha}, untouched throughout",
                *["  " + line for line in walk],
                "",
                "Every exit is refused, and the one verb that fits the situation -- reinstate, \"I looked, the file",
                "is good, believe it\" -- is the one the natural first move takes away. An operator on a NAS has no",
                "shell to finish this by hand, which is the whole of why #662 is a bug report rather than a support",
                "question. A durable restore point is one of: " + ", ".join(DURABLE_STATES) + ".",
            )

        if still_sha != disk_sha:
            die(
                "a verb in the walk changed the file it was supposed to be deciding about.",
                "before: " + disk_sha,
                "after:  " + still_sha,
            )
        note(f"resolved: {artifact} is {final_state} and the file is untouched")

    def _verbs_that_engage(
        self, mgr: str, prefix: str, artifact: str, walk: list[str], *, suffix: str = ""
    ) -> list[str]:
        """Which quarantine verbs actually look at this artifact.

        The distinction that matters for the one-way door is not the exit
        status -- both a refusal and a declined verdict exit 1 -- but
        whether the verb ENGAGED. `quarantine reinstate` on a QUARANTINED
        artifact re-reads the file and reports `checked=true`; on anything
        else it refuses on state grounds with "is not quarantined" and never
        looks. Losing the second is losing an option; being told no by the
        first is an answer.
        """
        engaged = []
        for verb in ("quarantine revalidate", "quarantine reinstate"):
            result = self.bm(
                mgr, prefix, *verb.split(), artifact, "--config", "/etc/backupd/config", check=False
            )
            said = _verdict(result)
            walk.append(verb + suffix + ": " + said)
            if "checked=" in said and "is not quarantined" not in said:
                engaged.append(verb)
        return engaged


# ---------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------


def _unlink(path: Path) -> None:
    try:
        path.unlink()
    except OSError:
        pass


def _rmdir(path: Path) -> None:
    try:
        path.rmdir()
    except OSError:
        pass


def _combined(proc: subprocess.CompletedProcess[str]) -> str:
    """A command's stdout and stderr as one block, the way `2>&1` reads."""
    out = (proc.stdout or "") + (proc.stderr or "")
    return out.rstrip("\n")


def _verdict(proc: subprocess.CompletedProcess[str]) -> str:
    """One line naming what a verb did: its status and what it said.

    The engine's structured startup lines are dropped. Every `backupd` call logs
    `backupd starting` and `embedded rclone version` as JSON on stderr before it
    does anything, and in a walk of nine verbs that is eighteen lines of
    identical preamble wrapped around the nine sentences somebody has to
    read. Dropping them is not hiding evidence: they say the same thing on
    every line, the full output is on the run's own stdout above, and a
    failure message whose defect is buried in preamble is a failure message
    nobody finishes reading.
    """
    kept = [line for line in _combined(proc).splitlines() if not line.startswith('{"time":')]
    said = " ".join(" ".join(kept).split())
    return "exit {} :: {}".format(proc.returncode, said if said else "(said nothing)")


def _line_containing(text: str, phrase: str) -> str:
    """The one line of a block that carries a phrase, for a note to quote.

    A note that quoted the whole block would bury the sentence somebody is
    being told to read under the engine's JSON startup preamble.
    """
    for line in text.splitlines():
        if phrase in line:
            return line.strip()
    return text.strip()


def _indent(text: str, width: int, *, bar: bool = False) -> str:
    prefix = (" " * width + "| ") if bar else " " * width
    return "\n".join(prefix + line for line in text.rstrip("\n").splitlines())


def _split_apply_receipt(apply_out: str) -> tuple[list[str], list[str]]:
    """The plan the apply printed first, and the receipt it printed after.

    Split on the apply's own ": applied plan " line, so the two halves are
    the decision and the record of carrying it out rather than one list read
    twice.
    """
    planned: list[str] = []
    applied: list[str] = []
    after = False
    for line in apply_out.splitlines():
        if ": applied plan " in line:
            after = True
            continue
        fields = line.split()
        if len(fields) >= 2 and fields[0] == "DELETE":
            (applied if after else planned).append(fields[1])
    return sorted(planned), sorted(applied)


def retention_complaints(
    before: list[tuple[str, str]], after: list[tuple[str, str]], want: Sequence[str]
) -> list[str]:
    """The retention assertion, as a function that says what is wrong.

    That shape is the point, and it is the same one core/service's own
    retentionEvidenceCompare takes: a helper that refused could only ever be
    exercised by a run that was already failing, so nothing could ask it
    "would you notice". This one can be handed a deliberately perturbed
    listing and required to complain.

    An empty `want` is refused rather than answered true about: with nothing
    named for deletion every clause here is vacuously satisfied by an apply
    that did nothing at all.
    """
    if not want:
        return ["the plan named nothing for deletion, so this comparison certifies nothing"]

    complaints: list[str] = []
    after_by_name = dict(after)
    before_names = {name for name, _ in before}
    for name, digest in before:
        now = after_by_name.get(name)
        if now is None:
            if name not in want:
                complaints.append(name + " was removed and no verdict in the plan named it")
        elif name in want:
            complaints.append(name + " is still on disk and the plan marked it DELETE")
        elif now != digest:
            complaints.append(name + " survived the apply but is not the same file")
    for name, _ in after:
        if name not in before_names:
            complaints.append(name + " appeared during the apply, and a retention apply creates nothing")
    return complaints


# ---------------------------------------------------------------------
# Options
# ---------------------------------------------------------------------


def parse_options(argv: Sequence[str], self_path: Path) -> tuple[list[str], bool] | None:
    """Parse the command line, or print the help and say so with None.

    Hand-rolled rather than argparse, and that is a decision rather than an
    omission: `--help` here renders the marker-delimited block at the top of
    this file byte for byte, and `scripts/tests/e2e-help.test.sh` compares it
    against a golden. argparse would wrap it to the terminal width, prepend a
    usage line of its own and reflow every indented case description, so the
    operator-visible text would become a function of COLUMNS. FR-35 clause 4
    says nothing may reword a line an operator already reads.
    """
    cases = "all"
    keep = False
    rest = list(argv)
    while rest:
        arg = rest.pop(0)
        if arg == "--case":
            cases = rest.pop(0) if rest else ""
        elif arg.startswith("--case="):
            cases = arg[len("--case="):]
        elif arg == "--keep-on-failure":
            # Leaves the two containers and the network up after a FAILING
            # case, for reading. Never the default: the whole point of the
            # teardown is that a crashed run leaves nothing behind.
            keep = True
        elif arg in ("-h", "--help"):
            print(harness.render_help(self_path))
            return None
        else:
            die("unknown option " + arg, USAGE)

    if cases == "all":
        case_list = list(CASES)
    elif cases in CASES:
        case_list = [cases]
    else:
        die("unknown case " + cases, "Cases are: " + ", ".join(CASES) + ", all.")
    return case_list, keep


def main(argv: Sequence[str]) -> int:
    harness.set_program("two-machine")
    self_path = Path(__file__).resolve()

    # Parsed BEFORE the teardown and the signal handlers are installed, and
    # that ordering is load bearing rather than incidental: `--help` and
    # `unknown option` must not print a teardown banner for a run that never
    # created anything, and e2e-help.test.sh compares --help's combined
    # output against a golden byte for byte. The bash this replaces got the
    # same property by setting its EXIT trap after the option loop.
    try:
        parsed = parse_options(argv, self_path)
    except harness.Failure as failure:
        return harness.report_failure(failure)
    if parsed is None:
        return harness.EXIT_OK
    case_list, keep = parsed

    proof = Proof(harness.repo_root(self_path), case_list, keep)
    harness.install_signal_handlers()
    return harness.finish(proof.run, teardown=proof.teardown)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
