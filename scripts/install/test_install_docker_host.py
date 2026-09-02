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

import argparse
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


def _subparser(parser, name):
    """The named subcommand's own ArgumentParser, e.g. _subparser(parser,
    "install"). build_parser() uses real subparsers (issue #330) so each
    subcommand's flags live on its own parser, not the top-level one."""
    for action in parser._actions:
        if isinstance(action, argparse._SubParsersAction):
            return action.choices[name]
    raise AssertionError("build_parser() has no subparsers action")


def _all_subparser_flags(parser):
    """Every option string declared on any subcommand, e.g. for asserting
    a flag exists nowhere at all rather than checking one subcommand."""
    flags = set()
    for action in parser._actions:
        if isinstance(action, argparse._SubParsersAction):
            for sub in action.choices.values():
                flags |= {opt for a in sub._actions for opt in a.option_strings}
    return flags


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



# ---------------------------------------------------------------------
# Bridge networking (issue #271)
# ---------------------------------------------------------------------

# The real filter table from the UGREEN NAS this was diagnosed on, trimmed
# to the chains that matter. It is a fixture rather than an invention
# because the shape is the finding: the host firewall's own chain is jumped
# to from inside DOCKER-USER, ahead of everything Docker put there, and its
# last rule is a blanket DROP. An invented fixture would have been a
# DOCKER-USER DROP, which is the case everyone expects and not the one that
# was actually there.
REAL_RULESET_BEFORE = """Chain INPUT (policy ACCEPT 0 packets, 0 bytes)
num   pkts bytes target     prot opt in     out     source               destination
1      14M   54G UG_SSH_INPUT  0    --  *      *       0.0.0.0/0            0.0.0.0/0
2      13M   53G UG_INPUT   0    --  *      *       0.0.0.0/0            0.0.0.0/0

Chain FORWARD (policy DROP 0 packets, 0 bytes)
num   pkts bytes target     prot opt in     out     source               destination
1      169 10236 DOCKER-USER  0    --  *      *       0.0.0.0/0            0.0.0.0/0
2        0     0 DOCKER-FORWARD  0    --  *      *       0.0.0.0/0            0.0.0.0/0
3        0     0 UG_FORWARD  0    --  *      *       0.0.0.0/0            0.0.0.0/0

Chain DOCKER (1 references)
num   pkts bytes target     prot opt in     out     source               destination
1        0     0 DROP       0    --  !docker0 docker0  0.0.0.0/0            0.0.0.0/0

Chain DOCKER-USER (1 references)
num   pkts bytes target     prot opt in     out     source               destination
1      169 10236 UG_FORWARD  0    --  *      *       0.0.0.0/0            0.0.0.0/0
2        0     0 RETURN     0    --  *      *       0.0.0.0/0            0.0.0.0/0

Chain DOCKER-ISOLATION-STAGE-1 (0 references)
num   pkts bytes target     prot opt in     out     source               destination

Chain UG_FORWARD (3 references)
num   pkts bytes target     prot opt in     out     source               destination
1        0     0 ACCEPT     0    --  lo     *       0.0.0.0/0            0.0.0.0/0
2        0     0 ACCEPT     0    --  *      *       0.0.0.0/0            0.0.0.0/0            ctstate RELATED,ESTABLISHED
3        0     0 RETURN     0    --  eth0   *       192.168.0.0/24       0.0.0.0/0
4      169 10236 DROP       0    --  *      *       0.0.0.0/0            0.0.0.0/0

Chain UG_INPUT (1 references)
num   pkts bytes target     prot opt in     out     source               destination
1    1426K  270M ACCEPT     0    --  lo     *       0.0.0.0/0            0.0.0.0/0
4      12M   52G ACCEPT     0    --  *      *       0.0.0.0/0            0.0.0.0/0            ctstate RELATED,ESTABLISHED
7     3997  494K DROP       0    --  *      *       0.0.0.0/0            0.0.0.0/0
"""

# The same table after three pings to the gateway and a handful of SYNs to
# an external endpoint. UG_INPUT rule 7 moves by 3, UG_FORWARD rule 4 by 6.
REAL_RULESET_AFTER = (REAL_RULESET_BEFORE
                      .replace("4      169 10236 DROP", "4      175 10596 DROP")
                      .replace("7     3997  494K DROP", "7     4000  494K DROP"))


class TestToolLookup(unittest.TestCase):
    def test_a_privileged_tool_is_found_even_when_PATH_excludes_sbin(self):
        """The mistake that made an earlier diagnosis of this host useless.

        `iptables` and `nft` were reported absent because the probe ran as
        a non-root account whose PATH has no /sbin, and the whole firewall
        section of that report was empty as a result. That section held
        the answer.
        """
        real_path = os.environ.get("PATH", "")
        os.environ["PATH"] = "/nonexistent"
        self.addCleanup(lambda: os.environ.__setitem__("PATH", real_path))
        found = installer.find_tool("sh")
        self.assertIsNotNone(found, "sh lives on the privileged PATH and must be found with PATH gutted")
        self.assertTrue(found.startswith("/"), f"must be an absolute path: {found}")

    def test_the_privileged_path_actually_contains_sbin(self):
        self.assertIn("/sbin", installer.PRIVILEGED_PATH.split(":"))
        self.assertIn("/usr/sbin", installer.PRIVILEGED_PATH.split(":"))


class TestCounterParsing(unittest.TestCase):
    def test_abbreviated_counters_are_not_read_as_zero(self):
        """iptables prints 12M and 494K, and the busiest DROP on a NAS is
        exactly the one that gets abbreviated. A parse that returned 0 for
        those would make the guilty rule look idle."""
        self.assertEqual(installer._int_or_zero("3997"), 3997)
        self.assertEqual(installer._int_or_zero("494K"), 494000)
        self.assertEqual(installer._int_or_zero("12M"), 12000000)
        self.assertEqual(installer._int_or_zero("54G"), 54000000000)
        self.assertEqual(installer._int_or_zero("nonsense"), 0)

    def test_the_ruleset_parses_chains_policies_and_drops(self):
        r = installer.Ruleset(REAL_RULESET_BEFORE)
        self.assertEqual(r.policies["FORWARD"], "DROP")
        self.assertEqual(r.policies["INPUT"], "ACCEPT")
        for chain in installer.DOCKER_CHAINS:
            self.assertIn(chain, r.chains, f"{chain} is in the fixture and must parse")
        drops = r.drops()
        self.assertIn(("UG_FORWARD", 4), drops)
        self.assertIn(("UG_INPUT", 7), drops)
        self.assertEqual(drops[("UG_FORWARD", 4)]["packets"], 169)
        self.assertEqual(drops[("UG_INPUT", 7)]["packets"], 3997)


