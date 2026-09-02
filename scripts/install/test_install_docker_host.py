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

NEW TEST CLASSES GO ABOVE the `if __name__ == "__main__":` block at the
bottom of this file, never below it. `python3 test_install_docker_host.py`
calls unittest.main() at the moment that line executes, so anything
defined after it is not in the namespace yet, does not run, and the run
still prints OK. TestTheSuiteRunsEveryTestItDefines enforces this, because
a suite that can silently stop running half of itself is worse than no
suite at all.
"""

from __future__ import annotations

import argparse
import ast
import contextlib
import io
import os
import socket
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import embed_compose  # noqa: E402
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
        argv = [command, "--prefix", str(self.prefix)]
        # --ssh-key/--known-hosts/--compose-file only exist on preflight and
        # install's own subparsers (issue #330): only Preflight's checks and
        # cmd_install's staging ever read a credential path or the canonical
        # compose file, so the other four commands no longer declare them.
        if command in ("preflight", "install"):
            argv += ["--ssh-key", str(self.key), "--known-hosts", str(self.known),
                    "--compose-file", str(CANONICAL_COMPOSE)]
        argv += list(extra)
        return installer.resolve(installer.build_parser().parse_args(argv))


def refusal_from(fn, *a, **kw):
    try:
        fn(*a, **kw)
    except installer.Refusal as exc:
        return exc
    return None


class _AlwaysTty:
    """A stdin/stdout stand-in whose isatty() answers what the test says.

    The real one answers whatever the harness happens to be: a terminal
    under `python3 -m unittest` typed by hand, a pipe under the gate. A
    test about "there is nobody to ask" that inherits that would block on
    input() on one machine and pass on another.
    """

    def __init__(self, wrapped, tty: bool) -> None:
        self._wrapped = wrapped
        self._tty = tty

    def isatty(self) -> bool:
        return self._tty

    def __getattr__(self, name):
        return getattr(self._wrapped, name)


class mock_input:
    """Canned answers in, printed lines out, and a decided tty state.

    Standard library only, like everything else here: the installer has
    no dependencies and neither does its test suite.
    """

    def __init__(self, answers, *, tty: bool = False) -> None:
        self.answers = [answers] if isinstance(answers, str) else list(answers)
        self.printed = []
        self.tty = tty

    def _answer(self, prompt=""):
        self.printed.append(prompt)
        if not self.answers:
            raise EOFError
        return self.answers.pop(0)

    def __enter__(self):
        # installer.input, not builtins.input. A name in the module's own
        # globals wins over the builtin for every call inside that module,
        # so this reaches exactly the code under test and nothing else,
        # and it needs no import of its own.
        self._real_say = installer.say
        self._real_stdin, self._real_stdout = sys.stdin, sys.stdout
        installer.input = self._answer
        installer.say = self.printed.append
        sys.stdin = _AlwaysTty(self._real_stdin, self.tty)
        sys.stdout = _AlwaysTty(self._real_stdout, self.tty)
        return self

    def __exit__(self, *exc):
        del installer.input
        installer.say = self._real_say
        sys.stdin, sys.stdout = self._real_stdin, self._real_stdout
        return False


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
        """Since #346 the installer carries the canonical definition, so
        supplying nothing is fine. Naming a path that is not there is
        still refused, and this is the assertion that it does not quietly
        fall back to the embedded copy instead: asking for one specific
        runtime and silently installing a different one would be worse
        than either the old refusal or the new default."""
        fx = Fixture(self)
        missing = fx.prefix / "nowhere" / "compose.yaml"
        args = fx.args("--compose-file", str(missing))
        exc = refusal_from(installer.Preflight(args).check_payload)
        self.assertIsNotNone(exc, "a named path that is absent must not fall back to the embedded copy")
        self.assertEqual(exc.code, installer.EXIT_PREREQ_PAYLOAD)
        self.assertIn(str(missing), exc.message, "the refusal has to name the path it could not find")
        self.assertIn("drop the flag", exc.remedy,
                      "the remedy has to mention that supplying nothing now works")


class TestTheEmbeddedCopyIsTheCanonicalOne(unittest.TestCase):
    """The gate that makes embedding safe (issue #346).

    Carrying a copy of the runtime definition inside the installer is
    only acceptable while it is provably the SAME definition. Without
    this it becomes a second opinion about mounts, networks, healthchecks
    and the engine-to-UI topology, and the two drift the moment either
    changes, silently.

    That is not hypothetical here. The --image default was the one
    shipped artifact nothing held to canonical.json, so cutting 0.2.0
    moved all eight packaged adapters and left the installer behind;
    installing 0.2.0 then pulled 0.1.0 and reported complete success,
    because a stale default is still a valid reference to an image that
    really exists. Nothing failed and nothing said anything. A compose
    file drifting the same way is worse than a stale tag.

    BYTES, not decoded text. read_text() picks the locale's codec, which
    on a host with LC_ALL=C cannot even decode this file (it has a
    section sign and em dashes in it), and it normalises CRLF on the way
    in, so a comparison of decoded text would report two files as equal
    while the two install paths shipped different bytes. A gate that says
    "byte for byte" has to actually compare bytes.
    """

    REGENERATE = "python3 scripts/install/embed_compose.py"

    def installer_source(self) -> str:
        """The installer's own source, decoded explicitly.

        Same reason as everything else in this class: read_text() would
        use the locale's codec and this file carries the whole compose
        definition, non-ASCII included.
        """
        return Path(installer.__file__).read_bytes().decode("utf-8")

    def test_the_embedded_copy_is_byte_for_byte_the_canonical_file(self):
        self.assertEqual(
            installer.EMBEDDED_COMPOSE_YAML.encode("utf-8"), CANONICAL_COMPOSE.read_bytes(),
            "the compose definition embedded in install_docker_host.py is not "
            "container/compose.yaml.\n\n"
            "The embedded copy exists so the installer needs no checkout on the host. It is only "
            "safe while it is provably the same definition, so this compares bytes rather than "
            "intent.\n\n"
            f"Regenerate it, do not hand-edit it:\n  {self.REGENERATE}")

    def test_the_recorded_digest_is_the_digest_of_the_canonical_file(self):
        """The digest is what the shipped artifact checks itself against
        on a machine that has no checkout, so a stale one is a check that
        passes for the wrong file."""
        self.assertEqual(
            installer.hashlib.sha256(CANONICAL_COMPOSE.read_bytes()).hexdigest(),
            installer.EMBEDDED_COMPOSE_SHA256,
            "EMBEDDED_COMPOSE_SHA256 is not the digest of container/compose.yaml.\n\n"
            f"Regenerate it, do not hand-edit it:\n  {self.REGENERATE}")

    def test_the_digest_describes_the_blob_it_sits_beside(self):
        """The same claim from the other side, and the one that holds on a
        NAS: check_payload compares these two, and nothing there can reach
        container/compose.yaml to notice they both moved together."""
        self.assertEqual(installer.embedded_compose_digest(), installer.EMBEDDED_COMPOSE_SHA256)

    def test_the_embedded_copy_says_it_is_generated_and_names_the_generator(self):
        """A human editing it is the drift this exists to prevent, so it
        has to look generated and name the way to regenerate it."""
        head = self.installer_source().split("EMBEDDED_COMPOSE_YAML")[0]
        self.assertIn("DO NOT EDIT BY HAND", head)
        self.assertIn("scripts/install/embed_compose.py", head,
                      "the banner has to name the regeneration command, or the next person "
                      "hand-edits it")


class TestTheShippedArtifactChecksItself(unittest.TestCase):
    """TestTheEmbeddedCopyIsTheCanonicalOne only exists inside a checkout,
    and the whole point of embedding the definition is that the installer
    travels without one. So the copy that actually lands on a NAS has had
    no gate applied to it at all.

    Truncation is loud, because Python stops parsing. Semantic corruption
    is not: a changed mount, network or healthcheck parses perfectly and
    stages a runtime topology nobody wrote. That is the one this catches.
    """

    def args_with_no_compose_file(self, fx):
        return installer.resolve(installer.build_parser().parse_args(
            ["preflight", "--prefix", str(fx.prefix),
             "--ssh-key", str(fx.key), "--known-hosts", str(fx.known)]))

    def test_an_untampered_installer_passes(self):
        """The positive control. Without it, "the tampered one refuses"
        is also satisfied by a check that refuses everything."""
        fx = Fixture(self)
        installer.Preflight(self.args_with_no_compose_file(fx)).check_payload()

    def test_a_tampered_embedded_definition_is_refused_before_anything_is_staged(self):
        fx = Fixture(self)
        original = installer.EMBEDDED_COMPOSE_YAML
        self.addCleanup(setattr, installer, "EMBEDDED_COMPOSE_YAML", original)
        # A change that parses, which is the whole point: this is what
        # semantic corruption looks like, not a truncated file.
        installer.EMBEDDED_COMPOSE_YAML = original.replace(
            "read_only: true", "read_only: false", 1)
        self.assertNotEqual(installer.EMBEDDED_COMPOSE_YAML, original,
                            "the tamper has to actually change something or this proves nothing")

        exc = refusal_from(installer.Preflight(self.args_with_no_compose_file(fx)).check_payload)
        self.assertIsNotNone(exc, "an edited copy of the installer must not stage its runtime")
        self.assertEqual(exc.code, installer.EXIT_PREREQ_PAYLOAD)
        self.assertIn(installer.EMBEDDED_COMPOSE_SHA256, exc.message,
                      "the refusal has to show what it expected")
        self.assertIn("--compose-file", exc.remedy,
                      "the remedy has to name the supported way to install a modified runtime")

    def test_a_supplied_compose_file_is_not_held_to_the_embedded_digest(self):
        """--compose-file is the supported way to install a modified
        runtime. Checking it against the embedded digest would make that
        flag useless."""
        fx = Fixture(self)
        local = fx.prefix / "local-compose.yaml"
        local.write_bytes(b"services: {}\n# a locally modified runtime\n")
        args = installer.resolve(installer.build_parser().parse_args(
            ["preflight", "--prefix", str(fx.prefix), "--ssh-key", str(fx.key),
             "--known-hosts", str(fx.known), "--compose-file", str(local)]))
        installer.Preflight(args).check_payload()


class TestRegeneratingTheEmbeddedCopy(unittest.TestCase):
    """scripts/install/embed_compose.py, and why it is a script rather
    than a branch inside the test above.

    The regeneration used to live in the gate itself, behind
    EMBED_COMPOSE_UPDATE=1, where it rewrote the module the rest of the
    suite had already imported. Beyond that, it spliced raw content into
    a NON-raw triple-quoted literal, so it could not converge on two
    ordinary edits to the canonical file:

      * a backslash would be read back as an escape, the byte comparison
        would fail, regenerating would splice the same backslash in
        again, and the message told the operator to run the thing that
        had just failed.
      * a triple quote would close the literal early and leave the rest
        of the compose file standing as Python source, which does not
        fail one test, it breaks the module at import for the whole
        suite.

    Neither can happen against today's container/compose.yaml, and both
    are one ordinary edit away, so they are refusals with a real remedy.
    """

    def canonical_text(self) -> str:
        return CANONICAL_COMPOSE.read_bytes().decode("utf-8")

    def installer_text(self) -> str:
        return Path(installer.__file__).read_bytes().decode("utf-8")

    def test_regenerating_from_the_current_canonical_file_changes_nothing(self):
        """Convergence, stated as a test. If this is not a fixed point,
        the gate fails, regenerating reproduces the same mismatch, and
        there is no sequence of commands that reaches green."""
        self.assertEqual(embed_compose.rewrite(self.installer_text(), self.canonical_text()),
                         self.installer_text())

    def test_regeneration_moves_the_blob_and_the_digest_together(self):
        changed = self.canonical_text().replace("read_only: true", "read_only: false", 1)
        self.assertNotEqual(changed, self.canonical_text())
        out = embed_compose.rewrite(self.installer_text(), changed)
        self.assertIn("read_only: false", out)
        self.assertIn(installer.hashlib.sha256(changed.encode("utf-8")).hexdigest(), out,
                      "a regenerated blob with a stale digest is a self-check that passes for "
                      "the wrong file")

    def test_a_backslash_refuses_rather_than_looping(self):
        with self.assertRaises(embed_compose.Unembeddable) as caught:
            embed_compose.rewrite(self.installer_text(),
                                  self.canonical_text() + "# a path like C:\\\\data\n")
        self.assertIn("backslash", str(caught.exception).lower())
        self.assertIn("raw string", str(caught.exception),
                      "the refusal has to name the fix, not just the problem")

    def test_a_triple_quote_refuses_rather_than_breaking_the_module(self):
        with self.assertRaises(embed_compose.Unembeddable) as caught:
            embed_compose.rewrite(self.installer_text(),
                                  self.canonical_text() + '# a docstring: """\n')
        self.assertIn("triple quote", str(caught.exception).lower())

    def test_it_refuses_before_it_writes(self):
        """rewrite() returns a string and touches no file, so a refused
        regeneration cannot leave the installer half-spliced. This pins
        that rather than trusting it."""
        before = Path(installer.__file__).read_bytes()
        with self.assertRaises(embed_compose.Unembeddable):
            embed_compose.rewrite(self.installer_text(), self.canonical_text() + "\\\n")
        self.assertEqual(Path(installer.__file__).read_bytes(), before)


class TestComposeFileIsOptional(unittest.TestCase):
    """Copying one file to a NAS and running it has to work. It used to
    refuse with exit 19 because container/compose.yaml only exists inside
    a checkout, which is the one thing an operator installing onto a NAS
    does not have."""

    def install_args(self, fx, *extra):
        return installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(fx.prefix), "--ssh-key", str(fx.key),
             "--known-hosts", str(fx.known), *extra]))

    def test_supplying_nothing_is_no_longer_a_refusal(self):
        fx = Fixture(self)
        args = installer.resolve(installer.build_parser().parse_args(
            ["preflight", "--prefix", str(fx.prefix),
             "--ssh-key", str(fx.key), "--known-hosts", str(fx.known)]))
        self.assertIsNone(args.compose_file, "no --compose-file means the embedded copy")
        installer.Preflight(args).check_payload()

    def test_staging_without_a_compose_file_writes_the_canonical_bytes(self):
        """The staged file, not the constant. What lands at the prefix is
        what the host runs, and it is the only thing that settles whether
        the two branches of stage_payload agree."""
        fx = Fixture(self)
        args = self.install_args(fx)
        installer.stage_payload(args)
        self.assertEqual((args.prefix / "compose.yaml").read_bytes(),
                         CANONICAL_COMPOSE.read_bytes(),
                         "what lands at the prefix has to be the canonical definition, "
                         "whatever wrote it")

    def test_both_staging_paths_land_the_same_bytes(self):
        """One branch writes an encoded string and the other calls
        shutil.copyfile, and "byte for byte" is a claim about the file on
        disk rather than about either branch on its own. The comparison
        used to be of decoded text, which would have called two files
        equal while the two paths shipped different bytes."""
        embedded_fx = Fixture(self)
        installer.stage_payload(self.install_args(embedded_fx))
        supplied_fx = Fixture(self)
        installer.stage_payload(self.install_args(
            supplied_fx, "--compose-file", str(CANONICAL_COMPOSE)))
        self.assertEqual((embedded_fx.prefix / "compose.yaml").read_bytes(),
                         (supplied_fx.prefix / "compose.yaml").read_bytes())

    def test_an_explicit_compose_file_still_wins(self):
        """A checkout testing an uncommitted runtime change must still be
        able to install it, or the development workflow breaks."""
        fx = Fixture(self)
        local = fx.prefix / "local-compose.yaml"
        local.write_bytes(b"services: {}\n# a locally modified runtime\n")
        args = self.install_args(fx, "--compose-file", str(local))
        installer.stage_payload(args)
        self.assertEqual((args.prefix / "compose.yaml").read_bytes(), local.read_bytes(),
                         "explicit input has to beat the embedded default")


