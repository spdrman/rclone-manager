"""Unit tests for deploy_generic.py's flag validation and config
rendering (issue #82/B4.1's "Additional TDD items" for the scripted CLI
deployment).

Run with:
    python3 -m unittest scripts.deploy.test_deploy_generic -v
or, from this directory:
    python3 -m unittest test_deploy_generic -v

These tests never touch Docker or the network - see
test_deploy_generic_integration.py for the real-SFTP end-to-end suite,
which does and is skipped when Docker is unavailable.
"""

from __future__ import annotations

import contextlib
import io
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import deploy_generic  # noqa: E402


def _base_args(ssh_key: str, known_hosts: str, **overrides: str) -> list[str]:
    args = [
        "--ssh-key", ssh_key,
        "--known-hosts", known_hosts,
        "--host", "sftp.example.com",
        "--user", "backupuser",
        "--remote-path", "/uploads",
        "--state-dir", "/tmp/state",
        "--backup-dir", "/tmp/backups",
        "--no-start",
    ]
    for key, value in overrides.items():
        args.extend(["--" + key.replace("_", "-"), value])
    return args


def _write_key(path: Path, mode: int = 0o600, content: str = "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n") -> None:
    path.write_text(content)
    os.chmod(path, mode)


def _run_main(argv: list[str]) -> tuple[int, str, str]:
    """Runs deploy_generic.main(argv), capturing its exit code and both
    output streams, without ever letting a real SystemExit escape (main()
    is expected to RETURN an int; argparse's own --help/parse-error path
    raises SystemExit, which this converts to that same int)."""
    stdout, stderr = io.StringIO(), io.StringIO()
    code = 0
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        try:
            code = deploy_generic.main(argv)
        except SystemExit as exc:
            code = exc.code if isinstance(exc.code, int) else 1
    return code, stdout.getvalue(), stderr.getvalue()