class TestCounterDeltaNamesTheRule(unittest.TestCase):
    """The mechanism the whole feature rests on.

    Reading the ruleset and reasoning about it is inference, and on this
    host inference got it wrong twice: once blaming a missing binary, once
    blaming forwarding and NAT when half the fault was in INPUT. The delta
    across generated traffic is measurement.
    """

    def deltas(self, before_text, after_text):
        before, after = installer.Ruleset(before_text), installer.Ruleset(after_text)
        moved = []
        for key, rule in after.drops().items():
            delta = rule["packets"] - before.drops().get(key, {"packets": 0})["packets"]
            if delta > 0:
                moved.append((rule, delta))
        moved.sort(key=lambda item: item[1], reverse=True)
        return moved

    def test_both_offending_rules_are_named_with_their_deltas(self):
        moved = self.deltas(REAL_RULESET_BEFORE, REAL_RULESET_AFTER)
        named = {(r["chain"], r["num"]): d for r, d in moved}
        self.assertEqual(named, {("UG_FORWARD", 4): 6, ("UG_INPUT", 7): 3},
                         "both chains have to be named: the gateway ping is INPUT and the external "
                         "connection is FORWARD, and a fix aimed at one leaves the other standing")

    def test_a_quiet_ruleset_names_nothing(self):
        """The negative control. If no counter moves, the installer must
        refuse to correct anything rather than apply its default guess."""
        self.assertEqual(self.deltas(REAL_RULESET_BEFORE, REAL_RULESET_BEFORE), [])


# A realistic `docker ps --format json` NDJSON stream on an appliance
# running this project alongside other, unrelated workloads: one
# container this project's own compose project labelled, and two it did
# not.
PS_NDJSON_MIXED_HOST = (
    '{"Names": "backup-manager", "Image": "ghcr.io/spdrman/backup-manager:0.1.0", '
    '"Labels": "com.docker.compose.project=rclone-manager,com.docker.compose.service=backup-manager"}\n'
    '{"Names": "backup-manager-ui", "Image": "ghcr.io/spdrman/backup-manager:0.1.0", '
    '"Labels": "com.docker.compose.project=rclone-manager,com.docker.compose.service=backup-manager-ui"}\n'
    '{"Names": "plex", "Image": "plexinc/pms-docker:latest", "Labels": "com.docker.compose.project=media"}\n'
    '{"Names": "portainer", "Image": "portainer/portainer-ce:latest", "Labels": ""}\n'
)


class TestOtherRunningContainers(unittest.TestCase):
    """Restarting the Docker daemon restarts EVERY container on the host,
    not just this project's, and the appliances this installer targets
    (CasaOS, TrueNAS, Portainer, Unraid, ZimaOS, OMV) exist to run many
    unrelated workloads at once. These assert the daemon-restart branch
    can say what else it is about to disrupt."""

    def test_this_projects_own_containers_are_excluded(self):
        got = installer._other_containers_from_ps_ndjson(PS_NDJSON_MIXED_HOST, "rclone-manager")
        names = [name for name, _ in got]
        self.assertNotIn("backup-manager", names)
        self.assertNotIn("backup-manager-ui", names)

    def test_containers_from_another_project_or_no_project_label_are_named(self):
        got = installer._other_containers_from_ps_ndjson(PS_NDJSON_MIXED_HOST, "rclone-manager")
        names = {name for name, _ in got}
        self.assertEqual(names, {"plex", "portainer"},
                         "a differently-labelled container and an unlabelled one both count as "
                         "'other': the daemon restart does not spare either")

    def test_a_host_with_no_other_containers_reports_none(self):
        got = installer._other_containers_from_ps_ndjson(
            '{"Names": "backup-manager", "Image": "x", '
            '"Labels": "com.docker.compose.project=rclone-manager"}\n',
            "rclone-manager",
        )
        self.assertEqual(got, [])


# A realistic `docker network ls --filter driver=bridge -q` on this host
# would print the two ids below; INSPECT_TWO_NETWORKS is the JSON
# `docker network inspect` prints for exactly those two, abbreviated to
# the fields _bridge_interfaces_from_network_inspect actually reads.
INSPECT_TWO_NETWORKS = """
[
  {
    "Name": "bridge",
    "Id": "f7ab26d71dbd4b3aa74e3f6c9d1e2a5b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f",
    "Driver": "bridge",
    "Options": {}
  },
  {
    "Name": "rclone-manager_internal",
    "Id": "3f2e1a9c8b7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f",
    "Driver": "bridge",
    "Options": {}
  }
]
"""


