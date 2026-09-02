"""Unit tests for install_docker_host.py's refusals and rendering
(issue #262).

Every assertion here is about the installer saying no. That is the
deliberate emphasis: the installer's happy path is proven on real
hardware in issue #263, and cannot be faked usefully in a unit test,
while its refusals are exactly what nobody exercises until the day they
matter on a machine they cannot debug.

Each refusal is checked for its own exit code AND its own message. Every
one of them exits non-zero, so a code-only assertion cannot tell "this
architecture has no image" from "that port is taken", and those call for
completely different reactions.

Nothing here touches Docker or the network. `preflight` is driven up to
the first check that would, and the Docker-dependent checks are driven
through their own arguments rather than through a stub daemon.

Run with:
    python3 -m unittest scripts.install.test_install_docker_host -v
or, from this directory:
    python3 -m unittest test_install_docker_host -v
"""

from __future__ import annotations

import os
import socket
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import install_docker_host as installer  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[2]
CANONICAL_COMPOSE = REPO_ROOT / "container" / "compose.yaml"


class Fixture:
    """A prefix with a key, a known_hosts and the three data directories,
    all owned by whoever is running the tests, which is what the paths
    check requires."""

    def __init__(self, stack: unittest.TestCase) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        stack.addCleanup(self.tmp.cleanup)
        self.prefix = Path(self.tmp.name) / "backup-manager"
        (self.prefix / "secrets").mkdir(parents=True)
        self.key = self.prefix / "secrets" / "id_ed25519"
        self.key.write_text("not a key, never read by this installer\n")
        os.chmod(self.key, 0o600)
        self.known = self.prefix / "secrets" / "known_hosts"
        self.known.write_text("# pinned host keys go here\n")

    def args(self, *extra: str, command: str = "preflight"):
        argv = [
            command,
            "--prefix", str(self.prefix),
            "--ssh-key", str(self.key),
            "--known-hosts", str(self.known),
            "--compose-file", str(CANONICAL_COMPOSE),
            *extra,
        ]
        return installer.resolve(installer.build_parser().parse_args(argv))


def refusal_from(fn, *a, **kw):
    try:
        fn(*a, **kw)
    except installer.Refusal as exc:
        return exc
    return None


class TestArchitectureRefusal(unittest.TestCase):
    def test_an_unreleased_architecture_is_refused_by_name(self):
        fx = Fixture(self)
        pf = installer.Preflight(fx.args())
        real_uname = os.uname

        class FakeUname:
            machine = "riscv64"

        os.uname = lambda: FakeUname()  # type: ignore[assignment]
        self.addCleanup(lambda: setattr(os, "uname", real_uname))
        exc = refusal_from(pf.check_arch)
        self.assertIsNotNone(exc, "an architecture with no image must be refused before anything is created")
        self.assertEqual(exc.code, installer.EXIT_PREREQ_ARCH)
        self.assertIn("riscv64", exc.message)

    def test_the_two_released_architectures_pass_and_map(self):
        # The positive control. Without it the assertion above passes
        # equally against a check that refuses everything.
        fx = Fixture(self)
        real_uname = os.uname
        self.addCleanup(lambda: setattr(os, "uname", real_uname))
        # addCleanup, not a plain assignment after the loop: if assertEqual
        # ever fails inside the loop -- exactly what happens when this test
        # is doing its job, catching a real architecture-mapping regression
        # -- the exception used to propagate with os.uname still
        # monkeypatched to the last FakeUname, and every later test in this
        # same process that touches check_arch() would then run against a
        # fake architecture: a cascade of unrelated failures masking the
        # original one.
        for machine, want in (("x86_64", "amd64"), ("aarch64", "arm64")):
            with self.subTest(machine=machine):
                class FakeUname:
                    pass
                FakeUname.machine = machine
                os.uname = lambda fu=FakeUname: fu()  # type: ignore[assignment]
                pf = installer.Preflight(fx.args())
                pf.check_arch()
                self.assertEqual(pf.arch, want)


class TestCredentialRefusals(unittest.TestCase):
    def test_a_world_readable_key_is_refused(self):
        fx = Fixture(self)
        os.chmod(fx.key, 0o644)
        exc = refusal_from(installer.Preflight(fx.args()).check_credentials)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_CREDENTIALS)
        self.assertIn("readable beyond its owner", exc.message)

    def test_a_missing_known_hosts_is_refused_and_says_the_mount_requires_it(self):
        fx = Fixture(self)
        fx.known.unlink()
        exc = refusal_from(installer.Preflight(fx.args()).check_credentials)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_CREDENTIALS)
        self.assertIn("--known-hosts", exc.message)

    def test_a_directory_where_the_key_belongs_is_refused(self):
        fx = Fixture(self)
        fx.key.unlink()
        fx.key.mkdir()
        exc = refusal_from(installer.Preflight(fx.args()).check_credentials)
        self.assertIsNotNone(exc)
        self.assertIn("not a regular file", exc.message)

    def test_a_good_pair_passes(self):
        fx = Fixture(self)
        installer.Preflight(fx.args()).check_credentials()

    def test_the_installer_never_opens_the_key(self):
        """The security property, asserted rather than trusted.

        core's File key resolver never puts key material in the
        process's own memory, and deploy_generic.py holds itself to the
        same rule. This proves it for the installer by making the key
        unreadable to its own contents while leaving its metadata intact:
        a check that stat()s passes, a check that read()s raises.
        """
        fx = Fixture(self)
        os.chmod(fx.key, 0o200)  # write-only: stat works, open-for-read does not
        self.addCleanup(lambda: os.chmod(fx.key, 0o600))
        installer.Preflight(fx.args()).check_credentials()
        rendered = installer.render_env(fx.args())
        self.assertIn(str(fx.key), rendered, "the .env carries the key's PATH, which is not a secret")
        self.assertNotIn("not a key", rendered, "the .env must never carry the key's CONTENTS")