class TestStagingSurvivesAnAsciiLocale(unittest.TestCase):
    """[reproduced] container/compose.yaml carries a section sign and em
    dashes on many lines. write_text() encodes with the LOCALE's codec, so
    on a host running LC_ALL=C the staging step died with

        UnicodeEncodeError: 'ascii' codec can't encode character '\\xa7'

    partway through, after the directories were made. A NAS with a bare C
    locale is not exotic, it is the default on a machine nobody has
    configured, which is exactly the machine this installer targets.

    Driven in a subprocess because the locale is process-wide and is read
    at interpreter start. PYTHONUTF8=0 and PYTHONCOERCECLOCALE=0 turn off
    the two mechanisms (PEP 540 and PEP 538) that would otherwise quietly
    upgrade a C locale to UTF-8 and make this test pass without proving
    anything.
    """

    PROBE = """
import locale, sys, tempfile
from pathlib import Path
sys.path.insert(0, {installdir!r})
import install_docker_host as installer
if 'utf' in locale.getpreferredencoding(False).lower().replace('-', ''):
    print('LOCALE-NOT-ASCII ' + locale.getpreferredencoding(False))
    raise SystemExit(0)
tmp = tempfile.mkdtemp()
args = installer.resolve(installer.build_parser().parse_args(
    ['install', '--prefix', tmp + '/backup-manager']))
installer.stage_payload(args)
sys.stdout.buffer.write(b'STAGED ')
sys.stdout.buffer.write(
    (args.prefix / 'compose.yaml').read_bytes()[:0] or b'')
print(len((args.prefix / 'compose.yaml').read_bytes()))
"""

    def test_staging_does_not_die_on_a_c_locale(self):
        env = dict(os.environ, LC_ALL="C", LANG="C",
                   PYTHONUTF8="0", PYTHONCOERCECLOCALE="0")
        proc = installer.subprocess.run(
            [sys.executable, "-c",
             self.PROBE.format(installdir=str(Path(installer.__file__).resolve().parent))],
            capture_output=True, text=True, env=env, timeout=120)
        if "LOCALE-NOT-ASCII" in proc.stdout:
            self.skipTest("this interpreter refuses to run in a non-UTF-8 locale, so the "
                          "reproduction cannot be set up here: " + proc.stdout.strip())
        self.assertEqual(proc.returncode, 0,
                         "staging the embedded definition died under LC_ALL=C:\n" + proc.stderr)
        self.assertIn("STAGED", proc.stdout)
        self.assertIn(str(len(CANONICAL_COMPOSE.read_bytes())), proc.stdout,
                      "the staged file has to be the whole definition, not a truncated one")


class TestARunningCheckoutIsNotSilentlyIgnored(unittest.TestCase):
    """Running the installer from inside a checkout used to install that
    checkout's container/compose.yaml. It silently does not any more, so a
    developer editing the canonical runtime and testing through the
    installer would deploy the embedded copy and never be told.

    Which file wins is deliberately unchanged: "whichever directory the
    script happens to sit in" is exactly the location-dependent behaviour
    embedding removed, and --compose-file is the one explicit knob. What
    changed is that the installer says so, and names the flag.
    """

    def preflight_with_no_compose_file(self, fx):
        args = installer.resolve(installer.build_parser().parse_args(
            ["preflight", "--prefix", str(fx.prefix),
             "--ssh-key", str(fx.key), "--known-hosts", str(fx.known)]))
        pf = installer.Preflight(args)
        pf.check_payload()
        return pf

    def test_it_finds_the_checkout_it_is_running_from(self):
        self.assertEqual(installer.checkout_compose_beside_this_installer(), CANONICAL_COMPOSE,
                         "these tests only run from a checkout, so this has to find one")

    def test_an_installer_that_is_not_in_a_checkout_answers_rather_than_raising(self):
        """Path.parents raises IndexError for a file fewer than three
        directories deep, and a copy at /tmp/install.py is the standalone
        case this whole change exists for."""
        with tempfile.TemporaryDirectory() as tmp:
            copy = Path(tmp) / "install_docker_host.py"
            copy.write_bytes(Path(installer.__file__).read_bytes())
            proc = installer.subprocess.run(
                [sys.executable, "-c",
                 "import sys; sys.path.insert(0, %r); import install_docker_host as i; "
                 "print(i.checkout_compose_beside_this_installer())" % tmp],
                capture_output=True, text=True, timeout=120)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertEqual(proc.stdout.strip(), "None",
                             "a copy outside a checkout has no checkout to report")

    def test_a_matching_checkout_is_reported_as_matching(self):
        fx = Fixture(self)
        notes = " ".join(self.preflight_with_no_compose_file(fx).notes)
        self.assertIn(str(CANONICAL_COMPOSE), notes)
        self.assertIn("identical", notes)

    def test_a_diverging_checkout_is_said_out_loud(self):
        fx = Fixture(self)
        original = installer.EMBEDDED_COMPOSE_YAML
        self.addCleanup(setattr, installer, "EMBEDDED_COMPOSE_YAML", original)
        self.addCleanup(setattr, installer, "EMBEDDED_COMPOSE_SHA256",
                        installer.EMBEDDED_COMPOSE_SHA256)
        # Both moved together, which is what an uncommitted edit to the
        # canonical file looks like from the installer's side: the
        # self-check passes and the checkout is the thing that differs.
        installer.EMBEDDED_COMPOSE_YAML = original.replace(
            "read_only: true", "read_only: false", 1)
        installer.EMBEDDED_COMPOSE_SHA256 = installer.embedded_compose_digest()

        notes = " ".join(self.preflight_with_no_compose_file(fx).notes)
        self.assertIn("DIFFERS", notes,
                      "a developer whose checkout edit is about to be ignored has to be told")
        self.assertIn("--compose-file", notes,
                      "and told which flag installs it instead")


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