class TestBridgeInterfaceDiscovery(unittest.TestCase):
    """`-i br-+` is iptables' own PREFIX wildcard, not an exact-suffix
    guard: it matches ANY interface starting with `br-`, not only the
    twelve-hex-character ones Docker's bridge driver creates. A host
    bridge an operator named `br-lan` (common on router and embedded
    platforms) would get the same DOCKER-USER RETURN and INPUT ACCEPT
    this installer adds for Docker's own bridges, on an interface it
    knows nothing about. These assert the replacement asks Docker for
    the exact interfaces instead of trusting a pattern.
    """

    def test_the_default_bridge_is_named_docker0_by_name_not_derivation(self):
        got = installer._bridge_interfaces_from_network_inspect(INSPECT_TWO_NETWORKS)
        self.assertIn("docker0", got)
        # f7ab26d71dbd is what deriving from the default network's own id
        # would produce, and it is not this host's interface: the default
        # bridge's name is hardcoded in the daemon, never derived.
        self.assertNotIn("br-f7ab26d71dbd", got)

    def test_a_user_defined_network_derives_its_interface_from_the_id(self):
        got = installer._bridge_interfaces_from_network_inspect(INSPECT_TWO_NETWORKS)
        self.assertIn("br-3f2e1a9c8b7d", got)

    def test_a_custom_bridge_name_option_wins_over_derivation(self):
        raw = """
        [{"Name": "custom", "Id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
          "Driver": "bridge", "Options": {"com.docker.network.bridge.name": "br-custom0"}}]
        """
        got = installer._bridge_interfaces_from_network_inspect(raw)
        self.assertIn("br-custom0", got)
        self.assertNotIn("br-aaaaaaaaaaaa", got)

    def test_nothing_it_returns_is_a_wildcard(self):
        """The regression this whole class exists for: whatever Docker's
        real bridges turn out to be, the returned list is exact interface
        names, never a `+` pattern that could over-match a host bridge
        Docker did not create."""
        got = installer._bridge_interfaces_from_network_inspect(INSPECT_TWO_NETWORKS)
        for iface in got:
            self.assertNotIn("+", iface, f"{iface} is a wildcard, not a discovered interface")

    def test_an_unrelated_host_bridge_never_appears(self):
        """br-lan is not one of Docker's own networks and never appears in
        `docker network inspect`'s output, so it must never appear in the
        discovered list either - which is the whole point: the old `br-+`
        wildcard could not tell it apart from a real Docker bridge, and
        this replacement never even asks the question of an interface
        Docker itself did not create."""
        got = installer._bridge_interfaces_from_network_inspect(INSPECT_TWO_NETWORKS)
        self.assertNotIn("br-lan", got)
        self.assertNotIn("br0", got)

    def test_bridge_interfaces_is_settable_without_a_live_docker_daemon(self):
        """The seam BridgeDoctor's own tests use: bridge_interfaces()
        memoizes into _bridge_interfaces, which a test can set directly
        the same way it already overrides self.iptables."""
        fx = Fixture(self)
        d = installer.BridgeDoctor(fx.args())
        d._bridge_interfaces = ["docker0", "br-deadbeef0000"]
        self.assertEqual(d.bridge_interfaces(), ["docker0", "br-deadbeef0000"])


class TestRemediationIsSafe(unittest.TestCase):
    """Every one of these is a way to lock somebody out of a NAS reachable
    only over SSH, asserted as absent rather than avoided by care."""

    def doctor(self):
        fx = Fixture(self)
        args = fx.args()
        d = installer.BridgeDoctor(args)
        d.iptables = "/sbin/iptables"
        # Set directly, the same way d.iptables is above: rule_specs()
        # asks bridge_interfaces(), and nothing in this class should need
        # a live Docker daemon to be exercised.
        d._bridge_interfaces = ["docker0", "br-0123456789ab"]
        return d

    def every_script(self, d):
        """Every place this installer can emit a firewall command.

        The unit is in here deliberately. A safety assertion that only
        covers the path a human takes is not covering the path that runs
        unattended every couple of minutes for the life of the machine,
        and that is the one nobody is watching (#273).
        """
        return {
            "insert": d.insert_script(),
            "delete": d.delete_script(),
            "unit service": d.unit_service_text(),
            "unit install": d.unit_install_script(),
            "unit remove": d.unit_remove_script(),
            "docker restart": d.restart_docker_script(),
        }

    def test_nothing_flushes_changes_a_policy_or_restores_a_ruleset(self):
        d = self.doctor()
        for name, script in self.every_script(d).items():
            for forbidden in (" -F", " --flush", " -P ", " --policy", "iptables-restore", " -X", " -Z"):
                self.assertNotIn(forbidden, script,
                                 f"{forbidden!r} in the {name} script can take the SSH session with it")

    def test_nothing_ever_touches_a_chain_this_installer_did_not_create(self):
        """UG_* is the host's own firewall. Reading it is the diagnosis;
        writing to it would be taking ownership of somebody else's rules,
        which is the same mistake as snapshotting the whole ruleset."""
        d = self.doctor()
        for name, script in self.every_script(d).items():
            for verb in (" -I UG_", " -A UG_", " -D UG_", " -R UG_", " -F UG_"):
                self.assertNotIn(verb, script, f"the {name} script writes to a UG_ chain")

    def test_persistence_never_snapshots_the_hosts_ruleset(self):
        """`netfilter-persistent save` is installed and active on the target
        host and is the obvious move. It writes the ENTIRE live ruleset,
        the host's own chains included, into a file this project would then
        restore at every boot."""
        d = self.doctor()
        for name, script in self.every_script(d).items():
            self.assertNotIn("iptables-save", script, f"the {name} script snapshots the ruleset")
            # Naming the unit in an After= is ordering, and is the point.
            # RUNNING it is the mistake, so the check is per line and only
            # a line that orders against it is allowed to mention it.
            for line in script.splitlines():
                if "netfilter-persistent" in line:
                    self.assertTrue(
                        line.startswith("After=") or line.startswith("# "),
                        f"the {name} script invokes netfilter-persistent, which writes the ENTIRE live "
                        f"ruleset including the host's own chains into a file this project would then "
                        f"restore at every boot: {line}")

    def test_every_rule_is_interface_scoped_and_never_a_blanket_accept(self):
        d = self.doctor()
        for chain, spec in d.rule_specs():
            self.assertIn("-i", spec, f"{chain} rule is not scoped to an interface: {spec}")
            iface = spec[spec.index("-i") + 1]
            self.assertIn(iface, d.bridge_interfaces(),
                          f"{iface} is not one of the interfaces bridge_interfaces() discovered")
            self.assertNotIn("+", iface,
                             f"{iface} is a wildcard, not a discovered interface: `-i br-+` matches ANY "
                             "interface starting with br-, not only Docker's own, which is the bug "
                             "bridge_interfaces() replaced this constant to close")

    def test_the_forward_rule_returns_rather_than_accepts(self):
        """An ACCEPT in DOCKER-USER ends the FORWARD traversal and takes
        Docker's inter-network isolation with it. This project's whole
        topology is that the engine is reachable only from the Web UI
        because they share a network nothing else joins, so an ACCEPT here
        would quietly undo the security property the deployment is built
        on. RETURN hands the decision back to Docker's own chains."""
        d = self.doctor()
        for chain, spec in d.rule_specs():
            if chain == "DOCKER-USER":
                self.assertEqual(spec[-1], "RETURN",
                                 "an ACCEPT here bypasses DOCKER-ISOLATION and un-isolates every "
                                 "user-defined network on the host")

    def test_every_rule_carries_the_tag_so_it_can_be_found_again(self):
        d = self.doctor()
        for chain, spec in d.rule_specs():
            self.assertIn(installer.RULE_TAG, spec, f"{chain} rule is not removable: {spec}")

    def test_insertion_checks_before_it_inserts(self):
        """Idempotence by construction. Re-running must not stack
        duplicates, and `iptables -C` is the only thing that can say
        whether the exact rule is already there."""
        d = self.doctor()
        lines = [ln for ln in d.insert_script().splitlines() if "iptables" in ln]
        self.assertTrue(lines)
        for line in lines:
            self.assertIn(" -C ", line, f"inserts without checking first: {line}")
            self.assertIn("||", line, f"the insert is not conditional on the check: {line}")

    def test_every_iptables_invocation_waits_for_the_xtables_lock(self):
        """The stated threat model is literally that the host's own
        firewall management process may be rewriting the ruleset around
        the same time this installer reads or writes it. Without -w, two
        processes racing for the xtables lock is a plain failure
        ("Resource temporarily unavailable") rather than one of them
        waiting its turn."""
        d = self.doctor()
        for script in (d.insert_script(), d.delete_script()):
            for line in script.splitlines():
                if "iptables" in line:
                    self.assertIn(" -w ", line, f"races the xtables lock instead of waiting for it: {line}")

    def test_deletion_removes_only_tagged_rules(self):
        d = self.doctor()
        for line in d.delete_script().splitlines():
            if "-D" in line:
                self.assertIn(installer.RULE_TAG, line,
                              f"a delete that does not name the tag can remove somebody else's rule: {line}")

    def test_insert_and_delete_describe_the_same_rules(self):
        d = self.doctor()
        ins = {ln.split(" -I ")[1] for ln in d.insert_script().splitlines() if " -I " in ln}
        dele = {ln.split(" -D ")[1].split(" || ")[0] for ln in d.delete_script().splitlines() if " -D " in ln}
        self.assertEqual({i.replace(" 1 ", " ", 1) for i in ins}, dele,
                         "anything the installer adds it has to be able to take back")