class RequiredFlagsTests(unittest.TestCase):
    """RED item: 'a test asserting the script exits non-zero, with a
    message naming the flag, when --ssh-key is absent.'"""

    def test_missing_ssh_key_exits_nonzero_and_names_the_flag(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("sftp.example.com ssh-ed25519 AAAA...\n")

            argv = [
                "--known-hosts", str(known_hosts),
                "--host", "sftp.example.com",
                "--user", "backupuser",
                "--remote-path", "/uploads",
                "--state-dir", "/tmp/state",
                "--backup-dir", "/tmp/backups",
                "--no-start",
            ]
            code, _out, err = _run_main(argv)

        self.assertNotEqual(code, 0, "missing --ssh-key must exit non-zero")
        self.assertIn("--ssh-key", err, "the error must name the missing flag")

    def test_missing_known_hosts_exits_nonzero_and_names_the_flag(self):
        with tempfile.TemporaryDirectory() as tmp:
            key = Path(tmp) / "id_ed25519"
            _write_key(key)

            argv = [
                "--ssh-key", str(key),
                "--host", "sftp.example.com",
                "--user", "backupuser",
                "--remote-path", "/uploads",
                "--state-dir", "/tmp/state",
                "--backup-dir", "/tmp/backups",
                "--no-start",
            ]
            code, _out, err = _run_main(argv)

        self.assertNotEqual(code, 0)
        self.assertIn("--known-hosts", err)


class SSHKeyValidationTests(unittest.TestCase):
    """RED items: 'a test asserting it refuses a path that does not
    exist, and one that exists but is not readable' - plus the
    permission-strictness requirement from the issue body ("refuse a key
    with permissions the SSH client would reject")."""

    def test_refuses_a_path_that_does_not_exist(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            missing = Path(tmp) / "does-not-exist"

            code, _out, err = _run_main(_base_args(str(missing), str(known_hosts)))

        self.assertNotEqual(code, 0)
        self.assertIn(str(missing), err)

    @unittest.skipIf(os.geteuid() == 0, "root bypasses POSIX read permission checks")
    def test_refuses_a_key_that_exists_but_is_not_readable(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            _write_key(key, mode=0o600)
            os.chmod(key, 0o000)

            code, _out, err = _run_main(_base_args(str(key), str(known_hosts)))

        self.assertNotEqual(code, 0)
        self.assertIn(str(key), err)

    def test_refuses_a_world_readable_key(self):
        """A real SSH client refuses a private key readable by group or
        other; this script must refuse it at the door too, before ever
        starting a container that would only fail to authenticate later
        with an opaque error."""
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            _write_key(key, mode=0o644)

            code, _out, err = _run_main(_base_args(str(key), str(known_hosts)))

        self.assertNotEqual(code, 0)
        self.assertIn("permission", err.lower())

    def test_accepts_a_key_with_owner_only_permissions(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            _write_key(key, mode=0o600)

            code, _out, err = _run_main(_base_args(str(key), str(known_hosts)))

        self.assertEqual(code, 0, err)

    def test_refuses_a_known_hosts_path_that_does_not_exist(self):
        with tempfile.TemporaryDirectory() as tmp:
            key = Path(tmp) / "id_ed25519"
            _write_key(key)
            missing = Path(tmp) / "no-such-known-hosts"

            code, _out, err = _run_main(_base_args(str(key), str(missing)))

        self.assertNotEqual(code, 0)
        self.assertIn(str(missing), err)

    def test_validation_happens_before_any_docker_call(self):
        """'Validate the key exists and is readable BEFORE starting
        anything' - proven here by making a bogus --ssh-key fail even
        though the deploy directory (and everything else needed to reach
        the docker step) is otherwise fully valid; if validation ran
        after rendering/starting, this would instead fail with a Docker
        or file-system error, not the SSH key error."""
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            missing = Path(tmp) / "does-not-exist"

            code, _out, err = _run_main(_base_args(str(missing), str(known_hosts)))

        self.assertNotEqual(code, 0)
        self.assertNotIn("docker", err.lower())


class ConfigRenderingTests(unittest.TestCase):
    """The --ssh-key value must map onto the config's key.file field
    (#74's resolvers), and the key's own contents must never appear in
    generated config or in any log line."""

    def test_ssh_key_maps_onto_the_fixed_container_key_file_path(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            secret_marker = "TOTALLY-SECRET-KEY-MATERIAL-abc123"
            _write_key(key, content=secret_marker)

            args = deploy_generic.parse_args(_base_args(str(key), str(known_hosts)))
            config_yaml = deploy_generic.render_config_yaml(args)

        self.assertIn(deploy_generic.CONTAINER_KEY_PATH, config_yaml)
        self.assertNotIn(str(key), config_yaml, "the host-side key PATH must not appear in config.yaml")
        self.assertNotIn(secret_marker, config_yaml, "the key's CONTENTS must never appear in config.yaml")

    def test_config_yaml_never_contains_key_contents_even_via_main_dry_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            secret_marker = "TOTALLY-SECRET-KEY-MATERIAL-xyz789"
            _write_key(key, content=secret_marker)
            deploy_dir = Path(tmp) / "deploy"

            code, out, err = _run_main(_base_args(str(key), str(known_hosts), deploy_dir=str(deploy_dir)))
            self.assertEqual(code, 0, err)

            self.assertNotIn(secret_marker, out)
            self.assertNotIn(secret_marker, err)
            for rendered in deploy_dir.rglob("*"):
                if rendered.is_file():
                    self.assertNotIn(secret_marker, rendered.read_text(errors="ignore"),
                                      f"{rendered} must never contain the key's contents")

    def test_env_file_points_at_the_hosts_ssh_key_path_read_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            _write_key(key)

            args = deploy_generic.parse_args(_base_args(str(key), str(known_hosts)))
            env_file = deploy_generic.render_env_file(args)

        # The .env file's whole job (matching container/.env.example) is to
        # point compose at where the key lives on the HOST - unlike
        # config.yaml, this is expected and matches existing convention.
        self.assertIn(str(key), env_file)

    def test_rendering_is_deterministic(self):
        with tempfile.TemporaryDirectory() as tmp:
            known_hosts = Path(tmp) / "known_hosts"
            known_hosts.write_text("host key\n")
            key = Path(tmp) / "id_ed25519"
            _write_key(key)

            args = deploy_generic.parse_args(_base_args(str(key), str(known_hosts)))
            first = deploy_generic.render_config_yaml(args)
            second = deploy_generic.render_config_yaml(args)

        self.assertEqual(first, second)


if __name__ == "__main__":
    unittest.main()