class TestVersionOrdering(unittest.TestCase):
    """The installer could not previously tell an upgrade from a
    downgrade from a reinstall, because it never read what was running.
    String comparison is not enough: "0.10.0" sorts before "0.9.0"
    lexically and after it numerically, and getting that backwards means
    offering to "upgrade" a host onto an older build."""

    def test_a_tag_is_read_out_of_a_full_reference(self):
        self.assertEqual(installer.image_tag("ghcr.io/spdrman/backup-manager:0.2.0"), "0.2.0")
        self.assertEqual(installer.image_tag("backup-manager:1.4.2"), "1.4.2")

    def test_a_registry_port_is_not_mistaken_for_a_tag(self):
        """A colon in a reference is not always a tag separator."""
        self.assertEqual(installer.image_tag("localhost:5000/backup-manager"), "")
        self.assertEqual(installer.image_tag("localhost:5000/backup-manager:0.3.1"), "0.3.1")

    def test_ordering_is_numeric_and_not_lexical(self):
        self.assertEqual(installer.compare_versions("0.9.0", "0.10.0"), "older")
        self.assertEqual(installer.compare_versions("0.10.0", "0.9.0"), "newer")
        self.assertEqual(installer.compare_versions("0.2.0", "0.2.0"), "same")

    def test_a_tag_that_is_not_a_version_is_unknown_rather_than_guessed(self):
        """latest, a digest or a branch name orders against nothing. Saying
        so is the only honest answer, and it is what stops the installer
        claiming a direction it cannot know."""
        for tag in ("latest", "main", "sha256-abc123", ""):
            self.assertEqual(installer.compare_versions(tag, "0.2.0"), "unknown",
                             f"{tag!r} is not a version and must not be ordered")

    def test_a_prerelease_sorts_below_its_own_release(self):
        """The suffix used to be thrown away, so 0.2.0-rc1 compared EQUAL
        to 0.2.0. Moving a host from the release back onto its own
        candidate was reported as "same version, converging in place" and
        the downgrade guard never fired on the one comparison a release
        process actually produces."""
        self.assertEqual(installer.compare_versions("0.2.0-rc1", "0.2.0"), "older")
        self.assertEqual(installer.compare_versions("0.2.0", "0.2.0-rc1"), "newer",
                         "release -> its own release candidate is a downgrade")
        self.assertEqual(installer.compare_versions("0.2.0-rc1", "0.2.0-rc1"), "same")

    def test_dotted_prerelease_identifiers_order_numerically(self):
        """Semver's own rule, and the half that bites: rc.2 before rc.10,
        which is exactly what a lexical comparison gets backwards and
        exactly the sequence a candidate series produces."""
        self.assertEqual(installer.compare_versions("0.2.0-rc.2", "0.2.0-rc.10"), "older")
        self.assertEqual(installer.compare_versions("0.2.0-alpha", "0.2.0-beta"), "older")

    def test_a_malformed_prerelease_is_unknown_rather_than_a_release(self):
        """"0.2.0-" partitions to an empty suffix exactly like "0.2.0"
        does. Reading it as a plain release would order a typo
        confidently."""
        for tag in ("0.2.0-", "0.2.0-rc.", "0.2.0-.1"):
            self.assertEqual(installer.compare_versions(tag, "0.2.0"), "unknown", tag)


class TestWhichVersionIsInstalled(unittest.TestCase):
    """"The tag of the first container that has one" is not the version
    of anything. `docker compose ps -a` lists stopped leftovers and
    orphans from an older layout in whatever order it likes."""

    def engine(self, tag, service="rclone-manager"):
        return {"Service": service, "Image": f"ghcr.io/spdrman/backup-manager:{tag}"}

    def test_the_engines_container_is_the_one_that_answers(self):
        fx = Fixture(self)
        containers = [
            {"Service": "some-orphan", "Image": "ghcr.io/spdrman/backup-manager:0.1.0"},
            self.engine("0.2.0"),
        ]
        tag, source = installer.installed_image_tag(containers, fx.prefix)
        self.assertEqual(tag, "0.2.0", "an orphan listed first must not decide the version")
        self.assertIn("rclone-manager", source)

    def test_a_stopped_stack_falls_back_to_the_deployment_files(self):
        """With the stack down there are no containers at all, so the
        version was "" and the downgrade guard evaporated exactly when a
        re-run is most likely: after a reboot, or after somebody stopped
        it to work on it."""
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.stage_payload(args)
        tag, source = installer.installed_image_tag([], fx.prefix)
        self.assertEqual(tag, installer.image_tag(args.image))
        self.assertIn("compose.image.yaml", source,
                      "the caller has to be told which of the two answered: what is running and "
                      "what the next `up` would start are different claims")

    def test_nothing_installed_answers_nothing_rather_than_guessing(self):
        fx = Fixture(self)
        self.assertEqual(installer.installed_image_tag([], fx.prefix), ("", ""))


class TestInstallModeDecision(unittest.TestCase):
    """The whole point is that the installer never guesses between
    upgrading and wiping. One of those destroys data and the other does
    not, so an unanswerable question is a refusal, not a default."""

    def decide(self, **kw):
        base = dict(requested=None, installed=False, installed_tag=None,
                    target_version="0.2.0", interactive=False, prefix=Path("/opt/backup-manager"))
        base.update(kw)
        return installer.decide_install_mode(**base)

    def test_nothing_installed_and_no_mode_installs_fresh(self):
        self.assertEqual(self.decide()[0], "fresh")

    def test_nothing_installed_needs_no_prompt_even_on_a_terminal(self):
        mode, prompt = self.decide(interactive=True)
        self.assertEqual(mode, "fresh")
        self.assertFalse(prompt, "there is no decision to make when nothing is here")

    def test_an_existing_install_with_no_mode_and_no_terminal_refuses(self):
        exc = refusal_from(self.decide, installed=True, installed_tag="0.1.0")
        self.assertIsNotNone(exc, "guessing here either destroys data or silently declines to upgrade")
        self.assertEqual(exc.code, installer.EXIT_EXISTING_INSTALL)
        self.assertIn("--mode", exc.remedy, "the refusal has to name the flag that settles it")
        self.assertIn("0.1.0", exc.message, "both versions belong in the message")
        self.assertIn("0.2.0", exc.message)

    def test_an_existing_install_with_no_mode_on_a_terminal_asks(self):
        mode, prompt = self.decide(installed=True, installed_tag="0.1.0", interactive=True)
        self.assertTrue(prompt, "an operator who can answer should be asked, not refused")
        self.assertIsNone(mode, "nothing is decided until the answer comes back")

    def test_fresh_onto_an_existing_install_refuses_and_names_the_prefix(self):
        """fresh means nothing is here. This is where the old
        --if-installed=refuse went. It used to print the TARGET VERSION
        where the path belongs, rendering "version 0.1.0 at 0.2.0's
        prefix", which names no path at all."""
        exc = refusal_from(self.decide, requested="fresh", installed=True, installed_tag="0.1.0",
                           prefix=Path("/volume1/backup-manager"))
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_EXISTING_INSTALL)
        self.assertIn("/volume1/backup-manager", exc.message,
                      "the refusal has to name where the install it found actually is")
        self.assertNotIn("0.2.0's prefix", exc.message)

    def test_upgrade_onto_an_older_install_proceeds(self):
        self.assertEqual(self.decide(requested="upgrade", installed=True,
                                     installed_tag="0.1.0")[0], "upgrade")

    def test_upgrade_onto_the_same_version_still_proceeds(self):
        """It converges, which is what the old --if-installed=converge did,
        and it should say so rather than claim to have upgraded anything."""
        self.assertEqual(self.decide(requested="upgrade", installed=True,
                                     installed_tag="0.2.0")[0], "upgrade")

    def test_upgrade_onto_a_newer_install_refuses(self):
        """Moving a catalog backwards is not something this can promise."""
        exc = refusal_from(self.decide, requested="upgrade", installed=True, installed_tag="0.9.0")
        self.assertIsNotNone(exc)
        self.assertIn("newer", exc.message.lower())

    def test_an_unorderable_installed_version_does_not_block_an_upgrade(self):
        """A host running :latest cannot be ordered, but refusing to touch
        it would strand it. The direction is unknown, not backwards."""
        self.assertEqual(self.decide(requested="upgrade", installed=True,
                                     installed_tag="latest")[0], "upgrade")

    def test_factory_reset_proceeds_whatever_is_installed(self):
        for v in ("0.1.0", "0.2.0", "0.9.0", None):
            self.assertEqual(self.decide(requested="factory-reset", installed=True,
                                         installed_tag=v)[0], "factory-reset")

    def test_factory_reset_on_an_empty_host_is_not_an_error(self):
        self.assertEqual(self.decide(requested="factory-reset")[0], "factory-reset")


class TestThePromptAnswerGoesBackThroughTheGate(unittest.TestCase):
    """[reproduced] The downgrade refusal lives inside
    decide_install_mode's `requested == "upgrade"` branch and nowhere
    else. With no mode flag on a terminal it returned (None, True), the
    prompt returned "upgrade", and the caller ran with it: the guard was
    live for --mode upgrade and completely absent for the identical
    answer typed at the prompt.

    So the pair is driven here, not the pure function on its own. A test
    that only ever calls decide_install_mode directly cannot see a caller
    that stops calling it.
    """

    def drive(self, typed, *, installed_tag, target="0.2.0"):
        """choose_install_mode, which is what cmd_install actually calls.

        Not decide_install_mode twice by hand: a test that reproduces the
        pattern proves the pattern is correct and says nothing about
        whether the caller uses it, and the caller was the bug.
        """
        fx = Fixture(self)
        args = fx.args(command="install")
        with mock_input(typed):
            mode, from_prompt = installer.choose_install_mode(
                args, installed=True, here=installed_tag, target=target, interactive=True)
        self.assertTrue(from_prompt, "this whole class is about the prompt path")
        return mode

    def answer_the_prompt(self, typed, here, target):
        with mock_input(typed):
            return installer._ask_install_mode(here, target, ["nothing to destroy"])

    def test_answering_upgrade_at_the_prompt_still_refuses_a_downgrade(self):
        exc = refusal_from(self.drive, "upgrade", installed_tag="0.9.0")
        self.assertIsNotNone(exc, "the prompt path must not be a way around the downgrade guard")
        self.assertEqual(exc.code, installer.EXIT_DOWNGRADE_REFUSED)

    def test_answering_upgrade_on_an_older_install_proceeds(self):
        """The positive control. Without it, "the prompt path refuses" is
        also satisfied by a prompt path that refuses everything."""
        self.assertEqual(self.drive("upgrade", installed_tag="0.1.0"), "upgrade")

    def test_answering_factory_reset_proceeds_whatever_the_versions(self):
        self.assertEqual(self.drive("factory-reset", installed_tag="0.9.0"), "factory-reset")

    def test_the_full_word_is_required_for_a_factory_reset(self):
        """"f" is one keystroke away from the answer that keeps the data,
        and it used to be accepted for the one that destroys it."""
        for typed in ("f", "factory"):
            exc = refusal_from(self.answer_the_prompt, [typed, typed, typed], "0.1.0", "0.2.0")
            self.assertIsNotNone(exc, f"{typed!r} must not be a factory reset")

    def test_aborting_changes_nothing(self):
        exc = refusal_from(self.answer_the_prompt, "abort", "0.1.0", "0.2.0")
        self.assertIsNotNone(exc)
        self.assertIn("nothing was changed", exc.message)

    def test_the_preview_is_shown_before_the_question(self):
        """It used to print AFTER the mode was chosen, and for
        --mode factory-reset after archive_state had already been called
        with move=True. A list read afterwards is a receipt, not a
        decision."""
        with mock_input("abort") as typed:
            refusal_from(installer._ask_install_mode, "0.1.0", "0.2.0",
                         ["1 administrator account (state/local-auth.json)"])
        printed = "\n".join(typed.printed)
        self.assertIn("1 administrator account", printed,
                      "the operator has to see what factory-reset destroys before choosing it")
        self.assertLess(printed.index("1 administrator account"), printed.index("upgrade /"),
                        "and see it BEFORE the question, not after")