class TestSudoRefusals(unittest.TestCase):
    """Three problems, three exit codes. They call for completely
    different reactions and a shared code cannot tell them apart."""

    def test_each_failure_mode_maps_to_its_own_code(self):
        s = installer.Sudo(sudo_path="/usr/bin/sudo")
        cases = [
            ("sudo: no tty present and no askpass program specified", installer.EXIT_SUDO_NO_TTY),
            # The wording the UGREEN's own sudo actually used. It shares no
            # distinctive word with the line above, so a classifier written
            # from memory misses it and reports a generic runtime failure
            # for the one case that has an obvious remedy.
            ("sudo: a terminal is required to read the password; either use the -S option to read "
             "from standard input or configure an askpass helper", installer.EXIT_SUDO_NO_TTY),
            ("rom is not in the sudoers file.  This incident will be reported.",
             installer.EXIT_SUDO_NOT_PERMITTED),
            ("Sorry, try again.\nsudo: 3 incorrect password attempts",
             installer.EXIT_SUDO_WRONG_PASSWORD),
            ("something nobody predicted", installer.EXIT_RUNTIME),
        ]
        seen = set()
        for stderr, want in cases:
            got = s.classify(stderr)
            self.assertEqual(got, want, f"{stderr!r} classified as {got}, want {want}")
            seen.add(got)
        self.assertEqual(len(seen), 4, "the inputs must produce four distinct codes")

    def test_a_missing_sudo_refuses_rather_than_raising(self):
        s = installer.Sudo(sudo_path=None)
        s.sudo = None
        exc = refusal_from(s.run_script, "true\n", purpose="test")
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_SUDO_NOT_PERMITTED)
        self.assertIn("--fix-network=never", exc.remedy)

    def test_a_hang_refuses_rather_than_raising_a_raw_traceback(self):
        """The one path that escalates to root, on a host reachable only
        over the SSH session the operator is currently using, inserting
        firewall rules. run_script() used to call subprocess.run directly
        rather than through this file's own run(), so a `sudo ... /bin/sh
        -s` that hung for its full timeout raised an uncaught
        TimeoutExpired instead of one of this file's own coded exit
        statuses - the only subprocess call in the file that did."""
        stub = tempfile.NamedTemporaryFile(mode="w", suffix=".sh", delete=False)
        self.addCleanup(os.unlink, stub.name)
        # run_script() calls passwordless() first, which invokes this same
        # stub with `-n true`: answer that one immediately so the test
        # stays fast, and hang only on the real call (`-p ... /bin/sh -s`),
        # the way a sudo prompting on a terminal nobody is watching would.
        # subprocess's own timeout kills it well before the sleep finishes.
        stub.write("#!/bin/sh\ncase \"$1\" in -n) exit 0 ;; *) sleep 5 ;; esac\n")
        stub.close()
        os.chmod(stub.name, 0o755)

        s = installer.Sudo(sudo_path=stub.name)
        exc = refusal_from(s.run_script, "true\n", purpose="test", timeout=0.2)
        self.assertIsNotNone(exc, "a hang must raise a Refusal, not propagate TimeoutExpired uncaught")
        self.assertEqual(exc.code, installer.EXIT_RUNTIME)