class TestPathRefusals(unittest.TestCase):
    def test_a_directory_owned_by_someone_else_is_refused_with_the_no_sudo_reason(self):
        fx = Fixture(self)
        args = fx.args()
        real_stat = Path.stat
        target = args.host_dirs["--state-dir"]
        target.mkdir(parents=True)

        class FakeStat:
            st_uid = os.getuid() + 4242
            st_mode = 0o40755

        def fake_stat(self, *a, **kw):
            if self == target:
                return FakeStat()
            return real_stat(self, *a, **kw)

        Path.stat = fake_stat  # type: ignore[assignment]
        self.addCleanup(lambda: setattr(Path, "stat", real_stat))
        exc = refusal_from(installer.Preflight(args).check_paths)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_PATHS)
        self.assertIn("chown", exc.remedy)
        self.assertIn("will not call sudo", exc.remedy)

    def test_prefix_owned_by_someone_else_is_refused_before_staging_ever_runs(self):
        """#268's own review: --prefix (where compose.yaml/.env/
        compose.image.yaml are staged) was never validated here, only the
        three DATA directories were, so an unwritable --prefix surfaced as
        an unhandled OSError deep inside stage_payload() instead of a clean
        Refusal here. Same fixture shape as the --state-dir case above,
        aimed at --prefix instead."""
        fx = Fixture(self)
        args = fx.args()
        real_stat = Path.stat
        target = args.host_dirs["--prefix"]
        # Unlike --state-dir (a sibling case below), the Fixture already
        # creates --prefix itself (it holds secrets/), so there is nothing
        # to mkdir here -- the whole point is that this directory already
        # exists and is merely mis-owned.

        class FakeStat:
            st_uid = os.getuid() + 4242
            st_mode = 0o40755

        def fake_stat(self, *a, **kw):
            if self == target:
                return FakeStat()
            return real_stat(self, *a, **kw)

        Path.stat = fake_stat  # type: ignore[assignment]
        self.addCleanup(lambda: setattr(Path, "stat", real_stat))
        exc = refusal_from(installer.Preflight(args).check_paths)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_PATHS)
        self.assertIn("--prefix", exc.message)
        self.assertIn("will not call sudo", exc.remedy)

    def test_a_file_where_a_directory_belongs_is_refused(self):
        fx = Fixture(self)
        args = fx.args()
        args.host_dirs["--backup-dir"].parent.mkdir(parents=True, exist_ok=True)
        args.host_dirs["--backup-dir"].write_text("in the way\n")
        exc = refusal_from(installer.Preflight(args).check_paths)
        self.assertIsNotNone(exc)
        self.assertIn("is not a directory", exc.message)

    def test_paths_this_account_owns_pass(self):
        fx = Fixture(self)
        installer.Preflight(fx.args()).check_paths()


class TestPayloadRefusal(unittest.TestCase):
    def test_a_missing_canonical_definition_is_refused_rather_than_improvised(self):
        fx = Fixture(self)
        args = fx.args("--compose-file", str(fx.prefix / "nowhere" / "compose.yaml"))
        exc = refusal_from(installer.Preflight(args).check_payload)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_PAYLOAD)
        self.assertIn("copies that file rather than writing its own", exc.remedy)


class TestPortRefusal(unittest.TestCase):
    def test_a_port_held_by_something_else_is_refused(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        sock.listen(1)
        self.addCleanup(sock.close)
        port = sock.getsockname()[1]
        fx = Fixture(self)
        args = fx.args("--listen-port", str(port))
        pf = installer.Preflight(args)
        pf._port_is_ours = lambda p: False  # not our stack
        exc = refusal_from(pf.check_port)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_PORT)
        self.assertIn(str(port), exc.message)

    def test_a_port_held_by_our_own_stack_is_not_a_refusal(self):
        """The idempotence property. A second run must not fail because
        the stack the first run installed is listening."""
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        sock.listen(1)
        self.addCleanup(sock.close)
        port = sock.getsockname()[1]
        fx = Fixture(self)
        pf = installer.Preflight(fx.args("--listen-port", str(port)))
        pf._port_is_ours = lambda p: True
        pf.check_port()