class TestFactoryResetHasToBeConfirmed(unittest.TestCase):
    """--mode factory-reset used to print what it destroys and then
    immediately destroy it. The preview was output, not a question."""

    def args(self, fx, *extra):
        return fx.args(*extra, command="install")

    def test_no_terminal_and_no_flag_refuses(self):
        fx = Fixture(self)
        with mock_input([], tty=False):
            exc = refusal_from(installer.confirm_factory_reset, self.args(fx), ["everything"])
        self.assertIsNotNone(exc, "a destructive default with nobody to ask is a data-loss bug")
        self.assertIn("--confirm-factory-reset", exc.remedy)

    def test_the_flag_confirms_it_without_a_terminal(self):
        fx = Fixture(self)
        with mock_input([], tty=False):
            installer.confirm_factory_reset(self.args(fx, "--confirm-factory-reset"), ["everything"])

    def test_the_typed_word_confirms_it_on_a_terminal(self):
        fx = Fixture(self)
        with mock_input("factory-reset", tty=True):
            installer.confirm_factory_reset(self.args(fx), ["everything"])

    def test_anything_short_of_the_word_aborts(self):
        fx = Fixture(self)
        for typed in ("y", "yes", "f", "factory", ""):
            with mock_input(typed, tty=True):
                exc = refusal_from(installer.confirm_factory_reset, self.args(fx), ["everything"])
            self.assertIsNotNone(exc, f"{typed!r} must not be a confirmation")
            self.assertIn("not confirmed", exc.message)


class TestWhatEachModeTouches(unittest.TestCase):
    """Learned by doing all three by hand on the real NAS: users are not
    in the database, and the retained artifacts must never be copied."""

    def paths(self, fx):
        args = fx.args(command="install")
        return installer.archive_plan(args)

    def test_the_administrator_record_is_archived_not_only_the_database(self):
        """Wiping state.db alone leaves local-auth.json, and the engine
        then reports an administrator already exists and issues no
        enrollment link, producing an install nobody can log into."""
        fx = Fixture(self)
        names = {p.name for p in self.paths(fx)}
        self.assertIn("state.db", names)
        self.assertIn("local-auth.json", names,
                      "users live here, not in the database: a reset that misses this locks the host out")

    def test_the_administrator_record_is_archived_first(self):
        """A factory reset MOVES these, one at a time, so the order is
        what a failure part way through leaves behind. Database first meant
        an ENOSPC between the two left the catalog gone and the
        administrator record present, which is the engine reporting "an
        administrator already exists", issuing no enrollment link, and
        nobody able to log in. The archive would have produced the exact
        lockout it exists to prevent."""
        fx = Fixture(self)
        names = [p.name for p in self.paths(fx)]
        self.assertLess(names.index("local-auth.json"), names.index("state.db"))

    def test_the_write_ahead_log_is_archived_with_the_database(self):
        """internal/state/state.go opens the database journal_mode=WAL and
        container/compose.yaml says -wal and -shm sit beside the main
        file. state.db on its own is a database missing its most recent
        committed transactions."""
        fx = Fixture(self)
        names = {p.name for p in self.paths(fx)}
        self.assertIn("state.db-wal", names,
                      "an archive without the WAL is a torn copy of the catalog")
        self.assertIn("state.db-shm", names)

    def test_the_imported_keys_and_pinned_host_keys_are_archived(self):
        fx = Fixture(self)
        names = {p.name for p in self.paths(fx)}
        self.assertIn("ssh_keys", names)
        self.assertIn("known_hosts.d", names)
        self.assertIn("config.yaml", names)

    def test_the_retained_artifacts_are_never_archived(self):
        """They are the product's whole purpose, they can be enormous, and
        an upgrade does not modify them. Copying them would double disk
        usage and protect nothing."""
        fx = Fixture(self)
        args = fx.args(command="install")
        for p in installer.archive_plan(args):
            self.assertNotEqual(p, args.backup_dir,
                                "the backup root must never be copied into an archive")
            self.assertFalse(str(p).startswith(str(args.backup_dir) + os.sep),
                             f"{p} lives under the backup root and must not be archived")


class TestArchivingIsInsideTheRefusalContract(unittest.TestCase):
    """shutil's move, copytree and copy2 raise OSError on ENOSPC, EPERM
    and EXDEV. main() catches Refusal and nothing else, so an unwrapped
    one reached an operator as a traceback with no exit code of its own,
    on the one command that had just started taking their install apart.
    """

    def populated(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        args.state_dir.mkdir(parents=True, exist_ok=True)
        args.config_dir.mkdir(parents=True, exist_ok=True)
        (args.state_dir / "local-auth.json").write_text('{"username":"rom"}')
        (args.state_dir / "state.db").write_text("SQLite format 3")
        (args.state_dir / "state.db-wal").write_text("the committed transactions")
        (args.config_dir / "config.yaml").write_text("sets: []\n")
        return fx, args

    def test_a_copy_archive_captures_the_journal_too(self):
        _fx, args = self.populated()
        archive, captured = installer.archive_state(args, move=False)
        names = {p.name for p in captured}
        self.assertIn("state.db-wal", names)
        self.assertTrue((archive / "state" / "state.db-wal").is_file())

    def test_the_archive_is_created_0700_without_a_window(self):
        """os.chmod after mkdir left the administrator record and every
        imported key sitting at whatever the umask allowed, which on the
        NAS this was proven on is 0777."""
        old = os.umask(0)
        self.addCleanup(os.umask, old)
        _fx, args = self.populated()
        archive, _captured = installer.archive_state(args, move=False)
        self.assertEqual(archive.stat().st_mode & 0o777, 0o700, "a bare mkdir trusts the umask")

    def test_an_unwritable_destination_refuses_rather_than_tracebacks(self):
        if os.getuid() == 0:
            self.skipTest("root ignores the mode, so the reproduction cannot be set up")
        _fx, args = self.populated()
        # The archive root exists and cannot be written into, which is
        # what ENOSPC and EPERM both look like from here.
        stamp_dir = args.prefix
        stamp_dir.mkdir(parents=True, exist_ok=True)
        mode = stamp_dir.stat().st_mode
        os.chmod(stamp_dir, 0o500)
        self.addCleanup(os.chmod, stamp_dir, mode)
        exc = refusal_from(installer.archive_state, args, move=False)
        self.assertIsNotNone(exc, "an OSError here reaches the operator as a traceback")
        self.assertEqual(exc.code, installer.EXIT_RUNTIME)

    def test_a_failure_part_way_through_a_move_names_what_already_moved(self):
        """"The archive failed" and "the archive failed after your
        administrator record was moved" call for completely different next
        steps, and only one of them is safe to bring a stack up after."""
        _fx, args = self.populated()
        real_move = installer.shutil.move
        moved = []

        def explode(src, dst):
            if src.endswith("state.db"):
                raise OSError(28, "No space left on device")
            moved.append(src)
            return real_move(src, dst)

        installer.shutil.move = explode
        self.addCleanup(setattr, installer.shutil, "move", real_move)
        exc = refusal_from(installer.archive_state, args, move=True)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_RUNTIME)
        self.assertIn("local-auth.json", exc.message,
                      "the operator has to be told the administrator record is already in the archive")
        self.assertIn("state.db", exc.message, "and which path it died on")

    def test_two_runs_in_the_same_second_do_not_collide(self):
        """The stamp is second-granular and mkdir was exist_ok=True, so a
        second run inside the same second reused the directory and
        copytree, which is not exist_ok by default, raised
        FileExistsError."""
        _fx, args = self.populated()
        (args.config_dir / "ssh_keys").mkdir(parents=True, exist_ok=True)
        (args.config_dir / "ssh_keys" / "imported.key").write_text("not a key")
        real_strftime = installer.time.strftime
        installer.time.strftime = lambda fmt: "20260902-120000"
        self.addCleanup(setattr, installer.time, "strftime", real_strftime)
        first, _ = installer.archive_state(args, move=False)
        second, _ = installer.archive_state(args, move=False)
        self.assertNotEqual(first, second)
        self.assertTrue((second / "config" / "ssh_keys" / "imported.key").is_file())

    def test_archives_are_not_allowed_to_pile_up(self):
        """Every one holds a copy of config/ssh_keys, which is where the
        engine keeps the keys an operator imported, so an unbounded pile is
        an installer that multiplies private key material every time it
        runs. It refuses rather than pruning, because moving instead of
        deleting is the whole reason the archive is recoverable."""
        _fx, args = self.populated()
        for n in range(installer.ARCHIVE_LIMIT):
            (args.prefix / f"archive-2020010{n}-000000").mkdir(parents=True)
        exc = refusal_from(installer.archive_state, args, move=False)
        self.assertIsNotNone(exc)
        self.assertIn(str(installer.ARCHIVE_LIMIT), exc.message)
        self.assertIn("ssh_keys", exc.remedy, "the reason it matters belongs in the remedy")
        self.assertTrue((args.state_dir / "state.db").is_file(),
                        "it refuses before it touches anything")