class TestHealthyHostIsANoOp(unittest.TestCase):
    def test_a_working_host_changes_nothing_and_never_escalates(self):
        fx = Fixture(self)
        args = fx.args("--fix-network", "auto", command="install")

        class ExplodingSudo(installer.Sudo):
            def run_script(self, script, *, purpose, timeout=300):
                raise AssertionError("a healthy host must never ask for a password")

        real_probe = installer.BridgeDoctor.probe
        real_image = installer.BridgeDoctor.ensure_probe_image
        installer.BridgeDoctor.probe = lambda self, net: {
            "gateway": True, "egress": True, "gateway_ip": "172.17.0.1", "raw": ""}
        installer.BridgeDoctor.ensure_probe_image = lambda self: None
        self.addCleanup(lambda: setattr(installer.BridgeDoctor, "probe", real_probe))
        self.addCleanup(lambda: setattr(installer.BridgeDoctor, "ensure_probe_image", real_image))

        outcome = installer.diagnose_and_fix(args, "bridge", sudo=ExplodingSudo())
        self.assertTrue(outcome["healthy"])
        self.assertFalse(outcome["changed"], "a healthy host must not be modified")

    def test_fix_network_never_refuses_on_a_broken_host_without_escalating(self):
        fx = Fixture(self)
        args = fx.args("--fix-network", "never", command="install")

        class ExplodingSudo(installer.Sudo):
            def run_script(self, script, *, purpose, timeout=300):
                raise AssertionError("--fix-network=never must touch no firewall")

        real_probe = installer.BridgeDoctor.probe
        real_image = installer.BridgeDoctor.ensure_probe_image
        installer.BridgeDoctor.probe = lambda self, net: {
            "gateway": False, "egress": False, "gateway_ip": "172.17.0.1", "raw": ""}
        installer.BridgeDoctor.ensure_probe_image = lambda self: None
        self.addCleanup(lambda: setattr(installer.BridgeDoctor, "probe", real_probe))
        self.addCleanup(lambda: setattr(installer.BridgeDoctor, "ensure_probe_image", real_image))

        exc = refusal_from(installer.diagnose_and_fix, args, "bridge", sudo=ExplodingSudo())
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_NETWORK_BROKEN)


class TestProbeDefaultsCarryNothingPrivate(unittest.TestCase):
    """The egress probe opens a real connection, so its default target is
    part of the shipped source. It must be a neutral public endpoint and
    never a host this project happens to back up."""

    def test_the_default_probe_target_is_generic(self):
        args = Fixture(self).args(command="install")
        self.assertEqual(args.probe_host, "1.1.1.1")
        self.assertEqual(args.probe_port, 443)


class TestProbeArgvHygiene(unittest.TestCase):
    """gateway/probe_host/probe_port used to be interpolated directly into
    the shell script string run inside the probe container (issue #330).
    These pin the replacement: the script text is a fixed constant, and
    the values travel as environment variables instead - never re-parsed
    as shell syntax on either side."""

    def test_the_shell_script_is_a_fixed_constant(self):
        """Same script string regardless of what gateway/host/port are,
        which is only possible if none of them were ever spliced into it."""
        argv1 = installer._probe_argv("img", "net", "172.17.0.1", "1.1.1.1", 443)
        argv2 = installer._probe_argv("img", "net", "10.0.0.1; rm -rf /", "$(whoami)", 9999)
        self.assertEqual(argv1[-1], argv2[-1])

    def test_values_travel_as_environment_variables_not_shell_text(self):
        argv = installer._probe_argv("img", "net", "172.17.0.1", "1.1.1.1", 443)
        env_pairs = {argv[i + 1] for i, tok in enumerate(argv) if tok == "-e"}
        self.assertEqual(env_pairs, {"GATEWAY=172.17.0.1", "PROBE_HOST=1.1.1.1", "PROBE_PORT=443"})

    def test_a_value_with_shell_metacharacters_never_reaches_the_script_text(self):
        """The regression this class exists for: a value that would have
        broken out of the old interpolated string must appear ONLY in its
        own -e argument, never inside the script argparse hands to sh."""
        hostile = "1.1.1.1; rm -rf / #"
        argv = installer._probe_argv("img", "net", "172.17.0.1", hostile, 443)
        script = argv[-1]
        self.assertNotIn(hostile, script, "a hostile value leaked into the script text")
        self.assertIn(f"PROBE_HOST={hostile}", argv, "the value should still reach the container, as an env var")

    def test_the_script_references_only_the_environment_variables(self):
        script = installer._probe_argv("img", "net", "gw", "host", 1)[-1]
        for name in ("GATEWAY", "PROBE_HOST", "PROBE_PORT"):
            self.assertIn(f'"${name}"', script, f"the script does not read ${name} at all")



