"""Real end-to-end coverage for deploy_generic.py against an actual SFTP
server and an actual Docker container (issue #82/B4.1's INTEGRATION and
REGRESSION items for the scripted CLI deployment):

    INTEGRATION: run it end to end against a real SFTP fixture, confirm
    one full backup cycle completes.
    REGRESSION: re-run the existing Docker CLI suite against a container
    the script produced, not only a hand-built one.

This mirrors core/tests/sftpfixture's own proven atmoz/sftp-in-Docker
pattern (ed25519 host+client keys, authorized_keys, ssh-keyscan for
known_hosts) rather than inventing a second one, and drives the real
GENERIC container the script itself starts via `docker compose` - not a
hand-built `docker run`.

Skipped automatically wherever docker/ssh-keygen/ssh-keyscan aren't
available, exactly like that Go fixture.

Run with (from the repo root):
    python3 -m unittest scripts.deploy.test_deploy_generic_integration -v
"""

from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import deploy_generic  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[2]
SFTP_USER = "backupuser"
SFTP_UID = "1001"


def _require_tools():
    """Skip rather than fail when this machine cannot run the suite.

    Everything here is checked, docker included, and the daemon is
    probed rather than assumed from the binary being on PATH: `docker`
    installed with nothing to talk to is the normal state of a CI
    container, and a failure there says nothing about the code. Any
    exception at all is a skip, since every way `docker info` can go
    wrong means the same thing to this suite.
    """
    for tool in ("docker", "ssh-keygen", "ssh-keyscan"):
        if shutil.which(tool) is None:
            raise unittest.SkipTest(f"{tool} not found on PATH")
    try:
        subprocess.run(["docker", "info"], capture_output=True, check=True, timeout=10)
    except Exception as exc:  # noqa: BLE001 - any failure here means "skip", not "fail"
        raise unittest.SkipTest(f"docker daemon not reachable: {exc}")