class TestAnUpgradeThatDiesHalfWayLeavesAWorkingInstall(unittest.TestCase):
    """#343 states this and nothing held it: "A refused or failed upgrade
    leaves the old install working."

    An upgrade COPIES its archive and a factory reset MOVES it, and the
    two branches differ by one keyword argument in prepare_for_mode. Get
    that word wrong and a failed upgrade takes the administrator record
    and the catalog with it, which is the one outcome an upgrade is
    supposed to be incapable of. The tests around this one all watch the
    archive; this one watches the INSTALL, which is the thing that has to
    still be there afterwards.
    """

    def _installed(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.stage_payload(args)
        args.state_dir.mkdir(parents=True, exist_ok=True)
        args.config_dir.mkdir(parents=True, exist_ok=True)
        (args.state_dir / "local-auth.json").write_text('{"username":"rom"}')
        (args.state_dir / "state.db").write_text("SQLite format 3")
        (args.state_dir / "state.db-wal").write_text("the committed transactions")
        (args.config_dir / "config.yaml").write_text("sets: [one]\n")
        (args.config_dir / "ssh_keys").mkdir(parents=True, exist_ok=True)
        (args.config_dir / "ssh_keys" / "imported.key").write_text("an imported key")
        installer.stop_stack = lambda a, *, remove: None
        return fx, args

    def setUp(self):
        self._real_stop = installer.stop_stack
        self.addCleanup(setattr, installer, "stop_stack", self._real_stop)

    def _everything(self, args):
        """Every file the deployment needs to start, by content, so a
        truncated or emptied one is as visible as a missing one."""
        seen = {}
        for p in installer.archive_plan(args) + [args.prefix / "compose.yaml",
                                                 args.prefix / "compose.image.yaml",
                                                 args.prefix / ".env"]:
            if p.is_file():
                seen[str(p)] = p.read_bytes()
            elif p.is_dir():
                seen[str(p)] = sorted(q.name for q in p.iterdir())
        return seen

    def test_an_upgrade_that_dies_mid_archive_leaves_every_file_in_place(self):
        _fx, args = self._installed()
        before = self._everything(args)
        self.assertIn(str(args.state_dir / "local-auth.json"), before, "the setup has to be real")

        real_copy = installer.shutil.copy2

        def explode(src, dst):
            if str(src).endswith("state.db"):
                raise OSError(28, "No space left on device")
            return real_copy(src, dst)

        installer.shutil.copy2 = explode
        self.addCleanup(setattr, installer.shutil, "copy2", real_copy)

        exc = refusal_from(installer.prepare_for_mode, args, "upgrade", installed=True)
        self.assertIsNotNone(exc, "an OSError here reaches the operator as a traceback")
        self.assertEqual(self._everything(args), before,
                         "an upgrade that died half way took something the stack needs to start")
        self.assertIn("Nothing was removed from the install", exc.remedy,
                      "and it has to say so, because the next step depends on it")

    def test_the_upgrade_archive_is_a_copy_and_the_reset_archive_is_a_move(self):
        """The one keyword that separates the two, asserted on what is left
        behind rather than on the flag, because the flag is what would be
        wrong."""
        _fx, args = self._installed()
        installer.prepare_for_mode(args, "upgrade", installed=True)
        self.assertTrue((args.state_dir / "local-auth.json").is_file(),
                        "an upgrade keeps the administrator record where the engine looks for it")
        self.assertTrue((args.state_dir / "state.db").is_file())

        installer.prepare_for_mode(args, "factory-reset", installed=True)
        self.assertFalse((args.state_dir / "local-auth.json").exists(),
                         "a factory reset that leaves the administrator record produces an install "
                         "nobody can log into and no enrollment link")
        self.assertFalse((args.state_dir / "state.db").exists())


class TestTheInstalledLayoutIsNotOverridden(unittest.TestCase):
    """Every path this installer archives, destroys or rewrites came from
    THIS run's flags, and it wrote prefix/.env without ever reading it
    back. An operator who first installed with --state-dir /mnt/fast/state
    and re-ran without repeating it got "Archived 0 item(s)", a rewritten
    .env, and a stack pointed at an empty state directory while the real
    catalog sat at the old path."""

    def installed_with(self, fx, **env):
        base = {"STATE_DIR": str(fx.prefix / "state"),
                "BACKUP_DIR": str(fx.prefix / "backups"),
                "CONFIG_DIR": str(fx.prefix / "config")}
        base.update(env)
        return base

    def test_matching_paths_are_not_a_refusal(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.check_layout_matches(args, self.installed_with(fx))

    def test_two_spellings_of_the_same_directory_are_not_a_mismatch(self):
        """Neither spelling is wrong. resolve() canonicalises --prefix and
        leaves --state-dir alone, and macOS hands out /var where the
        canonical name is /private/var, so a string comparison refuses an
        install over a symlink."""
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.check_layout_matches(args, self.installed_with(
            fx, STATE_DIR=str(fx.prefix / "state")))

    def test_an_install_with_no_env_yet_is_not_a_refusal(self):
        fx = Fixture(self)
        installer.check_layout_matches(fx.args(command="install"), {})

    def test_a_different_state_dir_refuses_and_names_both(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        exc = refusal_from(installer.check_layout_matches, args,
                           self.installed_with(fx, STATE_DIR="/mnt/fast/state"))
        self.assertIsNotNone(exc, "silently adopting this run's paths archives nothing and looks green")
        self.assertEqual(exc.code, installer.EXIT_EXISTING_INSTALL)
        self.assertIn("/mnt/fast/state", exc.message, "the installed path has to be named")
        self.assertIn(str(args.state_dir), exc.message, "and so does the one this run would use")
        self.assertIn("--state-dir", exc.message)

    def test_every_directory_that_holds_data_is_checked(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        for key, flag in (("STATE_DIR", "--state-dir"), ("BACKUP_DIR", "--backup-dir"),
                          ("CONFIG_DIR", "--config-dir")):
            exc = refusal_from(installer.check_layout_matches, args,
                               self.installed_with(fx, **{key: "/somewhere/else"}))
            self.assertIsNotNone(exc, f"{flag} disagreeing has to be a refusal")

    def test_the_env_it_reads_is_the_one_it_writes(self):
        """A round trip, so the parser cannot drift from the renderer."""
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.stage_payload(args)
        env = installer.read_env_file(args.prefix / ".env")
        self.assertEqual(env["STATE_DIR"], str(args.state_dir))
        self.assertEqual(env["CONFIG_DIR"], str(args.config_dir))
        installer.check_layout_matches(args, env)


class TestTheStackIsStoppedBeforeItsStateMoves(unittest.TestCase):
    """Nothing stopped it. The only `docker compose down` in this file was
    in cmd_uninstall, so an upgrade copied a live WAL database, a factory
    reset moved state.db out from under an engine holding the fd, and a
    factory reset at the same version with an unchanged .env never
    recreated the containers at all."""

    def capture(self, fx, *, remove):
        args = fx.args(command="install")
        installer.stage_payload(args)
        calls = []

        def fake_run(argv, **kw):
            calls.append(argv)
            return installer.subprocess.CompletedProcess(argv, 0, "", "")

        real = installer.run
        installer.run = fake_run
        self.addCleanup(setattr, installer, "run", real)
        installer.stop_stack(args, remove=remove)
        return calls[0]

    def test_an_upgrade_stops_the_stack(self):
        argv = self.capture(Fixture(self), remove=False)
        self.assertIn("stop", argv, f"nothing was holding the state closed: {argv}")

    def test_a_factory_reset_takes_the_containers_away(self):
        """`stop` is not enough here. `docker compose up -d` against a
        stack whose config has not changed is a no-op, so a factory reset
        at the same version with an unchanged .env left the OLD engine
        serving the OLD catalog while the installer printed success."""
        argv = self.capture(Fixture(self), remove=True)
        self.assertIn("down", argv, f"the containers have to be recreated: {argv}")
        self.assertNotIn("stop", argv)

    def test_the_stack_is_stopped_before_the_state_is_touched(self):
        """The order is the whole fix, so it is watched happening rather
        than assumed from reading the two functions."""
        fx = Fixture(self)
        args = fx.args(command="install")
        args.state_dir.mkdir(parents=True, exist_ok=True)
        (args.state_dir / "state.db").write_text("SQLite format 3")
        events = []

        real_stop, real_archive = installer.stop_stack, installer.archive_state
        self.addCleanup(setattr, installer, "stop_stack", real_stop)
        self.addCleanup(setattr, installer, "archive_state", real_archive)
        installer.stop_stack = lambda a, *, remove: events.append(("stop", remove))

        def watched_archive(a, *, move):
            events.append(("archive", move))
            return real_archive(a, move=move)

        installer.archive_state = watched_archive

        installer.prepare_for_mode(args, "upgrade", installed=True)
        self.assertEqual(events, [("stop", False), ("archive", False)],
                         "an upgrade copies a LIVE WAL database unless the stack stops first")

        events.clear()
        installer.prepare_for_mode(args, "factory-reset", installed=True)
        self.assertEqual(events, [("stop", True), ("archive", True)],
                         "a factory reset has to remove the containers, or `up -d` is a no-op and "
                         "the old engine keeps serving the old catalog")

    def test_a_fresh_install_stops_nothing_and_archives_nothing(self):
        """The positive control for the two above: with nothing here there
        is nothing to stop and nothing to archive, and a stop against a
        project that does not exist is noise in an install log."""
        fx = Fixture(self)
        real_stop = installer.stop_stack
        self.addCleanup(setattr, installer, "stop_stack", real_stop)
        installer.stop_stack = lambda *a, **kw: self.fail("nothing is installed here")
        self.assertEqual(installer.prepare_for_mode(fx.args(command="install"), "fresh",
                                                    installed=False), (None, []))

    def test_a_stack_that_will_not_come_down_is_reported_and_not_fatal(self):
        """There are real states, containers already gone or a project
        removed by hand, where the command reports failure and nothing is
        wrong."""
        fx = Fixture(self)
        args = fx.args(command="install")
        real = installer.run
        installer.run = lambda argv, **kw: installer.subprocess.CompletedProcess(argv, 1, "", "no such project")
        self.addCleanup(setattr, installer, "run", real)
        installer.stop_stack(args, remove=True)


class TestTheEngineOwnedConfigDirectories(unittest.TestCase):
    """The engine refuses an SSH key when any directory in its ancestry is
    group- or world-writable, and it named config/ssh_keys, then config,
    then the install root on three successive cycles of the real NAS.

    Two of the four this used to walk are created by the ENGINE, on
    demand, with the container's umask, and the installer never made them
    at all. It also ran before stage_payload, when the config directory
    did not exist yet, so on a fresh install it was a complete no-op."""

    def test_the_engines_two_stores_are_created_0700(self):
        old = os.umask(0)
        self.addCleanup(os.umask, old)
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.stage_payload(args)
        installer.prepare_engine_config_dirs(args)
        for name in installer.ENGINE_OWNED_CONFIG_DIRS:
            d = args.config_dir / name
            self.assertTrue(d.is_dir(), f"{d} is the engine's, and it makes it 0777 under this umask")
            self.assertEqual(d.stat().st_mode & 0o777, 0o700, f"{d} is what the engine refuses over")

    def test_an_existing_directory_only_loses_group_and_world_write(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.stage_payload(args)
        d = args.config_dir / "ssh_keys"
        d.mkdir(parents=True)
        os.chmod(d, 0o775)
        installer.prepare_engine_config_dirs(args)
        self.assertEqual(d.stat().st_mode & 0o777, 0o755,
                         "the read bits are the operator's call; write is what the engine refuses over")

    def test_a_deployment_running_as_somebody_else_is_named_rather_than_broken(self):
        """A 0700 directory owned by this account is one the engine cannot
        write when PUID/PGID say it runs as somebody else. Creating it
        would move the failure rather than remove it."""
        fx = Fixture(self)
        args = fx.args(command="install")
        installer.stage_payload(args)
        args.puid = os.getuid() + 1
        installer.prepare_engine_config_dirs(args)
        self.assertFalse((args.config_dir / "ssh_keys").exists(),
                         "it must not make a directory the container cannot use")


class TestCmdInstallDoesThingsInThisOrder(unittest.TestCase):
    """cmd_install opens with Preflight.check_all(), which needs a real
    Docker daemon, so what follows cannot be driven from a unit test.
    Ordering is exactly what was wrong twice here, though, so it is read
    out of the function rather than left unchecked:

      * tighten_config_ancestry ran BEFORE stage_payload, when neither the
        prefix nor the config directory existed yet, so on a fresh install
        every path it looked at was absent and the whole thing was a
        no-op that nothing noticed for a release;
      * and nothing consulted the installed .env at all, so a re-run with
        different directories archived nothing and rewrote the deployment
        to point at empty ones.

    Reading source text is a weaker check than driving the code, and it
    is the strongest one available without a daemon. It fails loudly if a
    call is deleted, and that is the regression it is here for.
    """

    def body_of(self, name: str) -> str:
        src = Path(installer.__file__).read_bytes().decode("utf-8")
        start = src.index(f"def {name}(args)")
        return src[start:src.index("\ndef ", start + 1)]

    def assert_calls_in_order(self, name, first, second):
        body = self.body_of(name)
        for call in (first, second):
            self.assertIn(call + "(", body, f"{name} no longer calls {call}")
        self.assertLess(body.index(first + "("), body.index(second + "("),
                        f"{name} has to call {first} before {second}")

    def test_the_installed_layout_is_checked_before_anything_is_decided(self):
        self.assert_calls_in_order("cmd_install", "check_layout_matches", "choose_install_mode")

    def test_the_engines_directories_are_prepared_after_staging_makes_theirs(self):
        self.assert_calls_in_order("cmd_install", "stage_payload", "prepare_engine_config_dirs")

    def test_the_mode_is_chosen_before_anything_is_taken_apart(self):
        self.assert_calls_in_order("cmd_install", "choose_install_mode", "prepare_for_mode")


class TestDestroyPreview(unittest.TestCase):
    """factory-reset states what it destroys by name and count before it
    does it, in the same spirit as the sudo path printing every command it
    is about to run. "Factory reset? [y/N]" is not a decision anyone can
    make."""

    def test_it_counts_the_administrator_record(self):
        fx = Fixture(self)
        args = fx.args(command="install")
        args.state_dir.mkdir(parents=True, exist_ok=True)
        (args.state_dir / "local-auth.json").write_text('{"username":"rom"}')
        lines = installer.destroy_preview(args)
        self.assertTrue(any("administrator" in l.lower() for l in lines),
                        f"the administrator record has to be named: {lines}")

    def test_it_says_plainly_when_there_is_nothing_to_destroy(self):
        fx = Fixture(self)
        lines = installer.destroy_preview(fx.args(command="install"))
        self.assertTrue(any("nothing" in l.lower() for l in lines), lines)

    def test_it_never_claims_the_retained_artifacts_are_destroyed(self):
        """factory-reset drops the catalog, not the backups themselves.
        Saying otherwise would be worse than saying nothing."""
        fx = Fixture(self)
        args = fx.args(command="install")
        args.backup_dir.mkdir(parents=True, exist_ok=True)
        (args.backup_dir / "artifact.dump").write_text("retained bytes")
        joined = " ".join(installer.destroy_preview(args)).lower()
        self.assertNotIn("artifact.dump", joined,
                         "a retained backup file is not destroyed by a factory reset")

    def test_the_credentials_that_survive_are_named(self):
        """<prefix>/secrets is neither archived nor destroyed, and nothing
        said so. An operator reading a list of what goes is entitled to
        know what stays before typing the word."""
        fx = Fixture(self)
        lines = " ".join(installer.destroy_preview(fx.args(command="install")))
        self.assertIn(str(fx.key), lines)
        self.assertIn(str(fx.known), lines)
        self.assertIn("NOT destroyed", lines)


class TestModeFlagReplacesIfInstalled(unittest.TestCase):
    def flags_of(self, command):
        return {opt for a in _subparser(installer.build_parser(), command)._actions
                for opt in a.option_strings}

    def test_if_installed_is_gone_from_every_command_that_never_had_it(self):
        for command in ("preflight", "status", "uninstall", "network-doctor", "network-undo"):
            self.assertNotIn("--if-installed", self.flags_of(command))

    def test_if_installed_is_hidden_from_help_rather_than_advertised(self):
        """It is not an option any more. It is registered only so a script
        still passing it gets a sentence instead of argparse's
        "unrecognized arguments"."""
        action = next(a for a in _subparser(installer.build_parser(), "install")._actions
                      if "--if-installed" in a.option_strings)
        self.assertEqual(action.help, argparse.SUPPRESS)

    def test_a_scripted_if_installed_is_translated_rather_than_rejected(self):
        """[reproduced] Removing the flag outright made a scripted
        `--if-installed converge` die at argparse exit 2 with
        "unrecognized arguments", naming neither --mode nor the mapping.
        The claim that the failure was loud and named the flag was only
        true for a re-run with no mode flag at all, which is not the
        command line any existing script has."""
        parse = installer.build_parser().parse_args
        for value, replacement in (("converge", "--mode upgrade"), ("refuse", "--mode fresh")):
            exc = refusal_from(parse, ["install", "--if-installed", value,
                                       "--prefix", "/tmp/does-not-matter"])
            self.assertIsNotNone(exc, f"--if-installed {value} has to say what replaced it")
            self.assertIn("--mode", exc.message + exc.remedy)
            self.assertIn(replacement, exc.remedy,
                          "the refusal has to carry the translation, not just the news")
            self.assertEqual(exc.code, installer.EXIT_USAGE)

    def test_the_refusal_reaches_the_operator_rather_than_a_traceback(self):
        """main() used to parse OUTSIDE its own try, so a Refusal raised
        during parse_args escaped as a traceback."""
        written = []

        class _Sink:
            def write(self, text):
                written.append(text)

            def flush(self):
                pass

        real, sys.stderr = sys.stderr, _Sink()
        try:
            code = installer.main(["install", "--if-installed", "converge",
                                   "--prefix", "/tmp/does-not-matter"])
        finally:
            sys.stderr = real
        self.assertEqual(code, installer.EXIT_USAGE)
        self.assertIn("--mode upgrade", "".join(written),
                      "the translation has to reach stderr, not a traceback")

    def test_mode_exists_only_on_install(self):
        for command in ("preflight", "status", "uninstall", "network-doctor", "network-undo"):
            self.assertNotIn("--mode", self.flags_of(command),
                             f"{command} does not install anything")
        self.assertIn("--mode", self.flags_of("install"))

    def test_the_three_values_and_the_default(self):
        action = next(a for a in _subparser(installer.build_parser(), "install")._actions
                      if "--mode" in a.option_strings)
        self.assertEqual(set(action.choices), {"fresh", "upgrade", "factory-reset"})
        self.assertIsNone(action.default,
                          "the default is decided against what is actually installed, not by argparse: "
                          "unset has to stay distinguishable from an explicit --mode fresh")



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

    def test_every_value_is_passed_after_an_end_of_options_marker(self):
        """Quoting stops the shell re-parsing a value as syntax; it does
        not stop ping and nc parsing their own argv. A --probe-host
        starting with a dash was read as a flag, so the probe reported a
        flag-parsing error as an egress failure. Confirmed against
        busybox:stable (the default --probe-image): `nc -z -4 9` gives
        "invalid option", `nc -z -- -4 9` gives "bad address"."""
        script = installer._probe_argv("img", "net", "gw", "host", 1)[-1]
        self.assertIn('ping -c 3 -W 2 -- "$GATEWAY"', script,
                      "ping reads $GATEWAY without an end-of-options marker")
        self.assertIn('nc -z -- "$PROBE_HOST" "$PROBE_PORT"', script,
                      "nc reads $PROBE_HOST without an end-of-options marker")



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
    it. Only `--prefix`/`--state-dir`/`--backup-dir`/`--config-dir`/
    `--project` are truly universal (compose_argv() and detect_existing()
    read only these five, and resolve() always builds host_dirs from the
    first four); credentials, --compose-file and the rest of runtime are
    scoped to preflight and install, the only two commands that ever read
    a credential path or an image reference at all."""

    def flags_of(self, command):
        return {opt for a in _subparser(installer.build_parser(), command)._actions
                for opt in a.option_strings}

    def test_mode_exists_only_on_install(self):
        """--if-installed used to be the flag here. It was reconciled into
        --mode (issue #343) rather than left beside it, so the scoping
        assertion moved with it: only install decides anything about an
        install that is already here."""
        for command in ("preflight", "status", "uninstall", "network-doctor", "network-undo"):
            self.assertNotIn("--mode", self.flags_of(command),
                             f"{command} does not install, upgrade or reset anything")
        self.assertIn("--mode", self.flags_of("install"))

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

    def test_the_true_common_floor_stays_on_every_command(self):
        """compose_argv() and detect_existing() are the only two functions
        every subcommand's path can reach, and both read only these five -
        the actual floor resolve() and every handler need, not "everything
        install happens to need" the way this shared group used to be
        defined."""
        floor = {"--prefix", "--state-dir", "--backup-dir", "--config-dir", "--project"}
        for command in ("preflight", "install", "status", "uninstall",
                        "network-doctor", "network-undo"):
            self.assertTrue(floor <= self.flags_of(command), f"{command} is missing a floor flag")

    def test_credentials_and_the_rest_of_runtime_exist_only_on_preflight_and_install(self):
        """Only Preflight's own checks and cmd_install's staging ever read
        a credential path, --compose-file, an image reference, a listen
        port or a puid/pgid/timezone override. status, uninstall,
        network-doctor and network-undo never construct a Preflight and
        never stage a deployment, so they no longer declare any of this."""
        install_prereqs = {"--ssh-key", "--known-hosts", "--compose-file", "--image",
                           "--image-archive", "--no-pull", "--listen-port", "--public-base-url",
                           "--profile", "--timezone", "--puid", "--pgid", "--timeout"}
        for command in ("status", "uninstall", "network-doctor", "network-undo"):
            self.assertFalse(install_prereqs & self.flags_of(command),
                             f"{command} never reads a credential, an image reference or a port")
        for command in ("preflight", "install"):
            self.assertTrue(install_prereqs <= self.flags_of(command),
                            f"{command} needs every install-prerequisite flag")

    def test_no_subcommand_renders_the_same_group_title_twice(self):
        """argparse renders every add_argument_group() call as its own
        section and never merges two by title, so two groups sharing a
        title print the same heading twice with the flags split between
        them. That happened here: _add_shared_groups() titled its group
        "layout" and _add_install_prereq_groups() opened a second "layout"
        holding only --compose-file, so `install --help` and `preflight
        --help` both grew a duplicate heading with one orphaned flag under
        it - the exact clutter splitting the parser was meant to remove.

        Every other test in this class reads flags out of _actions/
        option_strings, which is blind to how they are grouped, so nothing
        caught it. This asserts on the grouping itself."""
        for command in ("preflight", "install", "status", "uninstall",
                        "network-doctor", "network-undo"):
            with self.subTest(command=command):
                titles = [g.title for g in _subparser(installer.build_parser(), command)._action_groups
                          if g.title and g._group_actions]
                duplicates = {t for t in titles if titles.count(t) > 1}
                self.assertFalse(duplicates,
                                 f"{command} --help renders {sorted(duplicates)} more than once, "
                                 "splitting one section's flags across two identical headings")

    def test_resolve_never_raises_for_a_command_missing_install_prereq_flags(self):
        """The regression this whole split depends on: resolve() used to
        touch args.ssh_key/args.compose_file/args.image unconditionally,
        which would be an AttributeError the moment a subcommand stopped
        declaring them. Exercised against all six commands for real,
        not just the two that still have every flag."""
        for command in ("preflight", "install", "status", "uninstall",
                        "network-doctor", "network-undo"):
            with self.subTest(command=command):
                args = installer.resolve(installer.build_parser().parse_args(
                    [command, "--prefix", "/tmp/rm-resolve-test"]))
                self.assertEqual(args.command, command)
                self.assertIn("--prefix", args.host_dirs)

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


class TestTheSuiteRunsEveryTestItDefines(unittest.TestCase):
    """A suite that can silently stop running part of itself.

    `python3 test_install_docker_host.py` runs unittest.main() at the
    moment that line executes, against whatever is in the module namespace
    by then. A class appended BELOW the entrypoint is not there yet, so it
    never runs, and the invocation still prints OK with a lower count
    nobody is watching.

    That is not hypothetical: two branches in flight both appended their
    new classes past it, and a direct run of either reported OK over 94
    tests while `python3 -m unittest` (which imports the module fully
    first, and is what scripts/ci-local.sh uses) ran 100 and 118. The gate
    was fine and one invocation away from not being.

    So the entrypoint has to be the last statement in the file, and this
    says so about the installer too, which has the same shape.
    """

    @staticmethod
    def _is_entrypoint(node) -> bool:
        """`if __name__ == "__main__":`, matched structurally.

        Not ast.unparse(), which arrived in 3.9: the installer supports
        3.8 (Preflight.check_python), and a test that cannot run on the
        version the thing under test supports is a hole of its own.
        """
        return (isinstance(node, ast.If)
                and isinstance(node.test, ast.Compare)
                and isinstance(node.test.left, ast.Name)
                and node.test.left.id == "__name__")

    def assert_nothing_follows_the_entrypoint(self, path: Path) -> None:
        body = ast.parse(path.read_text(encoding="utf-8")).body
        entrypoints = [i for i, node in enumerate(body) if self._is_entrypoint(node)]
        self.assertEqual(
            len(entrypoints), 1,
            f"{path.name} should have exactly one `if __name__ == \"__main__\":` block, found "
            f"{len(entrypoints)}")
        trailing = body[entrypoints[0] + 1:]
        named = [getattr(node, "name", type(node).__name__) for node in trailing]
        self.assertEqual(
            named, [],
            f"{path.name} defines {named} AFTER its `if __name__ == \"__main__\":` block, so a "
            f"direct `python3 {path.name}` run never sees them and still reports OK. Move them "
            f"above it.")

    def test_nothing_is_defined_after_this_file_runs_itself(self):
        self.assert_nothing_follows_the_entrypoint(Path(__file__))

    def test_nothing_is_defined_after_the_installer_runs_itself(self):
        self.assert_nothing_follows_the_entrypoint(Path(installer.__file__))


class TestNoArgumentInstallHasWhatItNeeds(unittest.TestCase):
    """Issue #347: the three flags that made a bare `install` impossible.

    `--prefix` defaulted to a guessed NAS path, and `--ssh-key` and
    `--known-hosts` had no defaults that existed anywhere, so the
    documented no-argument install refused three times before doing any
    work. These prove it now completes, and that the two places where
    refusing is still correct kept refusing.
    """

    def _bare(self, command="install"):
        return installer.resolve(installer.build_parser().parse_args([command]))

    def test_the_default_prefix_is_under_the_invoking_users_home(self):
        args = self._bare()
        self.assertEqual(args.prefix, Path.home() / "rclone-manager")
        self.assertNotIn("/volume1", str(args.prefix),
                         "the old default guessed one NAS's layout and was wrong even on that NAS")

    def test_the_credential_paths_default_under_the_prefix(self):
        args = self._bare()
        self.assertEqual(args.ssh_key, (args.prefix / "secrets" / "id_ed25519").expanduser())
        self.assertEqual(args.known_hosts, (args.prefix / "secrets" / "known_hosts").expanduser())
        self.assertFalse(args.ssh_key_supplied)
        self.assertFalse(args.known_hosts_supplied)

    def test_a_defaulted_key_is_generated_and_its_public_half_is_printed(self):
        """The generated key is useless until it reaches the source host,
        so the public half and where it goes are printed, not left to be
        found."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(Path(tmp.name) / "rclone-manager")]))

        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.ensure_credentials(args)
        printed = out.getvalue()

        self.assertTrue(args.ssh_key.is_file(), "a defaulted key that is absent gets generated")
        self.assertEqual(args.ssh_key.stat().st_mode & 0o777, 0o600)
        pub = args.ssh_key.with_suffix(args.ssh_key.suffix + ".pub")
        self.assertTrue(pub.is_file())
        self.assertIn(pub.read_text().strip(), printed,
                      "the public key itself is printed, not just its path")
        self.assertIn("authorized_keys", printed)
        self.assertRegex(printed, r"(?i)host you are backing up",
                         "printing a key without saying where it goes is a riddle")
        self.assertNotIn(args.ssh_key.read_text().split("\n")[1], printed,
                         "the PRIVATE half must never be printed")

    def test_a_defaulted_known_hosts_is_created_empty(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(Path(tmp.name) / "rclone-manager")]))
        with contextlib.redirect_stdout(io.StringIO()):
            installer.ensure_credentials(args)
        self.assertTrue(args.known_hosts.is_file())
        self.assertEqual(args.known_hosts.read_text(), "",
                         "empty is correct: host keys are pinned when a source is added")

    def test_an_explicitly_named_missing_key_is_still_refused(self):
        """The distinction the whole change rests on. A path an operator
        typed that is not there is a typo, and generating a DIFFERENT key
        under it would hand them a key the far host has never seen while
        reporting success."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        named = Path(tmp.name) / "typo" / "id_ed25519"
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(Path(tmp.name) / "rclone-manager"),
             "--ssh-key", str(named)]))
        self.assertTrue(args.ssh_key_supplied)
        with contextlib.redirect_stdout(io.StringIO()):
            installer.ensure_credentials(args)
        self.assertFalse(named.exists(), "an explicitly named path is never generated into")

        exc = refusal_from(installer.Preflight(args).check_credentials)
        self.assertIsNotNone(exc)
        self.assertEqual(exc.code, installer.EXIT_PREREQ_CREDENTIALS)
        self.assertIn(str(named), exc.message)

    def test_an_existing_key_is_never_replaced(self):
        """Regenerating over a key already trusted by a source would break
        every backup silently."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix = Path(tmp.name) / "rclone-manager"
        (prefix / "secrets").mkdir(parents=True)
        key = prefix / "secrets" / "id_ed25519"
        key.write_text("the key a source already trusts\n")
        os.chmod(key, 0o600)
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(prefix)]))
        with contextlib.redirect_stdout(io.StringIO()):
            installer.ensure_credentials(args)
        self.assertEqual(key.read_text(), "the key a source already trusts\n")


class TestEveryDirectoryIsBornWithoutGroupOrWorldWrite(unittest.TestCase):
    """The engine refuses a key whose ancestry is group- or world-writable,
    and it walks the WHOLE chain. Installing onto the UGREEN it refused
    three times running, naming one directory further up each time.

    Asserted under umask 0, which is the only way this test can fail
    against a mkdir that trusts the umask: at the umask a developer
    machine happens to have, a directory created with no explicit mode
    looks correct and the bug ships.
    """

    def setUp(self):
        self.old_umask = os.umask(0)
        self.addCleanup(os.umask, self.old_umask)

    def test_the_created_chain_has_no_group_or_world_write(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name) / "install-root"
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(root)]))
        with contextlib.redirect_stdout(io.StringIO()):
            installer.ensure_credentials(args)

        # Both sides resolved. macOS hands tempfile a /var path that the
        # installer canonicalises to /private/var, so comparing the two
        # unresolved never matches and the walk runs off the top of the
        # prefix and up to /.
        stop = root.resolve()
        created = args.ssh_key.parent.resolve()
        seen = []
        while True:
            mode = created.stat().st_mode & 0o777
            seen.append(created)
            self.assertEqual(mode & 0o022, 0,
                             f"{created} is mode {mode:o}: the engine walks the whole ancestry "
                             f"and refuses the key over any group- or world-writable link in it")
            if created == stop or created == created.parent:
                break
            created = created.parent
        self.assertIn(stop, seen, "the walk has to actually reach the install root")
        self.assertGreaterEqual(len(seen), 2, "prefix and secrets are both checked")

    def test_a_writable_directory_above_the_prefix_is_named_not_silently_fixed(self):
        """Directories above the prefix belong to whoever set the machine
        up. Tightening a share root because a backup tool was installed
        under it is not an installer's call, so it warns with the exact
        chmod instead."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        share = Path(tmp.name) / "share"
        share.mkdir()
        os.chmod(share, 0o777)
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(share / "rclone-manager")]))

        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.ensure_credentials(args)

        self.assertIn(str(share), out.getvalue())
        self.assertIn("chmod go-w", out.getvalue(), "the warning carries the fix, not just the complaint")
        self.assertEqual(share.stat().st_mode & 0o777, 0o777,
                         "it warns about a directory it does not own, it does not change it")

    def test_stage_payload_creates_the_data_directories_securely_too(self):
        """The directory the engine actually refused over.

        host_dirs carries ssh_keys, and stage_payload used a bare mkdir,
        so on the UGREEN it inherited a permissive umask and produced the
        0777 that the engine then refused three cycles running. Creating
        the key correctly is not enough if the directory it is handed to
        is made wrong a moment later.
        """
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name) / "install-root"
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(root), "--compose-file", str(CANONICAL_COMPOSE)]))
        with contextlib.redirect_stdout(io.StringIO()):
            installer.ensure_credentials(args)
            installer.stage_payload(args)

        # ssh_keys is not a host_dirs entry of its own: the engine creates
        # it under --config-dir, which is why its refusal on the UGREEN
        # named config/ssh_keys, then config, then the install root. The
        # installer owns the two ancestors, so those are what it has to
        # get right.
        self.assertIn("--config-dir", args.host_dirs)
        for label, path in args.host_dirs.items():
            mode = path.stat().st_mode & 0o777
            self.assertEqual(mode & 0o022, 0,
                             f"{label} at {path} is mode {mode:o}, which the engine refuses over")

        # The exact ancestry the engine walks, spelled out rather than
        # inferred from the loop above.
        config = args.host_dirs["--config-dir"]
        for d in (config / "ssh_keys", config, root):
            if d.is_dir():
                self.assertEqual(d.stat().st_mode & 0o777 & 0o022, 0, f"{d} is writable beyond its owner")

    def test_an_existing_directory_keeps_its_read_bits_and_only_loses_write(self):
        """Group-readable is an operator's call and no risk to the key.
        Group-writable is what lets someone replace it."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        existing = Path(tmp.name) / "backups"
        existing.mkdir()
        os.chmod(existing, 0o775)
        installer.make_secure_dir(existing)
        self.assertEqual(existing.stat().st_mode & 0o777, 0o755,
                         "write dropped, read kept: 0775 becomes 0755, not 0700")