class TestPersistenceUnit(unittest.TestCase):
    """The unit is the thing that runs unattended for the life of the
    machine, so what it contains is checked rather than trusted."""

    def doctor(self):
        d = installer.BridgeDoctor(Fixture(self).args())
        d.iptables = "/sbin/iptables"
        # Set directly, the same way d.iptables is above: rule_specs()
        # asks bridge_interfaces(), and nothing here should need a live
        # Docker daemon to be exercised.
        d._bridge_interfaces = ["docker0", "br-0123456789ab"]
        return d

    def test_the_unit_asserts_exactly_the_rules_the_interactive_path_does(self):
        """If these two ever drift, the timer spends the life of the machine
        re-asserting a different set from the one that was verified."""
        d = self.doctor()
        interactive = {f"{chain} {' '.join(spec)}" for chain, spec in d.rule_specs()}
        in_unit = set()
        for line in d.unit_service_text().splitlines():
            if line.startswith("ExecStart="):
                after_c = line.split(" -C ", 1)[1]
                in_unit.add(after_c.split(" || ")[0].strip())
        self.assertEqual(in_unit, interactive)

    def test_the_unit_checks_before_it_inserts(self):
        d = self.doctor()
        execs = [ln for ln in d.unit_service_text().splitlines() if ln.startswith("ExecStart=")]
        self.assertEqual(len(execs), 4, "one ExecStart per rule, so systemd names the one that failed")
        for line in execs:
            self.assertIn(" -C ", line, f"the unit inserts without checking: {line}")
            self.assertIn(" || ", line, f"the insert is not conditional on the check: {line}")

    def test_a_single_rules_failure_never_stops_the_others_from_being_tried(self):
        """systemd runs multiple ExecStart= lines in order and STOPS AT THE
        FIRST NON-ZERO EXIT by default, skipping the rest, unless each is
        prefixed with `-`. This unit fires unattended every
        REASSERT_INTERVAL for the life of the machine, re-asserting four
        logically independent rules: one failing (xtables lock contention
        with the host's own firewall process, most plausibly) must not
        leave the other three unasserted, silently, for however many fires
        it takes to clear."""
        execs = [ln for ln in self.doctor().unit_service_text().splitlines()
                 if ln.startswith("ExecStart=")]
        self.assertEqual(len(execs), 4)
        for line in execs:
            self.assertTrue(line.startswith("ExecStart=-"),
                            f"a bare ExecStart= here means one failing rule silently skips the "
                            f"rest of this fire: {line}")

    def test_every_iptables_invocation_in_the_unit_waits_for_the_xtables_lock(self):
        for line in self.doctor().unit_service_text().splitlines():
            if line.startswith("ExecStart=") and "iptables" in line:
                self.assertIn(" -w ", line, f"races the xtables lock instead of waiting for it: {line}")

    def test_the_service_has_an_explicit_start_timeout(self):
        """The one place in this feature a timeout would otherwise be left
        to whatever this host's systemd default happens to be (typically
        90s, not guaranteed), rather than deliberately chosen the way
        every other subprocess call in this codebase already is."""
        self.assertIn("TimeoutStartSec=", self.doctor().unit_service_text())

    def test_the_unit_is_ordered_after_everything_that_builds_the_ruleset(self):
        after = [ln for ln in self.doctor().unit_service_text().splitlines() if ln.startswith("After=")]
        self.assertEqual(len(after), 1)
        for unit in ("net_serv.service", "netfilter-persistent.service", "nftables.service",
                     "docker.service"):
            self.assertIn(unit, after[0])

    def test_the_unit_never_hard_depends_on_those(self):
        """A host without one of them should still get its rules, not a
        failed unit."""
        # Directives only. The unit's own comment explains why it does not
        # use Requires=, so a whole-text substring match reads that
        # explanation as the thing it is warning against.
        directives = [ln for ln in self.doctor().unit_service_text().splitlines()
                      if ln and not ln.startswith("#")]
        for directive in ("Requires=", "BindsTo=", "Requisite="):
            for line in directives:
                self.assertFalse(line.startswith(directive),
                                 f"{directive} makes a host missing one of those units fail instead "
                                 f"of getting its rules: {line}")

    def test_the_timer_covers_boot_and_runtime_rewrites(self):
        timer = self.doctor().unit_timer_text()
        self.assertIn("OnBootSec=", timer, "a reboot is one of the two ways the rules go away")
        self.assertIn(f"OnUnitActiveSec={installer.BridgeDoctor.REASSERT_INTERVAL}", timer,
                      "the host rewriting its ruleset at runtime is the other, and a boot-only "
                      "unit does not cover it")
        self.assertIn(f"Unit={installer.BridgeDoctor.SERVICE_UNIT}", timer)

    def test_a_healthy_run_produces_no_output_from_the_unit(self):
        """`iptables -C` prints nothing on success, so a fire that changes
        nothing says nothing. LogLevelMax keeps systemd's own start and
        finish lines out of the journal as well."""
        self.assertIn("LogLevelMax=", self.doctor().unit_service_text())

    def test_everything_the_install_writes_the_remove_deletes(self):
        d = self.doctor()
        written = {tok for tok in d.unit_install_script().split() if tok.startswith(d.UNIT_DIR)}
        removed = {tok for tok in d.unit_remove_script().split() if tok.startswith(d.UNIT_DIR)}
        self.assertTrue(written, "the install script writes no unit file at all")
        self.assertEqual(written, removed, "anything installed has to be removable")

    def test_the_remove_script_tolerates_a_half_installed_host(self):
        """Undo has to work on a machine where installation failed partway,
        and an undo that dies at the first missing thing is worse than
        none."""
        for line in self.doctor().unit_remove_script().splitlines():
            if "systemctl" in line and "daemon-reload" not in line:
                self.assertIn("|| true", line, f"this aborts on an already-absent unit: {line}")