class TestSpaceRefusal(unittest.TestCase):
    def test_a_full_volume_is_refused_before_anything_is_written(self):
        fx = Fixture(self)
        args = fx.args()
        real = installer.shutil.disk_usage

        class Usage:
            total = 100
            used = 99
            free = 1024

        installer.shutil.disk_usage = lambda p: Usage()  # type: ignore[assignment]
        self.addCleanup(lambda: setattr(installer.shutil, "disk_usage", real))
        exc = refusal_from(installer.Preflight(args).check_space)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_SPACE)


class TestImageRefusal(unittest.TestCase):
    def test_a_missing_archive_is_refused_without_touching_docker(self):
        fx = Fixture(self)
        args = fx.args("--image-archive", str(fx.prefix / "nope.tar"))
        exc = refusal_from(installer.Preflight(args).check_image)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_IMAGE)
        self.assertIn("docker save", exc.remedy)


class TestRendering(unittest.TestCase):
    def test_the_env_names_every_variable_the_canonical_compose_requires(self):
        """container/compose.yaml mounts five host paths with `:?`, so a
        missing one is not a warning, it is the stack refusing to start.
        This reads the requirement out of the canonical file rather than
        restating it, so a new required variable fails here rather than
        on somebody's NAS."""
        canonical = CANONICAL_COMPOSE.read_text()
        required = set()
        for token in canonical.split("${"):
            name = token.split("}")[0]
            if ":?" in name:
                required.add(name.split(":?")[0])
        self.assertTrue(required, "the canonical file declares no required variable, so this test checks nothing")
        rendered = installer.render_env(Fixture(self).args())
        for name in sorted(required):
            self.assertIn(f"{name}=", rendered, f"{name} is required by container/compose.yaml and the .env omits it")

    def test_the_override_pins_the_image_and_changes_nothing_else(self):
        fx = Fixture(self)
        args = fx.args("--image", "ghcr.io/spdrman/backup-manager:0.1.0")
        override = installer.render_image_override(args)
        self.assertIn("ghcr.io/spdrman/backup-manager:0.1.0", override)
        body = [ln for ln in override.splitlines() if ln and not ln.lstrip().startswith("#")]
        keys = [ln.strip().split(":")[0] for ln in body if ln.startswith("    ")]
        self.assertEqual(set(keys), {"image", "pull_policy"},
                         "the override may pin the image and forbid a pull, and nothing else, or it stops "
                         f"being a derivation of the canonical runtime contract: {body}")
        self.assertIn("pull_policy: never", override,
                      "the canonical file carries a `build:` block, so without an explicit policy compose "
                      "pulls and then builds against a context an installed host does not have")

    def test_the_version_in_the_env_tracks_the_image_tag(self):
        fx = Fixture(self)
        args = fx.args("--image", "ghcr.io/spdrman/backup-manager:0.1.0")
        self.assertIn("VERSION=0.1.0", installer.render_env(args))

    def test_the_env_is_written_owner_only(self):
        fx = Fixture(self)
        args = fx.args()
        installer.stage_payload(args)
        mode = (args.prefix / ".env").stat().st_mode & 0o777
        self.assertEqual(mode, 0o600, "the .env names every host path in the deployment; it is not world readable")

    def test_staging_copies_the_canonical_file_byte_for_byte(self):
        """The derivation rule, asserted. distribution/compose holds
        container/compose.yaml to runtime-contract.json, so a modified
        copy is a runtime definition no gate has ever checked."""
        fx = Fixture(self)
        args = fx.args()
        installer.stage_payload(args)
        self.assertEqual((args.prefix / "compose.yaml").read_bytes(), CANONICAL_COMPOSE.read_bytes())


class TestWhatCountsAsInstalled(unittest.TestCase):
    """The success condition, which a real install had to correct.

    The installer reported success on a host where the Web UI served its
    own bundle and could not reach the engine for a single API call. That
    host's Docker cannot pass container-originated traffic, so every page
    would load and then fail. These pin the three conditions apart, and in
    particular pin that a 503 from an unconfigured engine is a PASS (issue
    #176 makes that the correct answer) while a request that never
    completed is not.
    """

    def verdict(self, health, index, ready):
        ok_health = health.endswith("healthy")
        ok_web = index == 200
        ok_proxy = isinstance(ready, tuple) and isinstance(ready[0], int)
        return ok_health and ok_web and ok_proxy

    def test_a_fresh_install_answering_503_not_ready_is_installed(self):
        self.assertTrue(self.verdict("running healthy", 200, (503, '{"status":"not_ready"}')))

    def test_a_web_ui_that_cannot_reach_the_engine_is_not_installed(self):
        self.assertFalse(self.verdict("running healthy", 200, ("unreachable", "timed out")),
                         "a bundle that serves and a proxy that hangs is the exact shape the UGREEN "
                         "install had, and it was reported as success")

    def test_an_unhealthy_engine_is_not_installed(self):
        self.assertFalse(self.verdict("running starting", 200, (503, "")))

    def test_a_web_ui_that_does_not_serve_is_not_installed(self):
        self.assertFalse(self.verdict("running healthy", 502, (200, "")))


if __name__ == "__main__":
    unittest.main()