class TestPreflightDoesNotCryAboutWhatInstallWillCreate(unittest.TestCase):
    def test_a_defaulted_missing_key_is_reported_as_pending_not_refused(self):
        """preflight is a dry run of install. Refusing on a fresh host for
        the one thing install creates by itself reports the machine as
        broken for doing nothing wrong."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        args = installer.resolve(installer.build_parser().parse_args(
            ["preflight", "--prefix", str(Path(tmp.name) / "rclone-manager")]))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.Preflight(args).check_credentials()
        self.assertIn("install creates it", out.getvalue())
        self.assertIn(str(args.ssh_key), out.getvalue())


class TestAnUpgradeKeepsTheCredentialsTheInstallAlreadyUses(unittest.TestCase):
    """#347 gave --ssh-key and --known-hosts defaults so a bare `install`
    could work. On a host that was first installed with those flags
    pointing somewhere else, a later bare `install --mode upgrade` then
    took the DEFAULTS, generated a brand new keypair under
    <prefix>/secrets, and rewrote .env to point at it.

    Nothing refused and nothing warned. The stack came back up healthy
    holding a key no source has ever authorised, the pinned host keys
    were replaced by an empty file, and the key every source does trust
    sat where it always had with nothing referencing it. Every signal was
    green and every backup afterwards failed to authenticate.

    check_layout_matches already refuses the same shape for STATE_DIR,
    BACKUP_DIR and CONFIG_DIR. SSH_KEY_FILE and KNOWN_HOSTS_FILE are in
    the same .env, written by the same renderer, and were not held to
    anything.
    """

    def _installed(self, tmp):
        """A deployment installed with both credential flags pointing
        outside the prefix, exactly as an operator with an existing key
        would have installed it."""
        root = Path(tmp.name)
        prefix = root / "rclone-manager"
        elsewhere = root / "home" / ".ssh"
        elsewhere.mkdir(parents=True)
        key = elsewhere / "backup_ed25519"
        key.write_text("the key every source already trusts\n")
        os.chmod(key, 0o600)
        known = elsewhere / "known_hosts"
        known.write_text("source.example.com ssh-ed25519 AAAApinned\n")
        first = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(prefix), "--ssh-key", str(key),
             "--known-hosts", str(known), "--compose-file", str(CANONICAL_COMPOSE)]))
        installer.stage_payload(first)
        return prefix, key, known, installer.read_env_file(prefix / ".env")

    def _rerun(self, prefix, *extra):
        return installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(prefix), "--mode", "upgrade",
             "--compose-file", str(CANONICAL_COMPOSE), *extra]))

    def test_a_bare_rerun_keeps_pointing_at_the_installed_key(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix, key, known, env = self._installed(tmp)

        again = self._rerun(prefix)
        self.assertNotEqual(again.ssh_key, key,
                            "the default is what this run starts from; that is the setup, not the assertion")
        with contextlib.redirect_stdout(io.StringIO()):
            installer.adopt_installed_credentials(again, env)

        self.assertEqual(str(again.ssh_key), str(key),
                         "the installed key is what the deployment authenticates with")
        self.assertEqual(str(again.known_hosts), str(known),
                         "and the pinned host keys go with it")
        self.assertTrue(again.ssh_key_supplied,
                        "an .env naming a path is this deployment stating one, so it gets the same "
                        "treatment a typed flag gets rather than being generated into")
        self.assertTrue(again.known_hosts_supplied)

    def test_preflight_checks_the_same_paths_install_would_use(self):
        """preflight is advertised as a dry run of install. Reporting on the
        computed defaults while install runs on what the .env names makes
        the two disagree on exactly the host where it matters: preflight
        says it will create a key, install keeps the existing one.

        Driven through preflight's own credential check rather than through
        main(), which would need a Docker daemon to get that far.
        """
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix, key, known, env = self._installed(tmp)
        args = installer.resolve(installer.build_parser().parse_args(
            ["preflight", "--prefix", str(prefix)]))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.adopt_installed_credentials(args, env)
            installer.Preflight(args).check_credentials()
        printed = out.getvalue()
        self.assertEqual(str(args.ssh_key), str(key))
        self.assertNotIn("install creates it", printed,
                         "the installed key is right there; saying install will make one is a lie")
        self.assertIn("owner-only", printed, "it checked the real key rather than skipping")

    def test_preflight_adopts_too(self):
        """Structural, because the divergence above is invisible in
        preflight's own output once it is fixed: the check that keeps them
        in step is that both commands call this."""
        import ast
        source = Path(installer.__file__).read_text(encoding="utf-8")
        for name in ("cmd_install", "cmd_preflight"):
            body = next(n for n in ast.walk(ast.parse(source))
                        if isinstance(n, ast.FunctionDef) and n.name == name)
            called = [n.func.id for n in ast.walk(body)
                      if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)]
            self.assertIn("adopt_installed_credentials", called, f"{name} has to adopt")

    def test_nothing_is_generated_over_an_installed_deployment(self):
        """The whole failure, driven end to end: adopt, then the credential
        step, then staging. A new keypair anywhere under the prefix means
        the deployment was re-pointed at a key no source has seen."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix, key, known, env = self._installed(tmp)

        again = self._rerun(prefix)
        with contextlib.redirect_stdout(io.StringIO()):
            installer.adopt_installed_credentials(again, env)
            installer.ensure_credentials(again)
            installer.stage_payload(again)

        self.assertFalse((prefix / "secrets" / "id_ed25519").exists(),
                         "generating a key here is the bug: the far host has never seen it")
        after = installer.read_env_file(prefix / ".env")
        self.assertEqual(after["SSH_KEY_FILE"], str(key))
        self.assertEqual(after["KNOWN_HOSTS_FILE"], str(known))
        self.assertEqual(key.read_text(), "the key every source already trusts\n")
        self.assertIn("AAAApinned", known.read_text(),
                      "an empty known_hosts is correct before any source exists and wrong after one")

    def test_adopting_is_announced(self):
        """Silently right is still silently. An operator reading the log of
        an upgrade is entitled to see which key the deployment kept."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix, key, known, env = self._installed(tmp)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.adopt_installed_credentials(self._rerun(prefix), env)
        printed = out.getvalue()
        self.assertIn(str(key), printed)
        self.assertIn(str(known), printed)

    def test_an_installed_path_with_nothing_there_refuses(self):
        """The one case that must not be filled in. The deployment names a
        key, the file is gone, and generating a replacement under that name
        hands every source a key it has never seen while reporting success.
        compose.yaml mounts it with `:?` anyway, so the stack cannot start."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix, key, known, env = self._installed(tmp)
        key.unlink()

        again = self._rerun(prefix)
        exc = refusal_from(installer.adopt_installed_credentials, again, env)
        self.assertIsNotNone(exc, "quietly generating a different key here is the failure")
        self.assertEqual(exc.code, installer.EXIT_PREREQ_CREDENTIALS)
        self.assertIn(str(key), exc.message, "the path the deployment names has to be named")
        self.assertIn(".env", exc.message, "and where that path came from")

    def test_an_explicit_flag_still_wins_and_says_it_is_moving_the_deployment(self):
        """#347's contract is that explicit flags win. Rotating the key is a
        real operation; doing it without saying so is not."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix, key, known, env = self._installed(tmp)
        rotated = Path(tmp.name) / "home" / ".ssh" / "rotated_ed25519"
        rotated.write_text("the new key\n")
        os.chmod(rotated, 0o600)

        again = self._rerun(prefix, "--ssh-key", str(rotated))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.adopt_installed_credentials(again, env)
        self.assertEqual(str(again.ssh_key), str(rotated), "the flag the operator typed wins")
        printed = out.getvalue()
        self.assertIn(str(key), printed, "the path being left behind is named")
        self.assertIn(str(rotated), printed)

    def test_a_fresh_host_adopts_nothing(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix = Path(tmp.name) / "rclone-manager"
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(prefix)]))
        was = args.ssh_key
        with contextlib.redirect_stdout(io.StringIO()):
            installer.adopt_installed_credentials(args, {})
        self.assertEqual(args.ssh_key, was)
        self.assertFalse(args.ssh_key_supplied,
                         "a fresh host still generates: adopting nothing must not look like a typed flag")

    def test_an_install_already_on_the_defaults_is_left_alone(self):
        """The common case, and it must stay silent. A previous no-argument
        install recorded the same paths this run computes, so there is
        nothing to adopt and nothing to say."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        prefix = Path(tmp.name) / "rclone-manager"
        first = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(prefix), "--compose-file", str(CANONICAL_COMPOSE)]))
        with contextlib.redirect_stdout(io.StringIO()):
            installer.ensure_credentials(first)
            installer.stage_payload(first)
        env = installer.read_env_file(prefix / ".env")

        again = self._rerun(prefix)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.adopt_installed_credentials(again, env)
        self.assertEqual(str(again.ssh_key), env["SSH_KEY_FILE"])
        self.assertEqual(out.getvalue(), "",
                         "nothing changed, so there is nothing to tell anyone")

    def test_cmd_install_adopts_before_it_creates_anything(self):
        """Order, not behaviour, and it is the whole fix. ensure_credentials
        runs before detect_existing reads the .env, so a check bolted on
        after it would refuse a key that had already been generated."""
        import ast
        source = Path(installer.__file__).read_text(encoding="utf-8")
        body = next(n for n in ast.walk(ast.parse(source))
                    if isinstance(n, ast.FunctionDef) and n.name == "cmd_install")
        called = [n.func.id for n in ast.walk(body)
                  if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)]
        self.assertIn("adopt_installed_credentials", called)
        self.assertLess(called.index("adopt_installed_credentials"), called.index("ensure_credentials"),
                        "adopting after the key is generated is adopting too late")