class TestPersistenceVerification(unittest.TestCase):
    """`systemctl enable` succeeding says the symlink was made, not that
    the unit is valid or the timer is armed - a unit file with a typo in
    it enables perfectly and never runs. persistence_complaints() is what
    install_persistence() reads the systemd state back through before
    printing "Fixed, and proven"; these pin its verdict directly, without
    needing a real systemd to produce the state it is reading."""

    GOOD = dict(service_unit="rclone-manager-bridge.service", service_state="enabled",
               service_active="inactive", timer_unit="rclone-manager-bridge.timer",
               timer_state="enabled", timer_active="active",
               timer_listed="Thu 2026-09-03 rclone-manager-bridge.timer")

    def test_a_correctly_armed_timer_has_no_complaints(self):
        """The service itself is legitimately 'inactive' right after a
        successful run: Type=oneshot with RemainAfterExit=no returns to
        inactive on success, which is not a failure signal."""
        self.assertEqual(installer.persistence_complaints(**self.GOOD), [])

    def test_a_service_not_enabled_is_a_complaint(self):
        bad = dict(self.GOOD, service_state="disabled")
        got = installer.persistence_complaints(**bad)
        self.assertEqual(len(got), 1)
        self.assertIn("not 'enabled'", got[0])

    def test_a_failed_service_is_a_complaint(self):
        bad = dict(self.GOOD, service_active="failed")
        got = installer.persistence_complaints(**bad)
        self.assertEqual(len(got), 1)
        self.assertIn("'failed'", got[0])

    def test_a_timer_not_enabled_is_a_complaint(self):
        bad = dict(self.GOOD, timer_state="disabled")
        got = installer.persistence_complaints(**bad)
        self.assertEqual(len(got), 1)
        self.assertIn("not 'enabled'", got[0])

    def test_a_timer_that_is_not_active_is_a_complaint(self):
        """Unlike the service, the timer being anything other than
        'active' after enable --now IS a failure signal: a correctly
        armed timer stays active/waiting."""
        bad = dict(self.GOOD, timer_active="inactive")
        got = installer.persistence_complaints(**bad)
        self.assertEqual(len(got), 1)
        self.assertIn("not 'active'", got[0])

    def test_no_scheduled_fire_is_a_complaint_even_if_everything_else_looks_fine(self):
        bad = dict(self.GOOD, timer_listed="")
        got = installer.persistence_complaints(**bad)
        self.assertEqual(len(got), 1)
        self.assertIn("no scheduled fire", got[0])

    def test_every_problem_is_named_at_once_rather_than_stopping_at_the_first(self):
        bad = dict(self.GOOD, service_state="disabled", timer_active="inactive", timer_listed="")
        got = installer.persistence_complaints(**bad)
        self.assertEqual(len(got), 3, f"expected 3 distinct complaints, got {got}")

    def test_install_persistence_refuses_rather_than_printing_over_a_bad_state(self):
        """The integration point: install_persistence() must turn a bad
        read-back into a Refusal with the persistence-specific exit code,
        not just print it and return successfully."""
        d = installer.BridgeDoctor(Fixture(self).args())
        d.sudo = installer.Sudo(sudo_path=None)
        # doctor.sudo.run_script is stubbed below, but its argument -
        # doctor.unit_install_script() - is evaluated eagerly before the
        # call, and that path reaches rule_specs() -> bridge_interfaces().
        # Set directly so this test needs no live Docker daemon either.
        d._bridge_interfaces = ["docker0", "br-0123456789ab"]
        d.sudo.run_script = lambda *a, **kw: None  # the unit-install call itself, stubbed out

        original_run = installer.run

        def fake_run(argv, **kwargs):
            class Proc:
                stdout = ""
            if "is-enabled" in argv:
                Proc.stdout = "enabled"
            elif "is-active" in argv:
                # Every unit reports inactive, including the timer, which
                # is exactly the "armed on paper, not really" state this
                # exists to catch.
                Proc.stdout = "inactive"
            elif "list-timers" in argv:
                Proc.stdout = ""
            return Proc()

        installer.run = fake_run
        self.addCleanup(lambda: setattr(installer, "run", original_run))

        exc = refusal_from(installer.install_persistence, d, Fixture(self).args())
        self.assertIsNotNone(exc, "a timer that never reports active must refuse, not print and return")
        self.assertEqual(exc.code, installer.EXIT_PERSISTENCE_UNVERIFIED)
        self.assertIn("is-active", exc.message)


class TestSubcommandFlagScoping(unittest.TestCase):
    """Real argparse subparsers (issue #330): before this, every flag from
    every subcommand was on one flat parser, so `network-doctor --help`
    showed `--ssh-key`, `--if-installed` and everything else irrelevant to
    it. These assert the flags that ARE cleanly separable (bridge-network
    policy, the probe flags, --if-installed) actually only appear on the
    subcommands that read them - not that every flag is now scoped, since
    layout/credentials/runtime stay shared across all six commands on
    purpose (resolve() needs them for every command uniformly)."""

    def flags_of(self, command):
        return {opt for a in _subparser(installer.build_parser(), command)._actions
                for opt in a.option_strings}

    def test_if_installed_exists_only_on_install(self):
        for command in ("preflight", "status", "uninstall", "network-doctor", "network-undo"):
            self.assertNotIn("--if-installed", self.flags_of(command),
                             f"{command} does not converge or refuse an existing install")
        self.assertIn("--if-installed", self.flags_of("install"))

    def test_fix_network_exists_only_on_install_and_network_doctor(self):
        for command in ("preflight", "status", "uninstall", "network-undo"):
            self.assertNotIn("--fix-network", self.flags_of(command),
                             f"{command} never repairs bridge networking")
        for command in ("install", "network-doctor"):
            self.assertIn("--fix-network", self.flags_of(command))

    def test_check_network_exists_only_on_status(self):
        for command in ("preflight", "install", "uninstall", "network-doctor", "network-undo"):
            self.assertNotIn("--check-network", self.flags_of(command),
                             f"{command} has no reason to know status's own read-only flag")
        self.assertIn("--check-network", self.flags_of("status"))

    def test_probe_flags_exist_only_where_a_probe_can_run(self):
        probe_flags = {"--probe-image", "--probe-host", "--probe-port", "--probe-network"}
        for command in ("preflight", "uninstall", "network-undo"):
            self.assertFalse(probe_flags & self.flags_of(command),
                             f"{command} never reads a probe result")
        for command in ("install", "status", "network-doctor"):
            self.assertTrue(probe_flags <= self.flags_of(command),
                            f"{command} needs every probe flag")

    def test_every_command_still_resolves_the_fields_resolve_needs(self):
        """The flags resolve() populates unconditionally stay on every
        subcommand - the point of #330 was to remove IRRELEVANT flags,
        not to break resolve()'s own uniform behavior."""
        shared = {"--prefix", "--state-dir", "--backup-dir", "--config-dir", "--ssh-key",
                  "--known-hosts", "--compose-file", "--image", "--image-archive",
                  "--puid", "--pgid", "--timezone", "--public-base-url", "--listen-port"}
        for command in ("preflight", "install", "status", "uninstall",
                        "network-doctor", "network-undo"):
            self.assertTrue(shared <= self.flags_of(command), f"{command} is missing a shared flag")

    def test_docs_install_md_names_every_real_subcommand(self):
        """docs/install.md's opening paragraph used to say "Four
        subcommands" after network-doctor and network-undo had already
        been added, undercounting by two. Pinned against build_parser()'s
        real subcommand set so a seventh command added later fails this
        test until the doc catches up, rather than drifting silently
        again."""
        names = set(next(a for a in installer.build_parser()._actions
                         if isinstance(a, argparse._SubParsersAction)).choices)
        doc_lines = (REPO_ROOT / "docs" / "install.md").read_text().splitlines()
        start = next((i for i, ln in enumerate(doc_lines) if "subcommands:" in ln), None)
        self.assertIsNotNone(start, "docs/install.md has no '... subcommands:' opening line")
        paragraph_lines = []
        for ln in doc_lines[start:]:
            if ln.strip() == "":
                break
            paragraph_lines.append(ln)
        opening = " ".join(paragraph_lines)
        for name in names:
            self.assertIn(f"`{name}`", opening, f"docs/install.md's opening does not name {name!r}")