def _sh(*args: str, **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(args, capture_output=True, text=True, timeout=kwargs.pop("timeout", 30), **kwargs)


def _keygen(path: Path, key_type: str = "ed25519", bits: str = "") -> None:
    args = ["ssh-keygen", "-q", "-t", key_type, "-N", "", "-C", "", "-f", str(path)]
    if bits:
        args += ["-b", bits]
    result = _sh(*args)
    if result.returncode != 0:
        raise RuntimeError(f"ssh-keygen failed: {result.stderr}")


class GenericSFTPFixture:
    """Python port of core/tests/sftpfixture's atmoz/sftp setup, scoped to
    exactly what this test needs (a running server plus a client key
    authorized to log in and pull a seeded artifact)."""

    # host.docker.internal is what a container run by `docker compose`
    # needs to reach this fixture's published port: from INSIDE a
    # container, "127.0.0.1" means that container itself, not the host
    # machine the fixture's port is actually published on. This only
    # resolves inside a container (Docker Desktop injects it), never on
    # the host itself - host-side readiness probes below still dial
    # "127.0.0.1" directly.
    CONTAINER_VISIBLE_HOST = "host.docker.internal"

    def __init__(self, run_dir: Path):
        self.run_dir = run_dir
        self.container_id = ""
        self.host = "127.0.0.1"
        self.port = 0
        self.client_key = run_dir / "id_ed25519"
        self.known_hosts = run_dir / "known_hosts"
        self.upload_dir = run_dir / "upload"

    def start(self) -> None:
        self.run_dir.mkdir(parents=True, exist_ok=True)
        host_key_ed25519 = self.run_dir / "ssh_host_ed25519_key"
        _keygen(host_key_ed25519)
        host_key_rsa = self.run_dir / "ssh_host_rsa_key"
        _keygen(host_key_rsa, "rsa", "2048")
        _keygen(self.client_key)

        authorized_dir = self.run_dir / "authorized_keys"
        authorized_dir.mkdir(parents=True, exist_ok=True)
        (authorized_dir / "id_ed25519.pub").write_text((self.client_key.with_suffix(".pub")).read_text())

        self.upload_dir.mkdir(parents=True, exist_ok=True)
        self.upload_dir.chmod(0o777)

        name = f"rclone-manager-deploy-sftp-{int(time.time() * 1000)}"
        _sh("docker", "pull", "atmoz/sftp:alpine", timeout=120)

        run = _sh(
            "docker", "run", "-d", "--name", name,
            "-p", "127.0.0.1::22",
            "-v", f"{host_key_ed25519}:/etc/ssh/ssh_host_ed25519_key:ro",
            "-v", f"{host_key_ed25519}.pub:/etc/ssh/ssh_host_ed25519_key.pub:ro",
            "-v", f"{host_key_rsa}:/etc/ssh/ssh_host_rsa_key:ro",
            "-v", f"{host_key_rsa}.pub:/etc/ssh/ssh_host_rsa_key.pub:ro",
            "-v", f"{authorized_dir}:/home/{SFTP_USER}/.ssh/keys:ro",
            "-v", f"{self.upload_dir}:/home/{SFTP_USER}/upload",
            "atmoz/sftp:alpine",
            f"{SFTP_USER}::{SFTP_UID}:{SFTP_UID}:upload",
        )
        if run.returncode != 0:
            raise RuntimeError(f"docker run (sftp fixture) failed: {run.stderr}")
        self.container_id = run.stdout.strip()

        self.port = self._wait_for_published_port()
        self._keyscan()
        self._wait_for_ssh_ready()

    def _wait_for_published_port(self) -> int:
        """The host port Docker actually chose, once it has chosen one.

        The port is not requested, it is read back, because a fixed one
        collides with whatever else the machine is running and a collision
        here reads as a broken deployment script. Polling rather than one
        read: `docker run` returns before the port mapping is published.
        """
        deadline = time.time() + 15
        while time.time() < deadline:
            result = _sh("docker", "port", self.container_id, "22/tcp")
            if result.returncode == 0 and result.stdout.strip():
                line = result.stdout.strip().splitlines()[0]
                return int(line.rsplit(":", 1)[-1])
            time.sleep(0.2)
        raise RuntimeError("sftp fixture container never published its SSH port")

    def _keyscan(self) -> None:
        """Record the server's host keys, under both names it will be reached by.

        The scanned bytes belong to the server instance and not to the
        address used to reach it, which is why the same key is written a
        second time keyed to the host:port a CONTAINER connects through.
        This host process reaches the fixture on 127.0.0.1 and the deployed
        container does not, so a known_hosts with only the first entry
        authenticates the test's own probe and then fails the deployment it
        is supposed to be proving. Real known_hosts files carry several
        patterns per key for the same reason.

        Both key types are waited for, since ssh-keyscan answers as soon as
        the server offers any one of them.
        """
        deadline = time.time() + 15
        while time.time() < deadline:
            result = _sh("ssh-keyscan", "-p", str(self.port), "-t", "rsa,ed25519", "127.0.0.1", timeout=10)
            if result.returncode == 0 and "ssh-ed25519" in result.stdout and "ssh-rsa" in result.stdout:
                # The scanned key BYTES are a property of the server
                # instance, not of which address was used to reach it, so
                # this also writes a second copy of each line keyed to
                # CONTAINER_VISIBLE_HOST/self.port - the host:port a
                # container (rather than this host process) will actually
                # connect through. Real known_hosts files commonly carry
                # multiple hostname patterns for the same key for exactly
                # this reason (e.g. a host reachable by more than one
                # name).
                container_lines = []
                for line in result.stdout.splitlines():
                    if not line or line.startswith("#"):
                        continue  # ssh-keyscan banner comment, not a host-key line
                    parts = line.split(" ", 1)
                    if len(parts) == 2:
                        container_lines.append(f"[{self.CONTAINER_VISIBLE_HOST}]:{self.port} {parts[1]}")
                self.known_hosts.write_text(result.stdout + "\n".join(container_lines) + "\n")
                return
            time.sleep(0.3)
        raise RuntimeError("ssh-keyscan never returned both host key types")

    def _wait_for_ssh_ready(self) -> None:
        # atmoz/sftp forces `internal-sftp` for this account (no shell), so
        # readiness has to be checked by actually speaking SFTP (the `sftp`
        # CLI in batch mode, quitting immediately) rather than running an
        # arbitrary remote command like `ssh ... true` would - the latter
        # is exactly what a ForceCommand internal-sftp account refuses,
        # which is what core/tests/sftpfixture.go avoids entirely by using
        # golang.org/x/crypto/ssh for a bare transport+auth handshake with
        # no session channel at all. `sftp -b -` (batch commands from
        # stdin) is this script's equivalent of that.
        deadline = time.time() + 20
        last_err = None
        while time.time() < deadline:
            result = subprocess.run(
                [
                    "sftp", "-i", str(self.client_key),
                    "-o", "UserKnownHostsFile=" + str(self.known_hosts),
                    "-o", "BatchMode=yes", "-o", "ConnectTimeout=2",
                    "-P", str(self.port), "-b", "-",
                    f"{SFTP_USER}@{self.host}",
                ],
                input="quit\n",
                capture_output=True,
                text=True,
                timeout=10,
            )
            if result.returncode == 0:
                return
            last_err = result.stderr
            time.sleep(0.3)
        raise RuntimeError(f"sftp fixture never became ready: {last_err}")

    def seed_artifact(self, name: str, content: str) -> None:
        """Put a file where the SFTP account will serve it from.

        Written on the host side of the bind mount rather than uploaded,
        because the fixture exists to give the backup something real to
        fetch and staging it through SFTP would be testing the fixture.
        """
        (self.upload_dir / name).write_text(content)

    def stop(self) -> None:
        """Remove the fixture container, if one was ever started.

        Force removal and no error check: this runs from teardown, on a path
        that may be reached because start() failed halfway, and a cleanup
        that raises replaces the real failure with its own.
        """
        if self.container_id:
            _sh("docker", "rm", "-f", self.container_id)


def _compose_container_id(project: str, env_file: Path, timeout: float = 30) -> str:
    """The engine container Compose created, once it exists.

    Asked of Compose by project and service name rather than matched on
    a name pattern, so this is looking at the container the script under
    test actually produced. Polling, because `up -d` returns before the
    container is listed, and a single read here would fail the test for
    being early.
    """
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        result = _sh(
            "docker", "compose", "-p", project, "-f", str(deploy_generic.COMPOSE_FILE),
            "--env-file", str(env_file),
            "ps", "-q", "rclone-manager",
        )
        if result.returncode == 0 and result.stdout.strip():
            return result.stdout.strip().splitlines()[0]
        last = result.stderr
        time.sleep(0.5)
    raise RuntimeError(f"docker compose never reported a container for project {project!r}: {last}")


@unittest.skipUnless(
    __import__("os").environ.get("RUN_DOCKER_INTEGRATION_TESTS") == "1",
    "set RUN_DOCKER_INTEGRATION_TESTS=1 to run the real Docker+SFTP end-to-end suite "
    "(builds/starts real containers; not run by default alongside the fast unit tests)",
)
class DeployGenericIntegrationTest(unittest.TestCase):
    """The end-to-end run: a real SFTP server, the real script, one real
    backup cycle.

    Behind an environment variable rather than an automatic skip on
    tool availability, unlike the fixture's own checks. It builds and
    starts containers and takes tens of seconds, so it is opt-in
    alongside the fast unit suite; the tool checks then decide whether
    an opted-in run can proceed at all.
    """
    def setUp(self):
        _require_tools()
        self.tmp = Path(tempfile.mkdtemp(prefix="deploy-generic-it-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

        self.fixture = GenericSFTPFixture(self.tmp / "sftp")
        self.fixture.start()
        self.addCleanup(self.fixture.stop)

        self.project = f"bm-deploy-it-{int(time.time())}"
        self.state_dir = self.tmp / "state"
        self.backup_dir = self.tmp / "backups"
        self.deploy_dir = self.tmp / "deploy"
        self.env_file = self.deploy_dir / ".env"
        for d in (self.state_dir, self.backup_dir):
            d.mkdir(parents=True, exist_ok=True)
            d.chmod(0o777)

        self.addCleanup(self._compose_down)

    def _compose_down(self):
        """Take the deployed stack down, volumes included.

        Volumes too, because the next run in this suite has to start from
        nothing: a surviving state database would let a second run pass on
        the artifacts the first one catalogued.
        """
        _sh(
            "docker", "compose", "-p", self.project, "-f", str(deploy_generic.COMPOSE_FILE),
            "--env-file", str(self.env_file),
            "down", "-v", "--remove-orphans",
        )

    def _deploy_args(self) -> list[str]:
        return [
            "--ssh-key", str(self.fixture.client_key),
            "--known-hosts", str(self.fixture.known_hosts),
            # The deployed backup-manager container reaches the fixture
            # through the host's published port via
            # CONTAINER_VISIBLE_HOST, NOT self.fixture.host ("127.0.0.1"
            # would mean the backup-manager container itself, not this
            # host machine, from inside that container).
            "--host", GenericSFTPFixture.CONTAINER_VISIBLE_HOST,
            "--port", str(self.fixture.port),
            "--user", SFTP_USER,
            # Absolute (core/internal/config's own validation requires
            # it), and "upload" because that's the fixed writable
            # subdirectory the fixture's chrooted SFTP account exposes
            # its home directory as (see GenericSFTPFixture.start's own
            # `docker run` args: "...:upload").
            "--remote-path", "/upload",
            "--include", "*.dump",
            "--poll-interval", "3s",
            "--stale-after", "1h",
            "--state-dir", str(self.state_dir),
            "--backup-dir", str(self.backup_dir),
            "--deploy-dir", str(self.deploy_dir),
            "--project-name", self.project,
        ]

    def test_deploys_a_working_container_and_completes_one_backup_cycle(self):
        self.fixture.seed_artifact("backup.dump", "integration test payload")

        code = deploy_generic.main(self._deploy_args())
        self.assertEqual(code, 0, "deploy_generic.py should deploy successfully")

        container_id = _compose_container_id(self.project, self.env_file)

        # INTEGRATION: one full backup cycle completes - the scheduler
        # inside the real container pulls the seeded remote artifact down
        # into the real host BACKUP_DIR.
        landed = self.backup_dir / "backup.dump"
        deadline = time.time() + 30
        while time.time() < deadline and not landed.exists():
            time.sleep(0.5)
        if not landed.exists():
            logs = _sh("docker", "logs", container_id)
            self.fail(
                f"expected {landed} to exist after a real backup cycle\n"
                f"container logs:\n{logs.stdout}\n{logs.stderr}"
            )
        self.assertEqual(landed.read_text(), "integration test payload")

        # REGRESSION (section 67's shape, against a script-produced
        # container): `status` reports HEALTHY for a freshly landed,
        # known-good backup, run inside the SAME container the script
        # started - not a hand-built one.
        status = _sh("docker", "exec", container_id, "/backup-manager", "status")
        self.assertEqual(status.returncode, 0, f"status: {status.stdout}\n{status.stderr}")
        self.assertIn("HEALTHY", status.stdout)

        # REGRESSION: healthcheck tracks that same status, on the
        # script-produced container.
        health = _sh("docker", "inspect", "-f", "{{.State.Health.Status}}", container_id)
        self.assertIn(health.stdout.strip(), ("starting", "healthy"))

        # Idempotency: re-running the script against the SAME project must
        # converge onto the SAME container, not create a second one.
        code_again = deploy_generic.main(self._deploy_args())
        self.assertEqual(code_again, 0)
        container_id_again = _compose_container_id(self.project, self.env_file)
        self.assertEqual(container_id, container_id_again,
                          "re-running the script must converge onto the same container, not duplicate it")


if __name__ == "__main__":
    unittest.main()