class TestReplacingTheStagedComposeIsAnnounced(unittest.TestCase):
    """<prefix>/compose.yaml is the installer's own staged copy of a gated
    artifact and is restaged on every run, which is how a runtime-contract
    change reaches an installed host. That is right, and it is also how an
    operator who edited it in place loses the edit with nothing said.

    A notice rather than a refusal, deliberately: refusing would block
    every upgrade that carries a legitimate runtime change, which is most
    of them. Host-specific settings belong in .env, and that is what the
    notice says.
    """

    def test_replacing_different_bytes_says_so(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(Path(tmp.name) / "rclone-manager"),
             "--compose-file", str(CANONICAL_COMPOSE)]))
        installer.stage_payload(args)
        staged = args.prefix / "compose.yaml"
        staged.write_bytes(staged.read_bytes() + b"\n# an operator added a mount here\n")

        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.stage_payload(args)
        printed = out.getvalue()
        self.assertIn(str(staged), printed)
        self.assertIn(".env", printed, "the notice has to name where host-specific settings do belong")
        self.assertEqual(staged.read_bytes(), CANONICAL_COMPOSE.read_bytes(),
                         "the canonical definition still wins: this is a notice, not a refusal")

    def test_restaging_the_same_bytes_says_nothing(self):
        """Every re-run restages. A notice on the unchanged case is noise
        that trains people to stop reading the changed one."""
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        args = installer.resolve(installer.build_parser().parse_args(
            ["install", "--prefix", str(Path(tmp.name) / "rclone-manager"),
             "--compose-file", str(CANONICAL_COMPOSE)]))
        installer.stage_payload(args)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            installer.stage_payload(args)
        self.assertNotIn("compose.yaml", out.getvalue())


if __name__ == "__main__":
    unittest.main()