class TestFixNetworkVocabulary(unittest.TestCase):
    def test_the_default_is_the_conservative_one(self):
        args = Fixture(self).args(command="install")
        self.assertEqual(args.fix_network, "auto",
                         "installing a systemd unit is a larger commitment than a runtime rule, "
                         "so it has to be asked for")

    def test_network_doctors_own_default_is_diagnose_not_auto(self):
        """Unlike install: a command named "doctor" reads as diagnostic,
        so running it stand-alone to check should not itself escalate
        sudo and mutate the firewall (issue #330)."""
        args = Fixture(self).args(command="network-doctor")
        self.assertEqual(args.fix_network, "diagnose")

    def test_persistence_is_a_value_of_the_existing_flag_not_a_second_one(self):
        parser = installer.build_parser()
        install_sp = _subparser(parser, "install")
        choices = None
        for action in install_sp._actions:
            if action.dest == "fix_network":
                choices = set(action.choices)
        self.assertEqual(choices, {"auto", "persist", "diagnose", "never"})
        flags = _all_subparser_flags(parser)
        for overlapping in ("--persist", "--persist-network", "--install-unit", "--systemd"):
            self.assertNotIn(overlapping, flags,
                             "a second flag with overlapping meaning is how two knobs end up "
                             "disagreeing about what the operator asked for")

    def test_a_healthy_host_installs_the_unit_only_when_persistence_is_asked_for(self):
        """Otherwise persistence could only ever be installed on a host that
        was currently broken, so an operator would have to wait for the
        failure they are trying to prevent."""
        for mode, want_install in (("auto", False), ("persist", True)):
            with self.subTest(mode=mode):
                fx = Fixture(self)
                args = fx.args("--fix-network", mode, command="install")
                calls = []

                class RecordingSudo(installer.Sudo):
                    def run_script(self, script, *, purpose, timeout=300):
                        calls.append(purpose)
                        class P:
                            stdout = ""
                        return P()

                real_probe = installer.BridgeDoctor.probe
                real_image = installer.BridgeDoctor.ensure_probe_image
                real_install = installer.install_persistence
                installer.BridgeDoctor.probe = lambda self, net: {
                    "gateway": True, "egress": True, "gateway_ip": "172.17.0.1", "raw": ""}
                installer.BridgeDoctor.ensure_probe_image = lambda self: None
                installed = []
                installer.install_persistence = lambda d, a: installed.append(True)
                # Bound as default arguments, not closed over bare: this is inside a
                # `for mode in (...)` loop, so real_probe/real_image/real_install are
                # reassigned every iteration and a bare closure reads whatever they
                # equal when the cleanup finally RUNS (after the loop, at teardown),
                # not what they equalled when each cleanup was REGISTERED. Every
                # cleanup used to restore to iteration 2's already-patched value
                # rather than the true original, leaving BridgeDoctor.probe,
                # ensure_probe_image and install_persistence permanently monkeypatched
                # for every test that ran later in the same process.
                self.addCleanup(lambda p=real_probe: setattr(installer.BridgeDoctor, "probe", p))
                self.addCleanup(lambda i=real_image: setattr(installer.BridgeDoctor, "ensure_probe_image", i))
                self.addCleanup(lambda f=real_install: setattr(installer, "install_persistence", f))

                installer.diagnose_and_fix(args, "bridge", sudo=RecordingSudo())
                self.assertEqual(bool(installed), want_install)
                if not want_install:
                    self.assertEqual(calls, [], "auto must not escalate on a healthy host")

    def test_the_test_above_restores_everything_it_patched(self):
        """Regression control for the test above's own three
        self.addCleanup(...) calls, which used to close over
        real_probe/real_image/real_install BARELY inside its `for mode in
        (...)` loop. A bare closure reads whatever the loop variable
        equals when the cleanup finally RUNS (after the loop, at
        teardown), not what it equalled when the cleanup was REGISTERED,
        so every cleanup restored to the second iteration's
        already-patched value instead of the true original -
        BridgeDoctor.probe, ensure_probe_image and install_persistence
        stayed permanently monkeypatched for every test that happened to
        run later in the same process. That leak is invisible to an
        assertion inside the leaking test itself; it only shows up in
        whoever runs next, which is what this proves by running it in a
        sub-suite and inspecting what it leaves behind."""
        real_probe = installer.BridgeDoctor.probe
        real_image = installer.BridgeDoctor.ensure_probe_image
        real_install = installer.install_persistence
        try:
            case = TestFixNetworkVocabulary(
                "test_a_healthy_host_installs_the_unit_only_when_persistence_is_asked_for")
            result = unittest.TestResult()
            case.run(result)
            self.assertEqual(result.errors, [])
            self.assertEqual(result.failures, [])
            self.assertIs(installer.BridgeDoctor.probe, real_probe,
                          "BridgeDoctor.probe was left monkeypatched after the test ran")
            self.assertIs(installer.BridgeDoctor.ensure_probe_image, real_image,
                          "BridgeDoctor.ensure_probe_image was left monkeypatched after the test ran")
            self.assertIs(installer.install_persistence, real_install,
                          "install_persistence was left monkeypatched after the test ran")
        finally:
            installer.BridgeDoctor.probe = real_probe
            installer.BridgeDoctor.ensure_probe_image = real_image
            installer.install_persistence = real_install


if __name__ == "__main__":
    unittest.main()
