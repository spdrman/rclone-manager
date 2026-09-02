#!/usr/bin/env python3
"""Install rclone-manager on a Docker host, or refuse and say exactly why.

Issue #262. Proven on a UGREEN NAS (issue #263): x86_64, Linux 6.12.30+,
Docker 29.4.3, Compose v5.1.3, and an SSH account that is NOT root, has
NO passwordless sudo, and is in the `docker` group.

That last fact decides most of what follows. This installer cannot
`chown`, cannot install a package, cannot bind a privileged port and
cannot write outside what its own uid already owns. Everything below is
built to work inside that, and to refuse clearly when something needs
more.

# What it installs

`container/compose.yaml` is the canonical runtime contract (issue #167),
and `distribution/compose` fails the build when a derived artifact stops
matching it. So this installer does not write a compose file. It COPIES
the canonical one and composes a two-line override beside it that pins
the image, which is the one field an installed deployment has to settle
and the one field the canonical file deliberately leaves as a build.
An installer that restated the stack would be a tenth adapter nobody
registered.

Everything else that varies per host goes in `.env`, which is exactly
what `container/.env.example` documents it for.

# What "installed" means here

Not "the container started". Two shipped behaviours decide the success
condition:

  * the engine's start gate is a LIVENESS probe and deliberately not
    `backup-manager status` (issue #206), because `status` is a backup
    freshness verdict that a fresh install legitimately fails, and gating
    the UI on it means the page you would fix a backup problem from never
    loads;
  * a fresh install with no config serves a first-run setup flow rather
    than refusing to start (issue #176).

So `install` succeeds when Docker reports the engine healthy by its own
probe, AND the Web UI serves its bundle, AND a request through the Web UI
reaches the engine. The third is separate from the second because a real
install proved they are different claims: on a UGREEN NAS whose Docker
cannot pass container-originated traffic, the bundle served perfectly and
not one API call could get through. Anything less is a verification
failure with its own exit code, and the stack is left up for inspection
rather than torn down under you.

# Credentials

The SSH private key is never read, never copied, never generated and
never printed. Only its host-side PATH reaches `.env`, which is the
convention `container/.env.example` states ("Nothing in this file is a
secret: it only points at where secrets live on the host") and the rule
`scripts/deploy/deploy_generic.py` already holds itself to. The same goes
for a non-default SSH port on a backup source: it is an input, never a
default and never a value written into this repository (issue #264).

# Dependencies

Standard library only, and Python 3.8+. A NAS appliance may not let you
install anything, so an installer with its own dependencies is an
installer that cannot run. Nothing here imports outside the standard
library, and every external tool it needs (docker, docker compose) is
checked for before anything is created.

# Bridge networking

The installer also checks whether a bridged container on this host can
originate traffic at all, and repairs it when it cannot (issue #271). That
is not a general-purpose firewall tool: it is here because the Web UI
reaches the engine over exactly that hop, and so does every SFTP transfer,
so a host that cannot pass it cannot run this product.

It diagnoses by measurement rather than by reading: counters, then the
failing traffic, then counters again, and it names the rule whose counter
moved. Remediation escalates through one announced `sudo` call, inserts
only interface-scoped rules, never flushes anything, never changes a chain
policy, is idempotent and is reversible with `network-undo`. A host whose
bridge networking already works is a no-op and is never asked for a
password. `--fix-network=never` skips all of it.

Usage:
    python3 install_docker_host.py preflight  [options]
    python3 install_docker_host.py install    [options]
    python3 install_docker_host.py status     [options]
    python3 install_docker_host.py uninstall  [options]
    python3 install_docker_host.py network-doctor [options]
    python3 install_docker_host.py network-undo   [options]

Run --help for the full flag list.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

# ---------------------------------------------------------------------
# Exit codes
# ---------------------------------------------------------------------
#
# A bare non-zero exit tells an operator that something went wrong and
# nothing about what to do next, and on a headless NAS "read the log" is
# often not available. Every refusal below has its own code, so a wrapper
# script or a later run can branch on the reason rather than parse prose.

EXIT_OK = 0
EXIT_USAGE = 2

EXIT_PREREQ_PYTHON = 10
EXIT_PREREQ_ARCH = 11
EXIT_PREREQ_DOCKER = 12
EXIT_PREREQ_COMPOSE = 13
EXIT_PREREQ_PATHS = 14
EXIT_PREREQ_PORT = 15
EXIT_PREREQ_SPACE = 16
EXIT_PREREQ_CREDENTIALS = 17
EXIT_PREREQ_IMAGE = 18
EXIT_PREREQ_PAYLOAD = 19

EXIT_EXISTING_INSTALL = 20
EXIT_RUNTIME = 30
EXIT_VERIFY = 31

# Bridge networking (issue #271). Its own block, because these are the only
# codes that can be reached after a password prompt, and an operator reading
# a wrapper's exit status should be able to tell "I could not ask you" from
# "you said no" from "the host would not let you".
EXIT_SUDO_NO_TTY = 40
EXIT_SUDO_WRONG_PASSWORD = 41
EXIT_SUDO_NOT_PERMITTED = 42
EXIT_NETWORK_BROKEN = 43
EXIT_NETWORK_STILL_BROKEN = 44
EXIT_NETWORK_UNDIAGNOSED = 45
EXIT_PERSISTENCE_UNVERIFIED = 46

# The architectures the release manifest claims. Anything else has no
# image, and finding that out from a `docker compose up` failure three
# minutes in is worse than finding it out here.
SUPPORTED_ARCH = {
    "x86_64": "amd64",
    "amd64": "amd64",
    "aarch64": "arm64",
    "arm64": "arm64",
}

# Fixed in-container paths. These mirror container/compose.yaml's own
# volume shape and are never host paths.
CONTAINER_CONFIG_DIR = "/etc/backup-manager/config"
CONTAINER_STATE_DIR = "/data/state"
CONTAINER_BACKUP_DIR = "/data/backups"

# Minimum free space on the filesystem holding the backup directory. Not
# a guess about how big a backup is: it is the floor below which a first
# cycle cannot complete at all, and a full disk mid-transfer is the
# failure this exists to turn into a refusal.
MIN_FREE_BYTES = 2 * 1024 * 1024 * 1024

DEFAULT_PROJECT = "rclone-manager"
DEFAULT_LISTEN_PORT = 8080

# A private key readable by anyone but its owner is what OpenSSH's own
# client refuses outright, so this refuses the same way, at the door.
DISALLOWED_KEY_MODE_BITS = stat.S_IRWXG | stat.S_IRWXO


class Refusal(Exception):
    """A precondition that must stop the run before anything is created.

    Carries the exit code so main() does not have to map messages back to
    reasons, and a `remedy` because "docker is not installed" without
    "install Docker and add this account to the docker group" is half a
    message.
    """

    def __init__(self, code: int, message: str, remedy: str = "") -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.remedy = remedy


# ---------------------------------------------------------------------
# Running things
# ---------------------------------------------------------------------


def run(argv, *, check=True, timeout=None, cwd=None, env=None, input=None):
    """Run a command and keep BOTH streams.

    A subprocess whose stderr is discarded is how an installer reports
    success on a failed step, so nothing here uses DEVNULL and every
    non-zero exit that matters is turned into a Refusal carrying what the
    command actually said.

    `input`, when given, is text delivered on the subprocess's stdin, the
    same as `subprocess.run(..., input=...)`. It exists so a caller that
    needs to pipe a script into a command's stdin - `Sudo.run_script()`'s
    `/bin/sh -s` chief among them - gets the SAME FileNotFoundError and
    TimeoutExpired translation every other call in this file gets, rather
    than calling subprocess.run directly and letting a hang on a live,
    SSH-only, sudo-escalated host surface as a raw traceback instead of a
    coded Refusal.
    """
    try:
        proc = subprocess.run(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            # utf-8/replace, not the platform-default strict decoding
            # text=True alone would use: a headless account on an
            # appliance can plausibly run under a C/POSIX locale, and
            # docker/docker compose output (image names, error text) can
            # carry non-ASCII bytes. A strict-decode failure here would be
            # an unhandled UnicodeDecodeError, not a Refusal, defeating the
            # whole coded-exit-status contract this function exists to
            # provide -- on exactly the calls (docker compose up, docker
            # pull) whose failure the operator most needs a clear message
            # for. probe_web_ui's HTTP body reads already use this same
            # decode discipline for the identical reason.
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
            cwd=cwd,
            env=env,
            input=input,
        )
    except FileNotFoundError as exc:
        raise Refusal(
            EXIT_PREREQ_DOCKER,
            f"{argv[0]} is not on PATH ({exc}).",
            f"Install {argv[0]}, or put it on PATH for this account.",
        ) from exc
    except subprocess.TimeoutExpired as exc:
        raise Refusal(
            EXIT_RUNTIME,
            f"{' '.join(argv)} did not finish within {timeout}s.",
            "Check the Docker daemon is responsive on this host.",
        ) from exc
    if check and proc.returncode != 0:
        raise Refusal(
            EXIT_RUNTIME,
            f"{' '.join(argv)} exited {proc.returncode}.\n"
            f"--- stdout ---\n{proc.stdout.rstrip()}\n"
            f"--- stderr ---\n{proc.stderr.rstrip()}",
            "",
        )
    return proc


def say(message: str) -> None:
    print(message, flush=True)


# ---------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------


class Preflight:
    """Every prerequisite, each with its own refusal and its own code.

    Ordered cheapest and most fundamental first, so an operator on an
    unsupported machine is told that before being told about a port.
    """

    def __init__(self, args) -> None:
        self.args = args
        self.notes = []

    def note(self, text: str) -> None:
        self.notes.append(text)
        say(f"  ok   {text}")

    def check_all(self) -> None:
        self.check_python()
        self.check_arch()
        self.check_docker()
        self.check_compose()
        self.check_payload()
        self.check_paths()
        self.check_credentials()
        self.check_port()
        self.check_space()
        self.check_image()

    # -- the machine ---------------------------------------------------

    def check_python(self) -> None:
        if sys.version_info < (3, 8):
            raise Refusal(
                EXIT_PREREQ_PYTHON,
                f"this installer needs Python 3.8 or newer and is running on {sys.version.split()[0]}.",
                "Run it with a newer python3, or from a machine that has one and target this host over SSH.",
            )
        self.note(f"python {sys.version.split()[0]}")

    def check_arch(self) -> None:
        machine = os.uname().machine
        arch = SUPPORTED_ARCH.get(machine)
        if arch is None:
            raise Refusal(
                EXIT_PREREQ_ARCH,
                f"this host reports {machine}, and the release is built for "
                f"{sorted(set(SUPPORTED_ARCH.values()))} only.",
                "There is no image for this architecture. Building one is a project decision, not an installer flag.",
            )
        self.arch = arch
        self.note(f"architecture {machine} maps to the released {arch}")

    def check_docker(self) -> None:
        if shutil.which("docker") is None:
            raise Refusal(
                EXIT_PREREQ_DOCKER,
                "docker is not on PATH.",
                "Install Docker, then add this account to the docker group and log in again.",
            )
        proc = run(["docker", "version", "--format", "{{.Server.Version}}"], check=False, timeout=30)
        if proc.returncode != 0:
            # The single most common shape on a NAS: the CLI is there and
            # the account cannot reach the socket. Saying "docker failed"
            # sends the operator to reinstall Docker over a group
            # membership.
            detail = (proc.stderr or proc.stdout).strip()
            hint = "Add this account to the docker group (`sudo usermod -aG docker $USER`) and start a new session."
            if "permission denied" in detail.lower():
                hint = (
                    "This account cannot reach the Docker socket. Add it to the docker group and start a new "
                    "session. This installer will not use sudo: it has no way to know that is allowed here."
                )
            raise Refusal(EXIT_PREREQ_DOCKER, f"the Docker daemon is not reachable:\n{detail}", hint)
        self.note(f"docker server {proc.stdout.strip()} is reachable as uid {os.getuid()}")

    def check_compose(self) -> None:
        proc = run(["docker", "compose", "version", "--short"], check=False, timeout=30)
        if proc.returncode != 0:
            raise Refusal(
                EXIT_PREREQ_COMPOSE,
                "`docker compose` is not available:\n" + (proc.stderr or proc.stdout).strip(),
                "Install the Compose v2 plugin. The legacy `docker-compose` script is not a substitute: "
                "this deployment uses `depends_on: condition: service_healthy`, which v1 does not honour.",
            )
        version = proc.stdout.strip().lstrip("v")
        major = version.split(".")[0]
        if not major.isdigit() or int(major) < 2:
            raise Refusal(
                EXIT_PREREQ_COMPOSE,
                f"docker compose reports version {version}, and this deployment needs v2 or newer.",
                "`depends_on: condition: service_healthy` is how the Web UI waits for the engine, and v1 ignores it.",
            )
        self.note(f"docker compose v{version}")

    # -- what we are about to install ----------------------------------

    def check_payload(self) -> None:
        canonical = self.args.compose_file
        if not canonical.is_file():
            raise Refusal(
                EXIT_PREREQ_PAYLOAD,
                f"the canonical compose definition is not at {canonical}.",
                "Point --compose-file at container/compose.yaml from a checkout, or run this from inside one. "
                "This installer copies that file rather than writing its own, so it cannot proceed without it.",
            )
        self.note(f"canonical runtime definition at {canonical}")

    def check_paths(self) -> None:
        """Every host directory, and whether THIS uid can actually use it.

        The three data directories (state, backup, config) are bind-mounted
        into the image, which has no shell, no root step and no init
        process, so nothing inside the container can fix ownership at
        startup: a directory owned by somebody else is a write failure at
        the first SQLite commit, hours later, reported as a database error.
        --prefix is not mounted into the container -- it is where THIS
        process itself writes compose.yaml/.env/compose.image.yaml before
        ever invoking `docker compose` -- but it fails the exact same way
        for the exact same structural reason (this account cannot chown or
        sudo its way past a directory it does not own), so it is checked
        here rather than left to surface as an unhandled OSError partway
        through staging. With no passwordless sudo there is also nothing
        this installer could do about any of these, so it has to be a
        refusal rather than a repair.
        """
        uid, gid = os.getuid(), os.getgid()
        for label, path in self.args.host_dirs.items():
            if path.exists():
                if not path.is_dir():
                    raise Refusal(
                        EXIT_PREREQ_PATHS,
                        f"{label} is {path}, which exists and is not a directory.",
                        "Point it somewhere else, or move whatever is in the way.",
                    )
                st = path.stat()
                if st.st_uid != uid:
                    raise Refusal(
                        EXIT_PREREQ_PATHS,
                        f"{label} is {path}, owned by uid {st.st_uid}, and this installer runs as uid {uid}.",
                        f"The container runs as PUID:PGID and cannot chown anything at startup: the image has no "
                        f"shell and no root step. Either chown it to {uid} yourself, or choose a path this account "
                        f"owns. This installer will not call sudo.",
                    )
                if not os.access(path, os.W_OK | os.X_OK):
                    raise Refusal(
                        EXIT_PREREQ_PATHS,
                        f"{label} is {path}, which this account owns but cannot write to (mode {oct(st.st_mode & 0o777)}).",
                        "Fix its mode, or choose another path.",
                    )
            else:
                parent = path.parent
                probe = parent
                while not probe.exists() and probe != probe.parent:
                    probe = probe.parent
                if not os.access(probe, os.W_OK | os.X_OK):
                    raise Refusal(
                        EXIT_PREREQ_PATHS,
                        f"{label} is {path}, which does not exist, and the nearest existing parent {probe} is "
                        f"not writable by uid {uid}.",
                        "Create it yourself with the right ownership, or choose a path under a directory this "
                        "account owns. This installer creates directories, and it never creates them as root.",
                    )
        self.note(f"every host directory is usable by uid {uid}:{gid}")

    def check_credentials(self) -> None:
        """Validate the key by its filesystem entry, never by its contents.

        `container/compose.yaml` mounts SSH_KEY_FILE and KNOWN_HOSTS_FILE
        with `:?`, so both are required for the stack to come up at all.
        Neither is ever opened here: Docker's read-only bind mount and
        rclone inside the container are the only things that read the
        key, which is the same property core's own File key resolver has
        and the reason a leak into a log is not merely unlikely but
        impossible from this process.
        """
        key = self.args.ssh_key
        known = self.args.known_hosts
        for label, path in (("--ssh-key", key), ("--known-hosts", known)):
            if not path.exists():
                raise Refusal(
                    EXIT_PREREQ_CREDENTIALS,
                    f"{label} is {path}, which is not there.",
                    "container/compose.yaml mounts both of these with `:?`, so the stack cannot start without "
                    "them. Create them on this host first. This installer never generates a key and never "
                    "reads one.",
                )
            if not path.is_file():
                raise Refusal(
                    EXIT_PREREQ_CREDENTIALS,
                    f"{label} is {path}, which is not a regular file.",
                    "Both mounts are read-only single files, deliberately: a directory would be a different claim.",
                )
        mode = key.stat().st_mode
        if mode & DISALLOWED_KEY_MODE_BITS:
            raise Refusal(
                EXIT_PREREQ_CREDENTIALS,
                f"--ssh-key is {key} with mode {oct(mode & 0o777)}, readable beyond its owner.",
                f"chmod 600 {key}. OpenSSH's own client refuses a key like this, and so does this installer, "
                f"rather than letting it surface later as an opaque authentication failure inside a container.",
            )
        self.note("ssh key and known_hosts are present, and the key is owner-only")

    def check_port(self) -> None:
        """Refuse a port something else already holds, but not our own.

        Re-running the installer on a working stack must not fail because
        the stack it installed is listening. So a bound port is only a
        refusal when the thing holding it is not this project.
        """
        port = self.args.listen_port
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(1.5)
        try:
            in_use = sock.connect_ex(("127.0.0.1", port)) == 0
        finally:
            sock.close()
        if not in_use:
            self.note(f"host port {port} is free")
            return
        if self._port_is_ours(port):
            self.note(f"host port {port} is held by this project's own web-ui, which is expected on a re-run")
            return
        raise Refusal(
            EXIT_PREREQ_PORT,
            f"host port {port} is already in use by something that is not this project.",
            f"Choose another with --listen-port, or stop whatever holds {port}. "
            f"Publishing over a port in use is how two services end up half-working.",
        )

    def _port_is_ours(self, port: int) -> bool:
        proc = run(
            ["docker", "compose", "-p", self.args.project, "ps", "--format", "json"],
            check=False,
            timeout=60,
            cwd=str(self.args.prefix) if self.args.prefix.exists() else None,
        )
        if proc.returncode != 0 or not proc.stdout.strip():
            return False
        for line in proc.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                # Discarded, not silently: a schema drift in `docker
                # compose ... --format json` across versions (this
                # function's own triple-fallback parsing below already
                # suggests that has happened once) would otherwise make
                # this return a confidently wrong False -- a misleading
                # "port already in use by something that is not this
                # project" refusal on a re-run where the port genuinely IS
                # held by this project's own stack.
                say(f"     (docker compose ps line did not parse as JSON, ignoring it: {line!r})")
                continue
            entries = entry if isinstance(entry, list) else [entry]
            for item in entries:
                if f":{port}->" in (item.get("Publishers") and str(item.get("Publishers")) or ""):
                    return True
                for pub in item.get("Publishers") or []:
                    if isinstance(pub, dict) and pub.get("PublishedPort") == port:
                        return True
                if f"{port}->" in str(item.get("Ports", "")):
                    return True
        return False

    def check_space(self) -> None:
        target = self.args.host_dirs["--backup-dir"]
        probe = target
        while not probe.exists() and probe != probe.parent:
            probe = probe.parent
        usage = shutil.disk_usage(probe)
        if usage.free < MIN_FREE_BYTES:
            raise Refusal(
                EXIT_PREREQ_SPACE,
                f"the filesystem holding {target} has {usage.free // (1024 ** 2)} MiB free, and this refuses "
                f"below {MIN_FREE_BYTES // (1024 ** 2)} MiB.",
                "A first backup cycle cannot complete under that, and a full disk mid-transfer is worse than "
                "not starting. Free space, or point --backup-dir at a larger volume.",
            )
        self.note(f"{usage.free // (1024 ** 3)} GiB free on the filesystem holding {target}")

    def check_image(self) -> None:
        """The image has to be resolvable BEFORE anything is created.

        Three ways, and exactly one of them has to work. Finding out that
        none does after the directories exist and the .env is written is
        a half install, which is the thing this file refuses to ever be.
        """
        ref = self.args.image
        if self.args.image_archive is not None:
            if not self.args.image_archive.is_file():
                raise Refusal(
                    EXIT_PREREQ_IMAGE,
                    f"--image-archive is {self.args.image_archive}, which is not a file.",
                    "Produce it with `docker save <ref> -o <file>` on a machine that has the image.",
                )
            self.note(f"image will be loaded from {self.args.image_archive}")
            return
        proc = run(["docker", "image", "inspect", ref], check=False, timeout=60)
        if proc.returncode == 0:
            self.note(f"image {ref} is already present on this host")
            return
        if self.args.no_pull:
            raise Refusal(
                EXIT_PREREQ_IMAGE,
                f"image {ref} is not on this host and --no-pull was given.",
                "Drop --no-pull, or supply --image-archive, or `docker pull` it first.",
            )
        say(f"  ..   {ref} is not local; trying to pull it")
        pull = run(["docker", "pull", ref], check=False, timeout=1800)
        if pull.returncode != 0:
            raise Refusal(
                EXIT_PREREQ_IMAGE,
                f"image {ref} is not on this host and could not be pulled:\n"
                + (pull.stderr or pull.stdout).strip(),
                "If the release is not published yet, build it elsewhere and bring it over with "
                "`docker save` plus --image-archive.",
            )
        self.note(f"pulled {ref}")


# ---------------------------------------------------------------------
# The install itself
# ---------------------------------------------------------------------


def render_env(args) -> str:
    """The one file this installer authors, and it holds no secret.

    Every value here is a host path, a uid, a port or a timezone. The key
    itself stays wherever SSH_KEY_FILE points, which is the convention
    container/.env.example states in its own first paragraph.
    """
    lines = [
        "# Generated by scripts/install/install_docker_host.py. Safe to read:",
        "# nothing here is a secret, it only points at where secrets live on",
        "# this host. Re-running the installer rewrites this file; the state,",
        "# config and backup directories it points at are never touched.",
        "",
        f"PUID={args.puid}",
        f"PGID={args.pgid}",
        "",
        f"STATE_DIR={args.host_dirs['--state-dir']}",
        f"BACKUP_DIR={args.host_dirs['--backup-dir']}",
        f"CONFIG_DIR={args.host_dirs['--config-dir']}",
        "",
        f"SSH_KEY_FILE={args.ssh_key}",
        f"KNOWN_HOSTS_FILE={args.known_hosts}",
        "",
        f"LISTEN_PORT={args.listen_port}",
        f"PUBLIC_BASE_URL={args.public_base_url}",
        f"TZ={args.timezone}",
        f"RUNTIME_PROFILE={args.profile}",
        "",
        "# The image tag compose resolves. The installer pins this rather",
        "# than letting it default to `dev`, so an installed deployment",
        "# always names the release it is running.",
        f"VERSION={args.image_tag}",
        f"COMMIT={args.image_commit}",
        "",
    ]
    return "\n".join(lines)


def render_image_override(args) -> str:
    """The smallest possible derivation from the canonical definition.

    Two services, one key each. The canonical file carries a `build:`
    block because it is written to be built from a checkout, and an
    installed host has no checkout; this pins the image instead and
    changes nothing else. Everything the runtime contract names still
    comes from the canonical file, unmodified, which is what keeps this a
    derivation rather than a tenth adapter.
    """
    return (
        "# Generated by scripts/install/install_docker_host.py.\n"
        "# Overlaid on the canonical container/compose.yaml, never in place of it.\n"
        "# Two keys per service, and nothing else: every other field, including\n"
        "# the health checks, the security posture and the mounts, stays\n"
        "# whatever the canonical runtime contract says it is.\n"
        "#\n"
        "# pull_policy is here because the canonical definition carries a\n"
        "# `build:` block, which is right for the file's own purpose (it is\n"
        "# written to be built from a checkout) and wrong for an installed\n"
        "# host, which has no checkout. Without this, `docker compose up`\n"
        "# tries to pull the pinned reference and then to build it, and the\n"
        "# build fails on a context directory that is not there. It is not a\n"
        "# hypothetical: it is what the first real install did, with the image\n"
        "# already loaded and sitting on the host.\n"
        "#\n"
        "# `never` rather than `missing`, deliberately. The image was resolved\n"
        "# during preflight, so if it is gone by now something removed it, and\n"
        "# quietly pulling a different copy of a tag is how a host ends up\n"
        "# running something other than what was installed.\n"
        "services:\n"
        f"  rclone-manager:\n    image: {args.image}\n    pull_policy: never\n"
        f"  web-ui:\n    image: {args.image}\n    pull_policy: never\n"
    )


def compose_argv(args):
    return [
        "docker", "compose",
        "-p", args.project,
        "--env-file", str(args.prefix / ".env"),
        "-f", str(args.prefix / "compose.yaml"),
        "-f", str(args.prefix / "compose.image.yaml"),
    ]


def detect_existing(args):
    """What is already here, as three separate facts.

    "Is it installed" is not one question. The directory can exist with
    no stack, the stack can be running from a directory somebody deleted,
    and both are states an operator can genuinely be in.
    """
    payload = (args.prefix / "compose.yaml").is_file() and (args.prefix / ".env").is_file()
    proc = run(["docker", "compose", "-p", args.project, "ps", "-a", "--format", "json"],
               check=False, timeout=60)
    containers = []
    if proc.returncode == 0 and proc.stdout.strip():
        for line in proc.stdout.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                # See _port_is_ours's identical discard: logged, not
                # silent, so a schema drift is visible instead of just
                # producing a wrong verdict about what is installed.
                say(f"     (docker compose ps -a line did not parse as JSON, ignoring it: {line!r})")
                continue
            containers.extend(entry if isinstance(entry, list) else [entry])
    return payload, containers


def _other_containers_from_ps_ndjson(raw: str, project: str):
    """Every entry in a `docker ps --format json` NDJSON stream that is
    NOT part of `project`, as (name, image) pairs.

    Split out from other_running_containers() so the filtering logic - by
    the same com.docker.compose.project label detect_existing() already
    keys off - is a fact this test suite can pin against canned NDJSON,
    the same way _bridge_interfaces_from_network_inspect() is pinned
    against a canned network inspect array.
    """
    others = []
    for line in raw.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            continue
        if f"com.docker.compose.project={project}" in (entry.get("Labels") or ""):
            continue
        others.append((entry.get("Names", "?"), entry.get("Image", "?")))
    return others


def other_running_containers(project: str):
    """Every running container NOT part of `project`, as (name, image)
    pairs, or None if `docker ps` itself could not be asked.

    Exists so restarting the Docker daemon - which restarts EVERY
    container on the host, not just this project's - can say what else
    it is about to disrupt before it does it. The platforms this
    installer targets (CasaOS, TrueNAS, Portainer, Unraid, ZimaOS, OMV)
    are appliances whose whole point is running many unrelated
    workloads, so "install just this one backup tool" and "bounce every
    other container on this machine" are not the same ask, and an
    operator is owed the difference.
    """
    proc = run(["docker", "ps", "--format", "json"], check=False, timeout=60)
    if proc.returncode != 0:
        return None
    return _other_containers_from_ps_ndjson(proc.stdout, project)


def stage_payload(args) -> None:
    args.prefix.mkdir(parents=True, exist_ok=True)
    for path in args.host_dirs.values():
        path.mkdir(parents=True, exist_ok=True)

    # Copy, never rewrite. distribution/compose holds this exact file to
    # runtime-contract.json, so shipping a modified copy would be
    # shipping something no gate has ever checked.
    shutil.copyfile(str(args.compose_file), str(args.prefix / "compose.yaml"))
    (args.prefix / "compose.image.yaml").write_text(render_image_override(args))

    env_path = args.prefix / ".env"
    env_path.write_text(render_env(args))
    os.chmod(env_path, 0o600)


def wait_for_engine_health(args, timeout: int):
    """Docker's own verdict on the engine's liveness probe.

    Read out of the daemon rather than inferred from the container being
    up, and it is the LIVENESS probe specifically: container/compose.yaml
    declares `/backup-manager-web healthcheck --url .../health/live` for
    the engine and explains at length why it is not `backup-manager
    status` (issue #206). A fresh install fails `status` by design, and
    gating on it means the Web UI never starts.
    """
    deadline = time.time() + timeout
    last = "no container yet"
    while time.time() < deadline:
        proc = run(compose_argv(args) + ["ps", "-q", "rclone-manager"], check=False, timeout=60,
                   cwd=str(args.prefix))
        cid = proc.stdout.strip().splitlines()
        if cid:
            inspect = run(
                ["docker", "inspect", "-f", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", cid[0]],
                check=False, timeout=60)
            last = inspect.stdout.strip()
            parts = last.split()
            if len(parts) == 2 and parts[1] == "healthy":
                return last
            if len(parts) == 2 and parts[0] in ("exited", "dead"):
                return last
        time.sleep(3)
    return f"timed out after {timeout}s, last seen: {last}"


def probe_web_ui(args, timeout: int):
    """Reachable AND serving the first-run flow, not merely answering.

    Issue #176 shipped a fresh install that serves a setup flow instead of
    refusing to start, and issue #211 fixed fourteen client paths that
    answered 404 against a real engine. So "the page loads" is a
    meaningful claim now, and this asks for the evidence rather than for
    a 200: the readiness endpoint's own verdict on whether the instance
    is configured.
    """
    base = f"http://127.0.0.1:{args.listen_port}"
    deadline = time.time() + timeout
    result = {"index": None, "ready": None, "body": ""}
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(base + "/", timeout=5) as resp:
                result["index"] = resp.status
                result["body"] = resp.read(4096).decode("utf-8", "replace")
        except (urllib.error.URLError, OSError) as exc:
            result["index"] = f"unreachable: {exc}"
            time.sleep(3)
            continue
        try:
            with urllib.request.urlopen(base + "/health/ready", timeout=5) as resp:
                result["ready"] = (resp.status, resp.read(2048).decode("utf-8", "replace"))
        except urllib.error.HTTPError as exc:
            # A status is a status. 503 not_ready is the right answer for
            # an unconfigured instance and proves the whole path works.
            result["ready"] = (exc.code, exc.read(2048).decode("utf-8", "replace"))
        except (urllib.error.URLError, OSError) as exc:
            # Not a status. The request did not complete, which is what a
            # broken proxy hop looks like from here.
            result["ready"] = ("unreachable", str(exc))
        return result
    return result


def cmd_preflight(args) -> int:
    say("==> Preflight")
    pf = Preflight(args)
    pf.check_all()
    say("==> Every prerequisite is satisfied.")
    return EXIT_OK


def cmd_install(args) -> int:
    say("==> Preflight")
    pf = Preflight(args)
    pf.check_all()

    payload, containers = detect_existing(args)
    running = [c for c in containers if str(c.get("State", "")).lower() == "running"]
    if payload or containers:
        # The decision, stated rather than implied: a second run
        # CONVERGES. It rewrites the deployment files from the current
        # inputs and brings the stack up again, which `docker compose up
        # -d` already does idempotently, and it never touches the state,
        # config or backup directories. Those hold the SQLite journal, the
        # administrator's Argon2id record and the backups themselves, and
        # an installer that can destroy any of them by being run twice is
        # an installer nobody can safely re-run after a reboot.
        if args.if_installed == "refuse":
            raise Refusal(
                EXIT_EXISTING_INSTALL,
                f"an install is already here: {len(containers)} container(s), "
                f"{len(running)} running, payload at {args.prefix} "
                f"{'present' if payload else 'absent'}.",
                "Re-run without --if-installed=refuse to converge it in place, or `uninstall` first.",
            )
        say(f"==> An install is already here ({len(containers)} container(s), {len(running)} running). "
            f"Converging it in place; state, config and backups are not touched.")

    # Before anything is staged, and before the stack comes up, because a
    # host that cannot pass container-originated traffic fails the Web-UI
    # to engine check at the end for a reason no amount of retrying fixes
    # (#271). A healthy host is a no-op here and never asks for a password.
    if args.fix_network != "never":
        diagnose_and_fix(args, args.probe_network)
    else:
        say("==> --fix-network=never: not checking Docker bridge networking, and touching no firewall.")

    say(f"==> Staging the deployment under {args.prefix}")
    stage_payload(args)

    if args.image_archive is not None:
        say(f"==> Loading {args.image_archive}")
        run(["docker", "load", "-i", str(args.image_archive)], timeout=1800)

    say("==> docker compose up -d")
    # --no-build, not merely pull_policy: never. The canonical definition
    # carries a `build:` block, and Compose treats "do not pull" as "build
    # it then", which on an installed host means building against a
    # context directory that is not there. Both were needed: with only
    # the policy, the first real install still failed at
    # `resolve : lstat .../container: no such file or directory`, with
    # the image already loaded and sitting on the host. --no-build is
    # also the honest statement of intent, since an installed host is
    # never the place a release gets built.
    up = run(compose_argv(args) + ["up", "-d", "--no-build", "--remove-orphans"], check=False, timeout=1800,
             cwd=str(args.prefix))
    if up.returncode != 0:
        raise Refusal(
            EXIT_RUNTIME,
            "docker compose up failed:\n"
            f"--- stdout ---\n{up.stdout.rstrip()}\n--- stderr ---\n{up.stderr.rstrip()}",
            "The stack is left as it is rather than torn down, so the containers and their logs are "
            "still there to read.",
        )
    say(up.stdout.rstrip() or up.stderr.rstrip())

    say(f"==> Waiting up to {args.timeout}s for the engine's liveness probe")
    health = wait_for_engine_health(args, args.timeout)
    say(f"     engine: {health}")

    say("==> Probing the Web UI")
    web = probe_web_ui(args, args.timeout)
    say(f"     index:  {web['index']}")
    say(f"     ready:  {web['ready']}")

    # Three conditions, and the third is the one a real install taught me
    # to add. On a UGREEN NAS all of this came up healthy, the Web UI
    # answered 200, and the installer said "Installed." It was not: that
    # host's Docker cannot pass container-originated traffic, so the Web
    # UI could serve its own static bundle and could not reach the engine
    # for a single API call. An operator would have found that out from a
    # page that loads and then fails everything.
    #
    # The static bundle and the proxy are different claims, so they are
    # checked separately. /health/ready is the cheapest request that has
    # to travel the whole path, and its STATUS does not matter here: 503
    # not_ready is the correct answer for a fresh install (issue #176) and
    # is a pass. What fails is not reaching the engine at all.
    ok_health = health.endswith("healthy")
    ok_web = web["index"] == 200
    ok_proxy = isinstance(web["ready"], tuple) and isinstance(web["ready"][0], int)
    if not (ok_health and ok_web and ok_proxy):
        remedy = (
            f"The stack is left up on purpose. Read it with:\n"
            f"  {' '.join(compose_argv(args))} logs --tail=100"
        )
        if ok_health and ok_web and not ok_proxy:
            remedy = (
                "The Web UI serves its own bundle and cannot reach the engine, so every API call from the\n"
                "browser will hang. That hop is container to container over the compose network, and the\n"
                "usual cause is the host, not this deployment: check whether ANY container on this machine\n"
                "can open a TCP connection, with\n"
                "  docker run --rm --network " + args.project + "_internal alpine wget -T5 -O- http://rclone-manager:8080/health/live\n"
                "A host whose bridge networking cannot pass container-originated traffic cannot run this\n"
                "deployment, and cannot reach an SFTP source either, so fixing it is not optional."
            )
        raise Refusal(
            EXIT_VERIFY,
            "the stack started but did not reach the state that counts as installed:\n"
            f"  engine liveness:       {health}\n"
            f"  web ui static bundle:  {web['index']}\n"
            f"  web ui -> engine:      {web['ready']}",
            remedy,
        )

    say("")
    say("==> Installed.")
    say(f"    Web UI:  {args.public_base_url}")
    say(f"    Compose: {' '.join(compose_argv(args))}")
    say("")
    say("    No config.yaml was written, on purpose. Issue #176 shipped a first-run setup flow")
    say("    precisely so that a fresh install does not need one hand-written before it starts.")
    say("    Open the Web UI and follow it. The enrollment link is in the engine's log:")
    say(f"      {' '.join(compose_argv(args))} logs rclone-manager | grep enroll")
    return EXIT_OK


# ---------------------------------------------------------------------
# Docker bridge networking: diagnose it by measurement, then repair it
# ---------------------------------------------------------------------
#
# Issue #271. The installer used to detect that a host could not pass
# container-originated traffic and stop there, telling the operator the
# machine could not run this deployment. That was honest and it was not
# enough: everything needed to find the exact rule and correct it is
# available, and on the machine this was built for the correction is two
# inserted rules.
#
# Three things an earlier diagnosis of the same host got wrong, all three
# encoded below as rules this code follows rather than as prose.
#
#   1. It reported iptables and nft as "not found in PATH". They were at
#      /sbin, and the probe ran as a non-root account whose PATH excludes
#      /sbin, so the whole firewall section of that report was empty for a
#      PATH reason and that section held the answer. NOTHING here concludes
#      "absent" from a bare name: find_tool searches an explicit privileged
#      PATH and returns an absolute path or nothing.
#   2. It blamed forwarding and NAT. A container cannot ping its own bridge
#      gateway, and that packet is delivered locally on the bridge: never
#      forwarded, never NAT'd. So INPUT is in scope too, and a fix aimed
#      only at FORWARD leaves half the fault standing. Both chains are
#      measured, separately.
#   3. It searched the plumbing. The veth is enslaved and up, the neighbour
#      table has the container MACs, ip_forward is 1. ARP resolves, so
#      frames cross and IP packets are dropped afterwards. That is a
#      netfilter rule, and naming it is the whole job.
#
# WHY COUNTERS. There are several plausible culprits here (a missing
# DOCKER chain, a DOCKER-USER DROP, a host firewall jumped to ahead of
# Docker's rules, a policy of DROP with no matching ACCEPT) and they need
# different corrections. Choosing between them by reading the ruleset is
# inference. Reading every DROP rule's counter, generating exactly the
# traffic that fails, and reading them again is measurement: the rule whose
# counter moved by the number of packets sent is the rule doing it. A
# remediation that cannot name the rule it corrects is a guess, and a guess
# applied to a firewall over SSH is how somebody loses a NAS.

# The PATH a privileged tool actually lives on. Explicit, because the
# account this runs as very likely does not have /sbin.
PRIVILEGED_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Every rule this installer adds carries this comment, which is what makes
# the set idempotent (check before insert) and reversible (delete by
# comment). Nothing else in the ruleset is ever touched.
RULE_TAG = "rclone-manager-bridge"

# docker0 is the default bridge's interface, unconditionally: it is a
# hardcoded name in the Docker daemon rather than something derived from a
# network's id, so nothing enumerated below ever needs to produce it.
#
# There used to be a second entry here, the iptables wildcard `br-+`,
# standing in for every user-defined bridge (which Docker names
# `br-<12 hex>`). `+` is iptables' own PREFIX wildcard, not an
# exact-suffix guard: `-i br-+` matches ANY interface whose name starts
# with `br-`, not only the twelve-hex-character ones Docker's bridge
# driver creates. A host bridge an operator named `br-lan` (common on
# router and embedded platforms, and not implausible on the NAS class
# this installer targets) would get the same DOCKER-USER RETURN and INPUT
# ACCEPT this class installs for Docker's own bridges: a real widening of
# what INPUT accepts and what DOCKER-USER lets skip isolation, on an
# interface this installer knows nothing about. BridgeDoctor now asks
# Docker for the exact interfaces instead (see bridge_interfaces()
# below), and every rule names one, never a pattern.
DOCKER_DEFAULT_BRIDGE = "docker0"

# The chains Docker installs. Their ABSENCE is the one finding that points
# at a restart rather than at a rule, so it is checked as its own question.
DOCKER_CHAINS = ("DOCKER", "DOCKER-USER", "DOCKER-ISOLATION-STAGE-1")

# Docker's own chain in the NAT table, separate from the filter-table
# DOCKER chain above (same name, different table). diagnose_and_fix()
# used to print "the NAT rules are intact" on the strength of the filter
# table alone, which is a claim about a table it never read.
DOCKER_NAT_CHAINS = ("DOCKER",)


def find_tool(name: str):
    """Absolute path to a privileged tool, or None.

    Never `shutil.which(name)` on the inherited PATH alone: that is the
    exact mistake that made an earlier diagnosis of this host report an
    empty firewall.
    """
    found = shutil.which(name, path=PRIVILEGED_PATH)
    if found:
        return found
    return shutil.which(name)


class Sudo:
    """One escalation, announced before it happens, and a password that
    exists only in the terminal.

    The password is never written to disk, never put in an environment
    variable, never passed on a command line and never logged. sudo reads
    it from /dev/tty itself; this process never sees it. The script sudo
    runs arrives on sudo's stdin, which is why the password cannot come
    from there and must come from a terminal, and why "no TTY" is a
    first-class refusal rather than a mysterious failure.

    A note worth writing down rather than leaving implicit: on a host
    where this account is in the `docker` group, it can already obtain
    root by running a privileged container, so asking for a password is
    not what stands between this installer and the firewall. What it
    buys is that the escalation is explicit, announced, and auditable
    instead of quietly arriving through the container runtime. That is
    worth the prompt.
    """

    def __init__(self, sudo_path=None) -> None:
        self.sudo = sudo_path or find_tool("sudo")
        self._passwordless = None

    def available(self) -> bool:
        return self.sudo is not None

    def passwordless(self) -> bool:
        """True when sudo needs no password at all. A read-only probe:
        `sudo -n true` never prompts and never runs anything else.

        Through run() rather than a direct subprocess.run, so a hang here
        gets the same coded Refusal every other subprocess call in this
        file gets instead of a raw TimeoutExpired traceback.
        """
        if self._passwordless is None:
            if not self.available():
                self._passwordless = False
            else:
                proc = run([self.sudo, "-n", "true"], check=False, timeout=30)
                self._passwordless = proc.returncode == 0
        return self._passwordless

    def classify(self, stderr: str) -> int:
        low = (stderr or "").lower()
        # Three wordings for the same problem, and sudo picks between them
        # by version and by how it was invoked. The one this was first seen
        # against says "a terminal is required to read the password", which
        # contains none of the words the other two do.
        if ("no tty present" in low or "askpass" in low
                or "terminal is required" in low or "no askpass" in low):
            return EXIT_SUDO_NO_TTY
        if "not in the sudoers" in low or "not allowed to execute" in low or "may not run" in low:
            return EXIT_SUDO_NOT_PERMITTED
        if "incorrect password" in low or "sorry, try again" in low or "authentication failure" in low:
            return EXIT_SUDO_WRONG_PASSWORD
        return EXIT_RUNTIME

    def run_script(self, script: str, *, purpose: str, timeout=300):
        """Run a shell script as root through exactly one sudo call.

        `script` reaches sudo on stdin, so it is never an argument and
        never appears in the process table. Every command inside it is
        printed first, in full, because an operator being asked for a
        password is owed the list of what it will be spent on.

        Through run() rather than a direct subprocess.run: this is the
        one path that escalates to root, on a host reachable only over
        the SSH session the operator is currently using, inserting
        firewall rules. A `sudo -p ... /bin/sh -s` that hangs for the
        full timeout used to raise a raw TimeoutExpired here instead of
        one of this file's own coded exit statuses, the only subprocess
        call in it that did. check=False because the returncode-specific
        classify() below needs proc.stderr even on failure, which
        run()'s own check=True Refusal would have consumed first.
        """
        if not self.available():
            raise Refusal(
                EXIT_SUDO_NOT_PERMITTED,
                "sudo is not on this host, and this step needs root.",
                "Run the installer with --fix-network=never to skip it, or apply the printed commands "
                "yourself as root.",
            )
        say("")
        say(f"==> {purpose}")
        say("    This needs root. Exactly these commands will run, and nothing else:")
        for line in script.strip().splitlines():
            stripped = line.strip()
            if stripped and not stripped.startswith("#"):
                say(f"      {stripped}")
        if self.passwordless():
            say("    sudo needs no password on this host.")
        else:
            say("    sudo will prompt on this terminal. The password is read by sudo itself: this")
            say("    installer never sees it, never stores it and never writes it anywhere.")
        say("")
        proc = run(
            [self.sudo, "-p", "[sudo] password for %p (rclone-manager installer): ", "/bin/sh", "-s"],
            input=script, check=False, timeout=timeout,
        )
        if proc.returncode != 0:
            code = self.classify(proc.stderr)
            detail = (proc.stderr or proc.stdout).strip()
            remedies = {
                EXIT_SUDO_NO_TTY: (
                    "sudo has no terminal to prompt on. Run the installer from an interactive shell "
                    "(`ssh -t host ...` if you are driving it remotely), or run it with "
                    "--fix-network=never and apply the printed commands yourself."
                ),
                EXIT_SUDO_NOT_PERMITTED: (
                    "This account is not allowed to run that as root. Ask whoever administers the host, "
                    "or run with --fix-network=never and apply the printed commands yourself."
                ),
                EXIT_SUDO_WRONG_PASSWORD: (
                    "The password was not accepted. Nothing was changed. Run the installer again."
                ),
            }
            raise Refusal(code, f"the privileged step did not run:\n{detail}",
                          remedies.get(code, "Nothing was changed."))
        return proc


class Ruleset:
    """A parsed iptables filter table, as counters rather than as prose."""

    def __init__(self, text: str) -> None:
        self.text = text
        self.rules = {}   # (chain, num) -> dict
        self.chains = set()
        self.policies = {}
        chain = None
        for line in text.splitlines():
            if line.startswith("Chain "):
                parts = line.split()
                chain = parts[1]
                self.chains.add(chain)
                if "(policy" in line:
                    self.policies[chain] = parts[3].rstrip(")")
                continue
            fields = line.split()
            if not fields or not fields[0].isdigit() or chain is None:
                continue
            self.rules[(chain, int(fields[0]))] = {
                "chain": chain,
                "num": int(fields[0]),
                "packets": _int_or_zero(fields[1]),
                "target": fields[3] if len(fields) > 3 else "",
                "text": " ".join(fields),
            }

    def drops(self):
        return {k: v for k, v in self.rules.items() if v["target"] == "DROP"}


def _int_or_zero(value: str) -> int:
    """iptables abbreviates counters (12M, 494K). A parse that silently
    returned 0 for those would make every abbreviated rule look idle, and
    the busiest DROP on a NAS is exactly the one that gets abbreviated."""
    value = value.strip()
    scale = {"K": 1000, "M": 1000000, "G": 1000000000}
    if value and value[-1] in scale:
        try:
            return int(float(value[:-1]) * scale[value[-1]])
        except ValueError:
            return 0
    try:
        return int(value)
    except ValueError:
        return 0


def _bridge_interfaces_from_network_inspect(raw: str) -> list[str]:
    """The exact interface name Docker gave each bridge network in a
    `docker network inspect <id> [<id> ...]` JSON array.

    Split out from the method that calls docker so the parsing itself is
    a fact this test suite can pin against a canned array, the same way
    Ruleset is pinned against a canned iptables dump rather than a live
    ruleset.

    The default bridge network is always named "bridge" and its interface
    is always "docker0" - a name the daemon hardcodes rather than derives,
    so it is matched by name here rather than by the derivation below.
    Every other bridge network either carries a custom
    `com.docker.network.bridge.name` option, or Docker names its
    interface `br-` followed by the network id's first twelve characters,
    which is the same convention the iptables wildcard this replaces used
    to trust blindly.
    """
    ifaces = {DOCKER_DEFAULT_BRIDGE}
    for net in json.loads(raw):
        if net.get("Name") == "bridge":
            continue
        name = (net.get("Options") or {}).get("com.docker.network.bridge.name")
        if not name:
            net_id = net.get("Id", "")
            if len(net_id) >= 12:
                name = f"br-{net_id[:12]}"
        if name:
            ifaces.add(name)
    return sorted(ifaces)


def _probe_argv(image: str, network: str, gateway: str, probe_host: str, probe_port: int) -> list[str]:
    """The `docker run` argv for BridgeDoctor.probe(), with gateway,
    probe_host and probe_port passed as environment variables rather than
    interpolated into the shell script string.

    The script text itself is a fixed constant, never built from these
    values, so there is nothing here for a value containing shell
    metacharacters to reach: `sh` expands `"$GATEWAY"` etc. as one quoted
    word regardless of what is in it, the same way every other privileged
    invocation in this file builds an argv list rather than a shell
    string. Practical risk was already low (unprivileged, --rm, these are
    the operator's own CLI args, never attacker-controlled), but this file
    is careful about injection everywhere else and this call was the one
    exception.

    The `--` before each value closes the other half, which quoting does
    not reach: quoting stops the SHELL re-parsing a value as syntax, but
    ping and nc still parse their own argv, so a --probe-host beginning
    with a dash was read as a flag rather than a hostname. Confirmed
    against the default --probe-image (busybox:stable, BusyBox v1.36.1):
    `nc -z -4 9` fails with "invalid option -- '4'", `nc -z -- -4 9` fails
    with "bad address '-4'", and both applets accept `--` normally
    otherwise. Not a security boundary either way, since these are the
    operator's own arguments on their own machine, but a probe that
    reports a flag-parsing error as an egress failure is a misleading
    diagnostic, which is the thing this file most tries not to ship.
    """
    script = (
        'ping -c 3 -W 2 -- "$GATEWAY" >/dev/null 2>&1; echo "gateway_rc=$?"; '
        'timeout 6 nc -z -- "$PROBE_HOST" "$PROBE_PORT" >/dev/null 2>&1; '
        'echo "egress_rc=$?"'
    )
    return [
        "docker", "run", "--rm", "--network", network,
        "-e", f"GATEWAY={gateway}",
        "-e", f"PROBE_HOST={probe_host}",
        "-e", f"PROBE_PORT={probe_port}",
        "--entrypoint", "/bin/sh", image, "-c", script,
    ]


class BridgeDoctor:
    """Diagnose, and if asked, repair, Docker bridge networking.

    Deliberately not a Preflight check, despite asking an overlapping
    "must this hold before we proceed" question. Preflight's checks are
    pure: they read and refuse, and nothing about running one changes the
    host. This class's repair path does the opposite on purpose - it
    escalates through sudo and mutates the host firewall - so folding it
    into Preflight would blur the one property that makes Preflight safe
    to run speculatively: that running it is never itself the risk.
    """

    def __init__(self, args, sudo=None) -> None:
        self.args = args
        self.sudo = sudo or Sudo()
        self.iptables = find_tool("iptables")
        self.findings = []
        # Discovered lazily and memoized by bridge_interfaces() below.
        # Tests set this directly, the same way they override self.iptables,
        # so rule_specs() never needs a live Docker daemon to be asserted
        # against.
        self._bridge_interfaces = None

    def bridge_interfaces(self) -> list[str]:
        """Every interface that is actually one of Docker's own bridges on
        this host right now, asked of Docker rather than trusted from an
        iptables wildcard (see DOCKER_DEFAULT_BRIDGE's comment for why
        that mattered).
        """
        if self._bridge_interfaces is None:
            ls = run(["docker", "network", "ls", "--filter", "driver=bridge", "-q"],
                     check=False, timeout=60)
            if ls.returncode != 0:
                raise Refusal(
                    EXIT_NETWORK_UNDIAGNOSED,
                    "could not list this host's Docker networks, so the exact bridge "
                    f"interfaces to scope a firewall rule to cannot be known:\n"
                    f"{(ls.stderr or ls.stdout).strip()}",
                    "Check the Docker daemon is responsive, or run with --fix-network=never.",
                )
            ids = ls.stdout.split()
            if not ids:
                self._bridge_interfaces = [DOCKER_DEFAULT_BRIDGE]
            else:
                inspect = run(["docker", "network", "inspect", *ids], check=False, timeout=60)
                if inspect.returncode != 0:
                    raise Refusal(
                        EXIT_NETWORK_UNDIAGNOSED,
                        f"could not inspect this host's bridge networks:\n"
                        f"{(inspect.stderr or inspect.stdout).strip()}",
                        "Check the Docker daemon is responsive, or run with --fix-network=never.",
                    )
                try:
                    self._bridge_interfaces = _bridge_interfaces_from_network_inspect(inspect.stdout)
                except json.JSONDecodeError as exc:
                    raise Refusal(
                        EXIT_NETWORK_UNDIAGNOSED,
                        f"docker network inspect did not print JSON this installer could parse: {exc}",
                        "Check the Docker daemon is responsive, or run with --fix-network=never.",
                    ) from exc
        return self._bridge_interfaces

    # -- reading the ruleset ------------------------------------------

    def _iptables_dump(self):
        """The whole filter table with counters, read as root.

        Read-only, but still root: iptables refuses an unprivileged
        reader outright ("Permission denied (you must be root)"), which
        is not something a fallback can work around.
        """
        script = f"{self.iptables} -w 5 -L -n -v --line-numbers\n"
        proc = self.sudo.run_script(script, purpose="Read the firewall ruleset (read-only)")
        return Ruleset(proc.stdout)

    def _nat_dump(self):
        script = f"{self.iptables} -w 5 -t nat -L -n -v --line-numbers\n"
        proc = self.sudo.run_script(script, purpose="Read the NAT table (read-only)")
        return Ruleset(proc.stdout)

    # -- the probes, which need no root -------------------------------

    def probe(self, network: str):
        """What a bridged container can actually do. No root anywhere.

        Two questions, deliberately separate, because they traverse
        different chains and an earlier diagnosis of this host conflated
        them: reaching the bridge gateway is INPUT (locally delivered on
        the bridge, never forwarded, never NAT'd), and reaching an
        external endpoint is FORWARD plus NAT.
        """
        image = self.args.probe_image
        gateway = self._gateway_of(network)
        proc = run(_probe_argv(image, network, gateway, self.args.probe_host, self.args.probe_port),
                   check=False, timeout=120)
        out = proc.stdout + proc.stderr
        result = {"gateway": None, "egress": None, "gateway_ip": gateway, "raw": out.strip()}
        for line in out.splitlines():
            if line.startswith("gateway_rc="):
                result["gateway"] = line.split("=", 1)[1].strip() == "0"
            if line.startswith("egress_rc="):
                result["egress"] = line.split("=", 1)[1].strip() == "0"
        return result

    def _gateway_of(self, network: str) -> str:
        proc = run(["docker", "network", "inspect", network,
                    "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}"],
                   check=False, timeout=60)
        gateway = proc.stdout.strip()
        return gateway or "172.17.0.1"

    def ensure_probe_image(self) -> None:
        proc = run(["docker", "image", "inspect", self.args.probe_image], check=False, timeout=60)
        if proc.returncode == 0:
            return
        say(f"  ..   pulling the probe image {self.args.probe_image}")
        pull = run(["docker", "pull", self.args.probe_image], check=False, timeout=600)
        if pull.returncode != 0:
            raise Refusal(
                EXIT_NETWORK_UNDIAGNOSED,
                f"the probe image {self.args.probe_image} is not on this host and could not be pulled:\n"
                + (pull.stderr or pull.stdout).strip(),
                "The probe needs a container with a shell, ping and nc. Point --probe-image at one "
                "this host already has, or run with --fix-network=never to skip the network check "
                "entirely.",
            )

    # -- measurement ---------------------------------------------------

    def measure(self, network: str):
        """Counters, then the failing traffic, then counters again.

        The rule whose counter moved is the rule doing it. Everything
        else in this class is presentation.
        """
        before = self._iptables_dump()
        say("==> Generating the traffic that fails, so the counters can say which rule stops it")
        result = self.probe(network)
        after = self._iptables_dump()

        moved = []
        for key, rule in after.drops().items():
            delta = rule["packets"] - before.drops().get(key, {"packets": 0})["packets"]
            if delta > 0:
                moved.append((rule, delta))
        moved.sort(key=lambda item: item[1], reverse=True)
        return before, result, moved

    # -- remediation ---------------------------------------------------

    def rule_specs(self):
        """Every rule this installer would add, as argv tails.

        Two families, one per chain the measurement can implicate.

        FORWARD: a RETURN at the top of DOCKER-USER, not an ACCEPT. An
        ACCEPT there terminates the FORWARD traversal and would take
        Docker's own inter-network isolation with it, which is precisely
        what this project's topology depends on: the engine is reachable
        only from the Web UI because the two share a network nothing else
        joins. RETURN skips whatever the host firewall jumped to from
        inside DOCKER-USER and hands the decision back to Docker's own
        chains, isolation included.

        INPUT: an ACCEPT scoped to Docker's bridge interfaces, inserted at
        the top. It cannot match anything arriving on a physical
        interface, so it cannot affect SSH, the LAN, or any host service
        as seen from outside. What it restores is the posture a stock
        Docker host already has, where INPUT's policy is ACCEPT and a
        container may talk to its own gateway.
        """
        specs = []
        ifaces = self.bridge_interfaces()
        for iface in ifaces:
            specs.append(("DOCKER-USER", ["-i", iface, "-m", "comment", "--comment", RULE_TAG, "-j", "RETURN"]))
        for iface in ifaces:
            specs.append(("INPUT", ["-i", iface, "-m", "comment", "--comment", RULE_TAG, "-j", "ACCEPT"]))
        return specs

    def insert_script(self) -> str:
        """Insert-if-absent, one line per rule. Idempotent by
        construction: `iptables -C` asks whether the exact rule is already
        there, and only a miss inserts.

        There is no flush here, no policy change and no whole-ruleset
        restore, and there never will be. This runs on a machine reachable
        only over SSH.
        """
        lines = ["set -e"]
        for chain, spec in self.rule_specs():
            tail = " ".join(spec)
            lines.append(f"{self.iptables} -w 5 -C {chain} {tail} 2>/dev/null || "
                         f"{self.iptables} -w 5 -I {chain} 1 {tail}")
        return "\n".join(lines) + "\n"

    def delete_script(self) -> str:
        lines = []
        for chain, spec in self.rule_specs():
            tail = " ".join(spec)
            lines.append(f"{self.iptables} -w 5 -C {chain} {tail} 2>/dev/null && "
                         f"{self.iptables} -w 5 -D {chain} {tail} || true")
        return "\n".join(lines) + "\n"

    # -- persistence ---------------------------------------------------
    #
    # Issue #273. The rules above are runtime inserts: a reboot loses them
    # and so does UGOS rewriting its own ruleset, and the containers come
    # back either way, so the product stops working silently.
    #
    # NOT `netfilter-persistent save`, even though the mechanism is
    # installed, enabled and active on this host with /etc/iptables/rules.v4
    # sitting right there. That command snapshots the ENTIRE live ruleset,
    # so the first save would take UG_INPUT, UG_FORWARD, UG_SSH_INPUT and
    # everything else UGOS owns and write them into a file this project then
    # restores at every boot. rules.v4 being zero bytes today is the whole
    # argument in one fact: nothing has claimed that file yet, and the first
    # thing to claim it inherits somebody else's firewall. It would fight
    # the Control Panel the moment an operator changed anything there, it
    # would silently restore stale UGOS rules after a UGOS update, and it
    # breaks the one property the rules above are careful to keep: never
    # own, reorder or replace a ruleset this installer did not create.
    #
    # So: a unit that owns exactly the four tagged rules and nothing else,
    # re-asserting them with the same check-before-insert the interactive
    # path uses.
    #
    # ONESHOT PLUS TIMER, and the alternatives were considered rather than
    # skipped:
    #
    #   Restart= needs a process to restart. There is nothing here that
    #   stays alive: the work is four checks that take milliseconds. A
    #   Type=oneshot unit with Restart=always and a RestartSec is a timer
    #   built out of the wrong primitive, and it reports as perpetually
    #   activating rather than as a scheduled job anybody can read.
    #
    #   A .path unit watches the filesystem, and netfilter rules are not a
    #   file. There is no path whose modification corresponds to UGOS
    #   rewriting the live ruleset; /etc/iptables/rules.v4 is zero bytes and
    #   nothing here writes it, so watching it would watch nothing happen.
    #
    #   oneshot plus timer is auditable in a way neither of those is:
    #   `systemctl cat` shows every command that will run, and
    #   `systemctl list-timers` shows when it last did and when it next
    #   will. For something that edits a firewall unattended, being legible
    #   afterwards matters more than being clever.
    #
    # The service is ALSO enabled in its own right, ordered after the four
    # units that construct the ruleset, so at boot the rules are in place as
    # part of the boot sequence rather than up to a timer interval later.
    # The timer's OnBootSec is the safety net for the case where that
    # ordering turns out to be wrong on some host.
    SERVICE_UNIT = "rclone-manager-bridge.service"
    TIMER_UNIT = "rclone-manager-bridge.timer"
    UNIT_DIR = "/etc/systemd/system"

    # Two minutes, chosen rather than inherited. The work is four
    # `iptables -C` calls, single-digit milliseconds, so the cost of the
    # interval is not the consideration. What sets it is the failure mode:
    # UGOS rewriting its ruleset is human-triggered, plausibly whenever the
    # Control Panel is touched, so the question is how long a person may be
    # left with a deployment that has quietly stopped working. Two minutes
    # bounds that well inside the time it takes anyone to notice, and it is
    # a small fraction of the engine's own default one-hour poll interval,
    # so a gap cannot swallow a backup cycle.
    REASSERT_INTERVAL = "2min"

    def unit_service_text(self) -> str:
        lines = [
            "[Unit]",
            "Description=rclone-manager: re-assert this deployment's own Docker bridge firewall rules",
            "Documentation=https://github.com/spdrman/rclone-manager/blob/main/docs/install.md",
            "# Ordered after everything that constructs the ruleset, so this runs on top of",
            "# whatever they built rather than underneath it. After= only, never Requires=:",
            "# a host without one of these should still get its rules, not a failed unit.",
            "After=net_serv.service netfilter-persistent.service nftables.service docker.service",
            "",
            "[Service]",
            "Type=oneshot",
            "RemainAfterExit=no",
            "# Quiet on a healthy host. `iptables -C` prints nothing when the rule is already",
            "# there, so a fire that changes nothing says nothing, and this keeps systemd's own",
            "# start and finish lines out of the journal too.",
            "LogLevelMax=warning",
            "# Bounded rather than left to whatever this host's systemd default happens to be",
            "# (typically 90s, not guaranteed): the one place in this feature a timeout would",
            "# otherwise be implicit instead of chosen.",
            "TimeoutStartSec=30s",
        ]
        for chain, spec in self.rule_specs():
            tail = " ".join(spec)
            # The leading `-` matters: systemd runs multiple ExecStart= lines in
            # order and STOPS AT THE FIRST NON-ZERO EXIT by default, skipping the
            # rest. These four rules are independent corrections, and this unit
            # fires unattended every REASSERT_INTERVAL for the life of the
            # machine - if the first rule's check-and-insert fails for any
            # reason (xtables lock contention with the host's own firewall
            # process, most plausibly, during exactly the window this unit
            # exists to react to), the remaining rules must still be attempted
            # rather than silently skipped. `-` keeps systemd logging and
            # naming the one that failed; it only stops treating that failure
            # as a reason to abandon the rest.
            lines.append(f'ExecStart=-/bin/sh -c "{self.iptables} -w 5 -C {chain} {tail} '
                         f'|| {self.iptables} -w 5 -I {chain} 1 {tail}"')
        lines += [
            "",
            "[Install]",
            "WantedBy=multi-user.target",
            "",
        ]
        return "\n".join(lines)

    def unit_timer_text(self) -> str:
        return "\n".join([
            "[Unit]",
            "Description=rclone-manager: periodically re-assert the Docker bridge firewall rules",
            "Documentation=https://github.com/spdrman/rclone-manager/blob/main/docs/install.md",
            "",
            "[Timer]",
            "# The boot safety net, in case the service's own After= ordering is not enough",
            "# on some host.",
            "OnBootSec=1min",
            f"OnUnitActiveSec={self.REASSERT_INTERVAL}",
            "AccuracySec=30s",
            f"Unit={self.SERVICE_UNIT}",
            "",
            "[Install]",
            "WantedBy=timers.target",
            "",
        ])

    def unit_install_script(self) -> str:
        """Write both units, reload, enable, and assert the rules once now.

        Idempotent at the unit level as well as the rule level: the files
        are rewritten with identical content, `systemctl enable` on an
        already-enabled unit is a no-op, and the rules themselves are
        check-before-insert. Running this twice converges.
        """
        systemctl = find_tool("systemctl") or "/bin/systemctl"
        service = f"{self.UNIT_DIR}/{self.SERVICE_UNIT}"
        timer = f"{self.UNIT_DIR}/{self.TIMER_UNIT}"
        return (
            "set -e\n"
            f"cat > {service} <<'RCLONE_MANAGER_UNIT'\n{self.unit_service_text()}RCLONE_MANAGER_UNIT\n"
            f"cat > {timer} <<'RCLONE_MANAGER_UNIT'\n{self.unit_timer_text()}RCLONE_MANAGER_UNIT\n"
            f"{systemctl} daemon-reload\n"
            f"{systemctl} enable {self.SERVICE_UNIT}\n"
            f"{systemctl} enable --now {self.TIMER_UNIT}\n"
            f"{systemctl} start {self.SERVICE_UNIT}\n"
        )

    def unit_remove_script(self) -> str:
        """Take the units away and leave nothing behind.

        Every step tolerates the thing already being gone, because undo has
        to work on a half-installed machine as well as a fully installed
        one, and an undo that fails partway is worse than no undo at all.
        """
        systemctl = find_tool("systemctl") or "/bin/systemctl"
        return (
            f"{systemctl} disable --now {self.TIMER_UNIT} 2>/dev/null || true\n"
            f"{systemctl} disable --now {self.SERVICE_UNIT} 2>/dev/null || true\n"
            f"rm -f {self.UNIT_DIR}/{self.SERVICE_UNIT} {self.UNIT_DIR}/{self.TIMER_UNIT}\n"
            f"{systemctl} daemon-reload\n"
            f"{systemctl} reset-failed {self.SERVICE_UNIT} 2>/dev/null || true\n"
        )

    def restart_docker_script(self) -> str:
        systemctl = find_tool("systemctl") or "/bin/systemctl"
        return f"{systemctl} restart docker\n"

def report_findings(doctor, before, result, moved) -> None:
    say("")
    say("==> What a bridged container can do")
    say(f"     reach its gateway {result['gateway_ip']}: {'yes' if result['gateway'] else 'NO'}")
    say(f"     open TCP to {doctor.args.probe_host}:{doctor.args.probe_port}: {'yes' if result['egress'] else 'NO'}")

    missing = [c for c in DOCKER_CHAINS if c not in before.chains]
    say("")
    say("==> The ruleset")
    say(f"     INPUT policy   {before.policies.get('INPUT', '?')}")
    say(f"     FORWARD policy {before.policies.get('FORWARD', '?')}")
    if missing:
        say(f"     Docker chains MISSING: {', '.join(missing)}")
    else:
        say(f"     Docker chains present: {', '.join(sorted(c for c in DOCKER_CHAINS if c in before.chains))}")

    say("")
    if moved:
        say("==> The rule dropping the packets, by counter delta across the traffic just generated")
        for rule, delta in moved:
            say(f"     +{delta:<5} {rule['chain']} rule {rule['num']}: {rule['text']}")
    else:
        say("==> No DROP rule's counter moved.")


def diagnose_and_fix(args, network: str, sudo=None) -> dict:
    """The whole cycle: probe, and if it is broken, measure, correct and
    prove the correction.

    A healthy host is a no-op and never asks for a password. That is
    checked first, with the probes, which need no root at all: an
    operator who declines the escalation still gets an answer.
    """
    doctor = BridgeDoctor(args, sudo=sudo)
    doctor.ensure_probe_image()

    say("==> Checking whether a bridged container can originate traffic")
    result = doctor.probe(network)
    say(f"     reach its gateway {result['gateway_ip']}: {'yes' if result['gateway'] else 'NO'}")
    say(f"     open TCP to {args.probe_host}:{args.probe_port}: {'yes' if result['egress'] else 'NO'}")

    if result["gateway"] and result["egress"]:
        if args.fix_network == "persist":
            # An explicit request, so it is honoured even though nothing is
            # broken right now. The alternative reading, that a healthy host
            # is always a no-op, would mean persistence could only ever be
            # installed on a host that was currently broken, so an operator
            # would have to wait for the failure they are trying to prevent.
            # `auto`, the default, is still a strict no-op here.
            say("==> Bridge networking is healthy, and --fix-network=persist asks for the rules to be")
            say("    kept that way across reboots, so the unit is installed anyway.")
            install_persistence(BridgeDoctor(args, sudo=sudo), args)
            return {"healthy": True, "changed": True, "moved": [], "method": "persist-only"}
        say("==> Bridge networking is healthy. Nothing to change, and no password needed.")
        return {"healthy": True, "changed": False, "moved": []}

    if args.fix_network == "never":
        raise Refusal(
            EXIT_NETWORK_BROKEN,
            "bridged containers on this host cannot originate traffic, and --fix-network=never was given.",
            "The Web UI reaches the engine over exactly that hop, and so does every SFTP transfer, so "
            "this deployment cannot work until it is corrected. Re-run without --fix-network=never to "
            "have the installer diagnose and repair it.",
        )

    if doctor.iptables is None:
        raise Refusal(
            EXIT_NETWORK_UNDIAGNOSED,
            "bridged networking is broken and no iptables binary was found on "
            f"{PRIVILEGED_PATH}, so the rule cannot be identified.",
            "Install iptables, or correct this by hand. This installer will not guess at a firewall.",
        )

    say("")
    say("==> Bridge networking is broken. Measuring which rule is responsible.")
    say("    The read-only diagnosis below needs root, because iptables refuses an unprivileged reader.")
    before, result, moved = doctor.measure(network)
    report_findings(doctor, before, result, moved)

    if not moved:
        raise Refusal(
            EXIT_NETWORK_UNDIAGNOSED,
            "traffic is being dropped and no DROP rule's counter moved, so this installer cannot name "
            "the rule responsible.",
            "It refuses to apply a correction it cannot justify. The ruleset is printed above; take it "
            "to whoever administers this host. Run with --fix-network=never to install anyway.",
        )

    if args.fix_network == "diagnose":
        say("")
        say("==> --fix-network=diagnose: stopping here. Nothing was changed.")
        raise Refusal(
            EXIT_NETWORK_BROKEN,
            "bridge networking is broken, the responsible rule is named above, and this run was asked "
            "only to diagnose.",
            "Re-run with --fix-network=auto to apply the correction.",
        )

    # Least invasive first, and the evidence chooses. Docker's chains
    # missing means the host's firewall very likely started after dockerd
    # and flushed them, and a restart makes Docker reinstall exactly those
    # chains. That is one reversible command and it says something the
    # rules cannot: that this is a boot-ordering and persistence problem
    # rather than a rule problem. When the chains are all present, a
    # restart reinstalls what is already there and cannot help, so it is
    # skipped rather than tried out of ritual.
    #
    # "All present" is checked in both tables Docker actually installs
    # into, filter and nat, not just the one the rest of this function was
    # already reading. The nat table used to go unread while a message
    # claimed it was intact anyway.
    missing = [c for c in DOCKER_CHAINS if c not in before.chains]
    nat = doctor._nat_dump()
    nat_missing = [c for c in DOCKER_NAT_CHAINS if c not in nat.chains]
    if missing or nat_missing:
        say("")
        say(f"==> Docker's own chains are missing ({', '.join(missing + nat_missing)}), which points at")
        say("    the host's firewall having flushed them after dockerd installed them. Trying the least")
        say("    invasive correction first: restarting Docker so it reinstalls them.")
        others = other_running_containers(args.project)
        if others is None:
            say("")
            say("    Could not list what else is running on this host (docker ps failed), so the blast")
            say("    radius of a daemon restart below is unknown rather than confirmed small.")
        elif others:
            say("")
            say(f"    Restarting the Docker daemon restarts EVERY container on this host, not only this")
            say(f"    project's. {len(others)} other container(s) are currently running and will be")
            say("    restarted too:")
            for name, image in others:
                say(f"      {name:<30} {image}")
        else:
            say("")
            say("    No other containers are currently running on this host, so the restart below only")
            say("    affects this project's own.")
        doctor.sudo.run_script(doctor.restart_docker_script(),
                               purpose="Restart Docker so it reinstalls its own chains")
        time.sleep(10)
        after = doctor.probe(network)
        if after["gateway"] and after["egress"]:
            say("==> Restarting Docker fixed it. That means the chains were flushed rather than wrong,")
            say("    so this will come back on every boot until the host firewall is ordered before")
            say("    dockerd, or Docker is restarted after it.")
            return {"healthy": True, "changed": True, "moved": moved, "method": "restart"}
        say("==> Restarting Docker did not fix it. Falling through to scoped rules.")
    else:
        say("")
        say("==> Every Docker chain is present in both the filter and NAT tables, so this is not a")
        say("    flushed ruleset and restarting Docker would reinstall what is already there. Skipping")
        say("    it.")

    doctor.sudo.run_script(doctor.insert_script(),
                           purpose="Insert scoped rules for Docker's bridge interfaces")

    say("")
    say("==> Proving the correction rather than assuming it")
    final = doctor.probe(network)
    say(f"     reach its gateway {final['gateway_ip']}: {'yes' if final['gateway'] else 'NO'}")
    say(f"     open TCP to {args.probe_host}:{args.probe_port}: {'yes' if final['egress'] else 'NO'}")
    if not (final["gateway"] and final["egress"]):
        raise Refusal(
            EXIT_NETWORK_STILL_BROKEN,
            "the rules were inserted and a bridged container still cannot originate traffic:\n"
            f"  gateway: {final['gateway']}\n  egress:  {final['egress']}",
            "The rules are still in place and can be removed with:\n"
            "  python3 install_docker_host.py network-undo\n"
            "Take the measurement above to whoever administers this host.",
        )

    if args.fix_network == "persist":
        install_persistence(doctor, args)

    say("")
    say("==> Fixed, and proven. What was added, and how to take it back:")
    for chain, spec in doctor.rule_specs():
        say(f"     {chain}: {' '.join(spec)}")
    say("     remove with: python3 install_docker_host.py network-undo")
    say("")
    if args.fix_network == "persist":
        say(f"     {BridgeDoctor.TIMER_UNIT} re-asserts these every "
            f"{BridgeDoctor.REASSERT_INTERVAL} and at boot, so a reboot and a runtime rewrite of the")
        say("     host firewall both recover on their own. It is still not absolute: the host can")
        say("     rewrite its ruleset between fires, and this hop is down until the next one.")
    else:
        say("     THESE RULES DO NOT SURVIVE A REBOOT. They are raw iptables inserts, and this host's own")
        say("     firewall rewrites its ruleset on boot and may rewrite it again at any time. After a")
        say("     reboot the containers come back and this hop is broken again, silently, because nothing")
        say("     re-runs the installer. Re-run with --fix-network=persist to install a unit that keeps")
        say("     them, or read docs/install.md for the durable fix in the host firewall's own config.")
    return {"healthy": True, "changed": True, "moved": moved,
            "method": "persist" if args.fix_network == "persist" else "rules"}


def persistence_complaints(service_unit, service_state, service_active,
                           timer_unit, timer_state, timer_active, timer_listed):
    """Every way the systemd state read back after installing the
    persistence unit and timer disagrees with "armed and will fire".

    Split out so the verdict is a fact this test suite can pin against
    canned `systemctl` output, the same way this file's other read-back
    logic is. A oneshot service with RemainAfterExit=no is legitimately
    `inactive` right after a successful run - that is not a failure
    signal, which is why the service's own check is "not failed" rather
    than "active", while the timer's is "active": a timer that armed
    correctly stays active/waiting, and one that did not is exactly what
    this exists to catch.
    """
    complaints = []
    if service_state != "enabled":
        complaints.append(f"{service_unit} is-enabled reports {service_state or 'unknown'!r}, not 'enabled'")
    if service_active == "failed":
        complaints.append(f"{service_unit} is-active reports 'failed'")
    if timer_state != "enabled":
        complaints.append(f"{timer_unit} is-enabled reports {timer_state or 'unknown'!r}, not 'enabled'")
    if timer_active != "active":
        complaints.append(f"{timer_unit} is-active reports {timer_active or 'unknown'!r}, not 'active'")
    if not timer_listed:
        complaints.append(f"systemctl list-timers reports no scheduled fire for {timer_unit}")
    return complaints


def install_persistence(doctor, args) -> None:
    """Install the unit and the timer, then read back what systemd thinks,
    and refuse rather than merely report if either did not arm as expected.

    Reading it back matters: `systemctl enable` succeeding says the symlink
    was made, not that the unit is valid or that the timer is armed, and a
    unit file with a typo in it enables perfectly and never runs. Printing
    that read-back and returning anyway restates the same gap one line
    later: "Fixed, and proven" printed over a state nobody checked is not
    proof, and #273's entire point is that a reboot or a runtime rewrite
    has to be recovered from unattended, with nobody there to notice a
    printed status line.
    """
    doctor.sudo.run_script(
        doctor.unit_install_script(),
        purpose=f"Install {doctor.SERVICE_UNIT} and {doctor.TIMER_UNIT} so the rules survive a reboot")
    systemctl = find_tool("systemctl") or "/bin/systemctl"
    states = {}
    for unit in (doctor.SERVICE_UNIT, doctor.TIMER_UNIT):
        state = run([systemctl, "is-enabled", unit], check=False, timeout=30).stdout.strip()
        active = run([systemctl, "is-active", unit], check=False, timeout=30).stdout.strip()
        states[unit] = (state, active)
        say(f"     {unit}: {state or 'unknown'}, {active or 'unknown'}")
    listed = run([systemctl, "list-timers", "--no-pager", "--no-legend", doctor.TIMER_UNIT],
                 check=False, timeout=30).stdout.strip()
    if listed:
        say(f"     next fire: {' '.join(listed.split())}")

    service_state, service_active = states[doctor.SERVICE_UNIT]
    timer_state, timer_active = states[doctor.TIMER_UNIT]
    complaints = persistence_complaints(
        doctor.SERVICE_UNIT, service_state, service_active,
        doctor.TIMER_UNIT, timer_state, timer_active, listed)
    if complaints:
        raise Refusal(
            EXIT_PERSISTENCE_UNVERIFIED,
            "the persistence unit and timer were written and systemctl enable/start reported "
            "success, but reading the state back finds:\n  " + "\n  ".join(complaints),
            f"systemctl enable succeeding says the symlink was made, not that the unit is valid. "
            f"Check `journalctl -u {doctor.SERVICE_UNIT}` and "
            f"`systemctl status {doctor.TIMER_UNIT}` on the host directly.",
        )


def cmd_network_doctor(args) -> int:
    diagnose_and_fix(args, args.probe_network)
    return EXIT_OK


def cmd_network_undo(args) -> int:
    doctor = BridgeDoctor(args)
    if doctor.iptables is None:
        raise Refusal(EXIT_NETWORK_UNDIAGNOSED,
                      f"no iptables binary on {PRIVILEGED_PATH}, so there is nothing to undo with.", "")
    # Units first, then rules. The other order leaves a window in which the
    # timer fires and puts back rules that were just deleted, and an undo
    # that races its own subject is an undo nobody can trust.
    doctor.sudo.run_script(doctor.unit_remove_script() + doctor.delete_script(),
                           purpose="Remove the unit, the timer and the rules this installer added, "
                                   "and only those")
    say("")
    say(f"==> Removed {doctor.SERVICE_UNIT}, {doctor.TIMER_UNIT}, and every rule carrying the comment "
        f"{RULE_TAG}. Nothing else was touched.")
    return EXIT_OK


def cmd_status(args) -> int:
    payload, containers = detect_existing(args)
    say(f"payload at {args.prefix}: {'present' if payload else 'absent'}")
    if not containers:
        say("no containers for project " + args.project)
        return EXIT_OK
    for c in containers:
        say(f"  {c.get('Service', '?'):<16} {c.get('State', '?'):<10} {c.get('Health', '') or 'no healthcheck':<10} {c.get('Status', '')}")

    # The rules install may have inserted are raw iptables inserts: a
    # reboot loses them, and so does the host firewall rewriting its own
    # set. The containers come back either way, so the failure is silent
    # unless something asks. This asks, with no root and no password.
    if args.check_network != "never":
        doctor = BridgeDoctor(args)
        try:
            doctor.ensure_probe_image()
            result = doctor.probe(args.probe_network)
        except Refusal as exc:
            say(f"bridge networking: not checked ({exc.message.splitlines()[0]})")
            return EXIT_OK
        ok = result["gateway"] and result["egress"]
        say(f"bridge networking: {'ok' if ok else 'BROKEN'} "
            f"(gateway {'yes' if result['gateway'] else 'NO'}, "
            f"egress {'yes' if result['egress'] else 'NO'})")
        # Unprivileged, so this costs nothing and answers the question an
        # operator actually has after a reboot: is anything keeping these.
        systemctl = find_tool("systemctl")
        if systemctl:
            state = run([systemctl, "is-enabled", BridgeDoctor.TIMER_UNIT],
                        check=False, timeout=30).stdout.strip()
            if state == "enabled":
                nxt = run([systemctl, "list-timers", "--no-pager", "--no-legend",
                           BridgeDoctor.TIMER_UNIT], check=False, timeout=30).stdout.strip()
                say(f"  persistence: {BridgeDoctor.TIMER_UNIT} enabled"
                    + (f", next fire {' '.join(nxt.split()[:3])}" if nxt else ""))
            else:
                say("  persistence: none. These rules are lost on reboot; "
                    "--fix-network=persist installs a unit that keeps them.")
        if not ok:
            say("  A bridged container cannot originate traffic, so the Web UI cannot reach the engine")
            say("  and no SFTP transfer can run. If this worked before a reboot, the inserted rules were")
            say("  lost: python3 install_docker_host.py network-doctor")
    return EXIT_OK


def cmd_uninstall(args) -> int:
    """Remove what the installer created, and nothing else - with one
    exception, printed below rather than left for an operator to discover.

    `docker compose down` rather than deleting paths, and the state,
    config and backup directories are left where they are unless the
    operator says otherwise in as many words. Those hold the SQLite
    journal, the administrator record and the backups; an uninstall that
    takes them by default is a data-loss bug with a friendly name.

    The exception: if `install` or `network-doctor` ever repaired this
    host's bridge networking (the default, `--fix-network=auto`), that
    repair inserted rclone-manager-bridge-tagged iptables rules directly
    on the host firewall. Detecting whether they are still there needs
    the same root read `_iptables_dump()` does, which this command has
    never otherwise needed - uninstall stays a command that touches only
    what this account already owns. So this prints the exact remedy
    instead of silently claiming a contract nothing here checks.
    """
    payload, containers = detect_existing(args)
    if not payload and not containers:
        say("nothing to uninstall: no payload and no containers for project " + args.project)
        return EXIT_OK
    if payload:
        down = run(compose_argv(args) + ["down", "--remove-orphans"], check=False, timeout=600,
                   cwd=str(args.prefix))
    else:
        down = run(["docker", "compose", "-p", args.project, "down", "--remove-orphans"],
                   check=False, timeout=600)
    say(down.stdout.rstrip() or down.stderr.rstrip())
    if down.returncode != 0:
        raise Refusal(EXIT_RUNTIME, "docker compose down failed:\n" + (down.stderr or down.stdout).strip(), "")

    for name in ("compose.yaml", "compose.image.yaml", ".env"):
        p = args.prefix / name
        if p.exists():
            p.unlink()
            say(f"removed {p}")
    say("")
    say("Left in place, deliberately:")
    for label, path in args.host_dirs.items():
        say(f"  {label:<14} {path}")
    say("They hold the SQLite journal, the administrator record and the backups themselves.")
    say("Remove them by hand if that is really what you want.")
    say("")
    say("NOT removed by this command: if `install` or `network-doctor` ever repaired this host's")
    say("bridge networking, the rclone-manager-bridge-tagged iptables rules it inserted are still on")
    say("the host firewall. Remove exactly those, and nothing else, with:")
    say("  python3 install_docker_host.py network-undo")
    return EXIT_OK


# ---------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------


class _HelpFormatter(argparse.ArgumentDefaultsHelpFormatter, argparse.RawDescriptionHelpFormatter):
    pass


def _add_shared_groups(sp: argparse.ArgumentParser) -> None:
    """layout: every subcommand gets these, and only these, unconditionally.

    resolve() below always builds host_dirs from prefix/state_dir/
    backup_dir/config_dir, and every cmd_* handler needs at least prefix
    and project (compose_argv() and detect_existing() both read only
    these five) - this is the true common floor, not "everything install
    happens to need" the way this function used to define it.
    """
    layout = sp.add_argument_group("layout")
    layout.add_argument("--prefix", type=Path, default=Path("/volume1/backup-manager"),
                        help="Directory the deployment files and the default data directories live under.")
    layout.add_argument("--state-dir", type=Path, default=None,
                        help="Host directory for the SQLite lifecycle journal. Defaults to <prefix>/state.")
    layout.add_argument("--backup-dir", type=Path, default=None,
                        help="Host directory completed artifacts land in. Defaults to <prefix>/backups.")
    layout.add_argument("--config-dir", type=Path, default=None,
                        help="Host directory holding config.yaml. Defaults to <prefix>/config. May be empty: "
                             "a fresh install serves a first-run flow (issue #176).")
    layout.add_argument("--project", default=DEFAULT_PROJECT, help="Compose project name.")


def _add_install_prereq_groups(sp: argparse.ArgumentParser, repo_root: Path) -> None:
    """credentials and the rest of runtime: only preflight and install read
    any of these (every check in the Preflight class, and everything
    cmd_install stages and brings up). status, uninstall, network-doctor
    and network-undo never touch a credential path, an image reference or
    a listen port, so they no longer declare these flags at all.
    """
    creds = sp.add_argument_group("credentials (paths only, never contents)")
    creds.add_argument("--ssh-key", type=Path, default=None,
                       help="Host path to the SFTP client private key. Never read, never generated, never "
                            "printed. Defaults to <prefix>/secrets/id_ed25519.")
    creds.add_argument("--known-hosts", type=Path, default=None,
                       help="Host path to the pinned known_hosts file. Defaults to <prefix>/secrets/known_hosts. "
                            "For a source on a non-default SSH port the entry is keyed [host]:port, and that "
                            "port is yours to supply: it is never defaulted here and never written into this "
                            "repository (issue #264).")

    runtime = sp.add_argument_group("runtime")
    # Here rather than in a second group of its own titled "layout": argparse
    # renders every add_argument_group() call as its own section and never
    # merges two by title, so a second "layout" group put this flag under a
    # duplicate heading in `install --help` and `preflight --help`, which is
    # the opposite of what splitting the parser was for. It reads as runtime
    # anyway: it names the runtime definition, and its scope (preflight and
    # install only) is exactly this group's.
    runtime.add_argument("--compose-file", type=Path, default=repo_root / "container" / "compose.yaml",
                         help="The canonical runtime definition to copy. Not a template: it is copied verbatim.")
    runtime.add_argument("--image", default="ghcr.io/spdrman/backup-manager:0.1.0",
                         help="Image reference both services run.")
    runtime.add_argument("--image-archive", type=Path, default=None,
                         help="A `docker save` tarball to load instead of pulling. For a host that cannot reach "
                              "the registry, or a release that is not published yet.")
    runtime.add_argument("--no-pull", action="store_true",
                         help="Refuse rather than pull if the image is not already on this host.")
    runtime.add_argument("--listen-port", type=int, default=DEFAULT_LISTEN_PORT,
                         help="Host port the Web UI is published on. The engine publishes nothing.")
    runtime.add_argument("--public-base-url", default=None,
                         help="Externally reachable base URL, used for the one-time enrollment link. Defaults "
                              "to http://<this host's name>:<listen port>.")
    runtime.add_argument("--profile", default="generic",
                         help="Runtime profile, from container/compose.yaml's x-canonical-runtime.profiles.")
    runtime.add_argument("--timezone", default=None,
                         help="TZ for both containers. Retention's calendar boundaries depend on it. Defaults "
                              "to this host's /etc/timezone when it has one, else UTC.")
    runtime.add_argument("--puid", type=int, default=None, help="Defaults to this account's uid.")
    runtime.add_argument("--pgid", type=int, default=None, help="Defaults to this account's gid.")
    runtime.add_argument("--timeout", type=int, default=180,
                         help="Seconds to wait for the engine's liveness probe and the Web UI.")


def _add_probe_flags(sp: argparse.ArgumentParser) -> None:
    """--probe-image/--probe-host/--probe-port/--probe-network: every
    command that can ask BridgeDoctor to probe (install, status,
    network-doctor) needs these; preflight, uninstall and network-undo
    never read a probe result at all.
    """
    probe = sp.add_argument_group("bridge probe (issue #271)")
    probe.add_argument("--probe-image", default="busybox:stable",
                       help="A small image with a shell, ping and nc, used to ask what a bridged container "
                            "can actually do. Point it at one this host already has to avoid a pull.")
    probe.add_argument("--probe-host", default="1.1.1.1",
                       help="External endpoint the egress probe opens TCP to. Nothing is sent to it.")
    probe.add_argument("--probe-port", type=int, default=443, help="Port for the egress probe.")
    probe.add_argument("--probe-network", default="bridge",
                       help="Docker network the probe container joins. The rules this installer inserts "
                            "are scoped to docker0 and every bridge network Docker itself reports at "
                            "repair time, so re-running after a network is created covers it too.")


def _add_fix_network_flag(sp: argparse.ArgumentParser, *, default: str, why_this_default: str) -> None:
    net = sp.add_argument_group("bridge networking (issue #271)")
    net.add_argument("--fix-network", choices=["auto", "persist", "diagnose", "never"], default=default,
                     help="auto diagnoses and repairs Docker bridge networking when a bridged container "
                          "cannot originate traffic, escalating through sudo, with rules that are lost on "
                          "reboot. persist does the same and additionally installs a systemd unit and "
                          "timer that re-assert those same rules, which survives a reboot and a runtime "
                          "rewrite of the host firewall. diagnose stops after naming the rule. never "
                          f"skips the check entirely and touches no firewall. {why_this_default}")


def build_parser() -> argparse.ArgumentParser:
    repo_root = Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(
        prog="install_docker_host.py",
        description="Install rclone-manager on a Docker host (issue #262).",
        formatter_class=_HelpFormatter,
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sp_preflight = subparsers.add_parser(
        "preflight", formatter_class=_HelpFormatter,
        help="Check every prerequisite and exit. Changes nothing on the host.")
    _add_shared_groups(sp_preflight)
    _add_install_prereq_groups(sp_preflight, repo_root)

    sp_install = subparsers.add_parser(
        "install", formatter_class=_HelpFormatter,
        help="Bring the engine and Web UI up on this host, or refuse and say why.",
        epilog=(
            "example:\n"
            "  python3 install_docker_host.py install \\\n"
            "      --prefix /volume1/backup-manager \\\n"
            "      --ssh-key /volume1/backup-manager/secrets/id_ed25519 \\\n"
            "      --known-hosts /volume1/backup-manager/secrets/known_hosts \\\n"
            "      --image ghcr.io/spdrman/backup-manager:0.1.0\n"
        ),
    )
    _add_shared_groups(sp_install)
    _add_install_prereq_groups(sp_install, repo_root)
    _add_fix_network_flag(
        sp_install, default="auto",
        why_this_default="auto is the default because a healthy host is a strict no-op either way, so "
                          "there is nothing to opt into for the common case; installing a systemd unit "
                          "(persist) is still a larger commitment than a runtime rule and needs asking for.")
    _add_probe_flags(sp_install)
    existing = sp_install.add_argument_group("existing install")
    existing.add_argument("--if-installed", choices=["converge", "refuse"], default="converge",
                          help="What to do when an install is already here. converge rewrites the deployment "
                               "files and brings the stack up again, never touching state, config or backups.")

    sp_status = subparsers.add_parser(
        "status", formatter_class=_HelpFormatter,
        help="Report what's here, and whether bridge networking still works. Read-only.")
    _add_shared_groups(sp_status)
    check = sp_status.add_argument_group("bridge networking (issue #271)")
    check.add_argument("--check-network", choices=["auto", "never"], default="auto",
                       help="auto asks, unprivileged and read-only, whether a bridged container can still "
                            "originate traffic and reports rclone-manager-bridge.timer's own state "
                            "alongside it. never skips the check. A separate flag from install's own "
                            "--fix-network on purpose: this command only ever reads, it repairs nothing, "
                            "so it needed its own policy rather than borrowing a flag whose name promises "
                            "a fix.")
    _add_probe_flags(sp_status)

    sp_uninstall = subparsers.add_parser(
        "uninstall", formatter_class=_HelpFormatter,
        help="Remove what install created (docker compose down), and nothing else.")
    _add_shared_groups(sp_uninstall)

    sp_doctor = subparsers.add_parser(
        "network-doctor", formatter_class=_HelpFormatter,
        help="Diagnose Docker bridge networking, and repair it if --fix-network says to.")
    _add_shared_groups(sp_doctor)
    _add_fix_network_flag(
        sp_doctor, default="diagnose",
        why_this_default="diagnose is the default here, unlike install's auto: a command named \"doctor\" "
                          "reads as diagnostic, so running it stand-alone to check should not itself "
                          "escalate sudo and mutate the firewall. Pass --fix-network=auto explicitly to "
                          "repair.")
    _add_probe_flags(sp_doctor)

    sp_undo = subparsers.add_parser(
        "network-undo", formatter_class=_HelpFormatter,
        help="Remove exactly the firewall rules and persistence unit this installer added, and nothing else.")
    _add_shared_groups(sp_undo)

    return parser


def resolve(args):
    """Fill in every default this installer computes from --prefix and the
    account it runs as.

    Guarded with hasattr() past the layout group: since #330's subparsers
    split, only preflight and install declare --ssh-key, --compose-file
    and the rest of the install-prerequisite flags (status, uninstall,
    network-doctor and network-undo never read a credential path or an
    image reference), so those attributes are simply absent from the
    Namespace for the other four commands. layout itself (prefix,
    state_dir, backup_dir, config_dir, project) stays unconditional: every
    subcommand declares it, and host_dirs below needs all four regardless
    of which command is running.

    KEEP THESE TWO IN STEP: every hasattr()-guarded field below must also
    be reachable from
    TestSubcommandFlagScoping.test_resolve_never_raises_for_a_command_missing_install_prereq_flags,
    which resolves all six real subparsers. argparse gives no static signal
    here, so a guard nothing exercises is a guard nobody has checked, and
    the failure it is meant to prevent (an AttributeError deep inside a
    privileged repair path, mid-run) only surfaces when that command is
    actually invoked. Adding a seventh subcommand with its own scoped flag
    and a computed default here means adding it to that test in the same
    change.
    """
    args.prefix = args.prefix.expanduser().resolve()
    args.state_dir = (args.state_dir or args.prefix / "state").expanduser()
    args.backup_dir = (args.backup_dir or args.prefix / "backups").expanduser()
    args.config_dir = (args.config_dir or args.prefix / "config").expanduser()
    args.host_dirs = {
        "--prefix": args.prefix,
        "--state-dir": args.state_dir,
        "--backup-dir": args.backup_dir,
        "--config-dir": args.config_dir,
    }
    if hasattr(args, "ssh_key"):
        args.ssh_key = (args.ssh_key or args.prefix / "secrets" / "id_ed25519").expanduser()
    if hasattr(args, "known_hosts"):
        args.known_hosts = (args.known_hosts or args.prefix / "secrets" / "known_hosts").expanduser()
    if hasattr(args, "compose_file"):
        args.compose_file = args.compose_file.expanduser()
    if hasattr(args, "image_archive") and args.image_archive is not None:
        args.image_archive = args.image_archive.expanduser()
    if hasattr(args, "puid") and args.puid is None:
        args.puid = os.getuid()
    if hasattr(args, "pgid") and args.pgid is None:
        args.pgid = os.getgid()
    if hasattr(args, "timezone") and args.timezone is None:
        tzfile = Path("/etc/timezone")
        args.timezone = tzfile.read_text().strip() if tzfile.is_file() else "UTC"
    if hasattr(args, "public_base_url") and args.public_base_url is None:
        args.public_base_url = f"http://{socket.gethostname()}:{args.listen_port}"
    if hasattr(args, "image"):
        # The tag compose resolves for VERSION, taken from the reference so
        # the .env cannot claim a different release from the image.
        ref = args.image
        args.image_tag = ref.rsplit(":", 1)[-1] if ":" in ref.rsplit("/", 1)[-1] else "latest"
        args.image_commit = "none"
    return args


def main(argv) -> int:
    args = resolve(build_parser().parse_args(argv))
    handlers = {
        "preflight": cmd_preflight,
        "install": cmd_install,
        "status": cmd_status,
        "uninstall": cmd_uninstall,
        "network-doctor": cmd_network_doctor,
        "network-undo": cmd_network_undo,
    }
    try:
        return handlers[args.command](args)
    except Refusal as refusal:
        print(f"\nrefusing: {refusal.message}", file=sys.stderr)
        if refusal.remedy:
            print(f"\n{refusal.remedy}", file=sys.stderr)
        print(f"\n(exit {refusal.code})", file=sys.stderr)
        return refusal.code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
