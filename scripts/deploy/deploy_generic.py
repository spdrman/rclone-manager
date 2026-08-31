#!/usr/bin/env python3
"""Deploys the generic backup-manager Docker app end to end (issue
#82/B4.1): validates an SSH private key and known_hosts file, renders
config.yaml and a compose .env file, wires the mounts, and starts the
container via `docker compose`.

Mirrors container/compose.yaml's own deployment shape (see
docs/deployment.md); this script does not invent a new one. The
generated config.yaml's remote.key.file always points at
CONTAINER_KEY_PATH (a fixed, read-only mount inside the container),
matching the same key resolvers issue #74 built
(core/internal/config's Key.File / Key.Env / Key.Command) - --ssh-key
selects the File resolver specifically, the documented default for a
hand-run deployment.

# Security contract (read before changing this file)

- --ssh-key is REQUIRED. It is never defaulted, never inferred from
  ~/.ssh, and never prompted for interactively - see main()'s own
  argparse setup, which has no `default=` for it.
- The key's existence, readability, and permissions are all validated
  BEFORE anything else happens (before config/.env are rendered, before
  any `docker` command runs). See validate_ssh_key(), called first in
  main().
- The key's own CONTENTS are never read by this script. It is validated
  by inspecting the filesystem entry (os.stat/os.access) only,
  mirroring core/internal/transport/rclone/ssh.go's own "the file
  resolver never puts key material into this process's own memory"
  property (see core/internal/config's Key type doc) - Docker (via a
  read-only bind mount) and rclone (inside the container) are the only
  things that ever actually open it.
- The key's contents therefore can never leak into a generated file or
  a log line, because this script never has them in memory to leak.
  What DOES appear in the generated .env file is the key's host-side
  PATH (matching container/.env.example's own documented convention:
  "Nothing in this file is a secret: it only points at where secrets
  live on the host"). The generated config.yaml never contains the
  host-side path either - only CONTAINER_KEY_PATH, the fixed in-container
  mount point.

Usage:
    python3 scripts/deploy/deploy_generic.py \\
        --ssh-key /path/to/id_ed25519 --known-hosts /path/to/known_hosts \\
        --host sftp.example.com --user backupuser --remote-path /uploads \\
        --state-dir /srv/backup-manager/state --backup-dir /srv/backup-manager/backups

Run `--help` for the full flag list, and see test_deploy_generic.py /
test_deploy_generic_integration.py for what's actually verified.
"""

from __future__ import annotations

import argparse
import os
import stat
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
COMPOSE_FILE = REPO_ROOT / "container" / "compose.yaml"

# Fixed, in-container mount points - never a host path. Matches
# container/compose.yaml's own documented volume shape exactly.
# The configuration mount is the DIRECTORY, not the file inside it
# (issue #196): the engine creates and atomically replaces config.yaml and
# keeps ssh_keys/ and known_hosts.d/ beside it.
CONTAINER_CONFIG_DIR = "/etc/backup-manager/config"
CONTAINER_CONFIG_PATH = CONTAINER_CONFIG_DIR + "/config.yaml"
CONTAINER_KEY_PATH = "/etc/backup-manager/id_ed25519"
CONTAINER_KNOWN_HOSTS_PATH = "/etc/backup-manager/known_hosts"
CONTAINER_STATE_DIR = "/data/state"
CONTAINER_BACKUP_DIR = "/data/backups"

DEFAULT_LISTEN_PORT = "8080"
DEFAULT_PUID = "1000"
DEFAULT_PGID = "1000"

# A private key readable by anyone but its owner is exactly what OpenSSH's
# own client refuses ("UNPROTECTED PRIVATE KEY FILE!"), so this script
# refuses the same way, at the door, rather than letting that surface
# later as an opaque authentication failure inside a running container.
_DISALLOWED_KEY_MODE_BITS = stat.S_IRWXG | stat.S_IRWXO


class DeploymentError(Exception):
    """Raised for any precondition that must fail loudly, with a clear
    message, before anything is started. main() is the only place this
    is caught."""


class _HelpFormatter(argparse.ArgumentDefaultsHelpFormatter, argparse.RawDescriptionHelpFormatter):
    """Combines default-value display (every flag's help text already
    shows its own default) with preserved line breaks for description/
    epilog text (RawDescriptionHelpFormatter) - the usage example in
    epilog below needs its own line breaks kept exactly as written,
    which ArgumentDefaultsHelpFormatter alone would rewrap into a single
    paragraph."""


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="deploy_generic.py",
        description="Deploy the generic backup-manager Docker app (issue #82/B4.1).",
        formatter_class=_HelpFormatter,
        epilog=(
            "example:\n"
            "  python3 scripts/deploy/deploy_generic.py \\\n"
            "      --ssh-key /path/to/id_ed25519 --known-hosts /path/to/known_hosts \\\n"
            "      --host sftp.example.com --user backupuser --remote-path /uploads \\\n"
            "      --state-dir /srv/backup-manager/state --backup-dir /srv/backup-manager/backups\n"
        ),
    )

    key_group = parser.add_argument_group("SSH credentials (required)")
    key_group.add_argument(
        "--ssh-key",
        required=True,
        help="Path to the SFTP client's private key. Required: never defaulted, "
             "never inferred from ~/.ssh, never prompted for.",
    )
    key_group.add_argument(
        "--known-hosts",
        required=True,
        help="Path to the pinned known_hosts file. Required: host-key verification "
             "is mandatory, and a deployment without one cannot connect to anything.",
    )

    remote_group = parser.add_argument_group("Remote SFTP source")
    remote_group.add_argument("--host", required=True, help="SFTP remote host.")
    remote_group.add_argument("--user", required=True, help="SFTP remote username.")
    remote_group.add_argument("--port", type=int, default=0, help="SFTP remote port (0 = backend default, 22).")
    remote_group.add_argument("--remote-path", required=True, help="Remote path this backup set pulls artifacts from.")
    remote_group.add_argument("--include", action="append", default=None,
                               help="Glob of filenames to include (repeatable). Default: '*'.")

    set_group = parser.add_argument_group("Backup set identity")
    set_group.add_argument("--source-id", default="production",
                            help="Config-level source id this backup set belongs to (config.yaml's sources[].id).")
    set_group.add_argument("--backup-set-id", default="primary",
                            help="This backup set's own id within --source-id (config.yaml's backup_sets[].id).")
    set_group.add_argument("--stale-after", default="24h",
                            help="Duration (e.g. 24h) after which a backup set with no successful cycle is reported STALE.")
    set_group.add_argument("--poll-interval", default="1h",
                            help="Duration (e.g. 1h) between scheduled run_cycle passes (config.yaml's top-level poll_interval).")

    paths_group = parser.add_argument_group("Host paths")
    paths_group.add_argument(
        "--deploy-dir",
        default=str(REPO_ROOT / "container" / ".generated"),
        help="Directory this script renders config.yaml/.env into.",
    )
    paths_group.add_argument("--state-dir", required=True, help="Host directory for the persistent SQLite journal.")
    paths_group.add_argument("--backup-dir", required=True, help="Host directory completed artifacts land in.")

    deploy_group = parser.add_argument_group("Deployment")
    deploy_group.add_argument("--listen-port", default=DEFAULT_LISTEN_PORT,
                               help="Host port web-ui's HTTP listener is published on (container/.env.example's own LISTEN_PORT).")
    deploy_group.add_argument("--puid", default=DEFAULT_PUID,
                               help="Host uid the containers run as - must already own --state-dir/--backup-dir.")
    deploy_group.add_argument("--pgid", default=DEFAULT_PGID,
                               help="Host gid the containers run as - must already own --state-dir/--backup-dir.")
    deploy_group.add_argument("--image-version", default="dev",
                               help="Build stamp baked into the image via container/Dockerfile's VERSION build arg "
                                    "(not this script's own version - deploy_generic.py has none to report).")
    deploy_group.add_argument("--image-commit", default="none",
                               help="Build stamp baked into the image via container/Dockerfile's COMMIT build arg "
                                    "(not this script's own commit - deploy_generic.py has none to report).")
    deploy_group.add_argument(
        "--project-name",
        default="backup-manager",
        help="docker compose project name. Re-running with the SAME name converges an "
             "existing deployment (unchanged services untouched, changed ones recreated) "
             "instead of creating a duplicate one - this is what makes the whole script "
             "idempotent, and is docker compose's own behavior, not reimplemented here.",
    )
    deploy_group.add_argument(
        "--no-start",
        action="store_true",
        help="Validate and render config/.env, but do not actually run `docker compose up` "
             "(a dry run; also what the unit test suite uses).",
    )

    return parser.parse_args(argv)


def validate_ssh_key(path: str) -> None:
    """Validates path exists, is a regular file, is readable by this
    process, and carries permissions a real SSH client would accept -
    all via filesystem metadata (os.stat/os.access), never by opening
    and reading the file's contents. Raises DeploymentError with a
    specific, actionable message on any failure."""
    p = Path(path)
    if not p.exists():
        raise DeploymentError(f"--ssh-key path does not exist: {path}")
    if not p.is_file():
        raise DeploymentError(f"--ssh-key path is not a regular file: {path}")
    if not os.access(p, os.R_OK):
        raise DeploymentError(f"--ssh-key path exists but is not readable by this user: {path}")

    mode = stat.S_IMODE(p.stat().st_mode)
    if mode & _DISALLOWED_KEY_MODE_BITS:
        raise DeploymentError(
            f"--ssh-key has permission {oct(mode)}, which a real SSH client would refuse "
            f"(group/other must have no access - e.g. run `chmod 600 {path}`): {path}"
        )


def validate_known_hosts(path: str) -> None:
    """Validates path exists and is readable. known_hosts is not secret
    (it names host public keys, not a private one), so there is no
    permission-strictness check here - only existence and readability,
    since host-key verification is mandatory and a missing file would
    otherwise surface as an opaque connection failure."""
    p = Path(path)
    if not p.exists():
        raise DeploymentError(f"--known-hosts path does not exist: {path}")
    if not p.is_file():
        raise DeploymentError(f"--known-hosts path is not a regular file: {path}")
    if not os.access(p, os.R_OK):
        raise DeploymentError(f"--known-hosts path exists but is not readable by this user: {path}")


def _yaml_str(value: str) -> str:
    """Encodes value as a double-quoted YAML scalar. Used for every
    operator-supplied string this script writes into config.yaml, so a
    value containing a literal '"' or backslash can never break out of
    its quoting and inject a sibling YAML key - config.yaml's schema is
    simple enough that this one helper is enough, rather than pulling in
    a YAML library for a handful of scalar fields."""
    escaped = value.replace("\\", "\\\\").replace('"', '\\"')
    return f'"{escaped}"'


def render_config_yaml(args: argparse.Namespace) -> str:
    """Renders the manager's config.yaml. remote.key.file is ALWAYS
    CONTAINER_KEY_PATH - the host-side --ssh-key value never appears
    here, only in the .env file compose reads to perform the bind mount
    (see render_env_file); this is what "maps --ssh-key onto the
    config's key.file field" means concretely (issue #74's File
    resolver, core/internal/config's Key type)."""
    includes = args.include or ["*"]
    include_lines = "\n".join(f"          - {_yaml_str(pattern)}" for pattern in includes)

    port_line = f"          port: {args.port}\n" if args.port else ""

    return (
        f"poll_interval: {args.poll_interval}\n"
        "state:\n"
        f"  database: {CONTAINER_STATE_DIR}/state.db\n"
        "sources:\n"
        f"  - id: {args.source_id}\n"
        "    backup_sets:\n"
        f"      - id: {args.backup_set_id}\n"
        "        remote:\n"
        "          type: sftp\n"
        f"          host: {_yaml_str(args.host)}\n"
        f"{port_line}"
        f"          user: {_yaml_str(args.user)}\n"
        "          key:\n"
        f"            file: {_yaml_str(CONTAINER_KEY_PATH)}\n"
        f"          known_hosts: {_yaml_str(CONTAINER_KNOWN_HOSTS_PATH)}\n"
        f"        remote_path: {_yaml_str(args.remote_path)}\n"
        f"        local_path: {CONTAINER_BACKUP_DIR}\n"
        "        include:\n"
        f"{include_lines}\n"
        "        completion:\n"
        "          strategy: rename\n"
        f"        stale_after: {args.stale_after}\n"
        "retention:\n"
        "  timezone: UTC\n"
        "  week_starts_on: monday\n"
    )


def render_env_file(args: argparse.Namespace) -> str:
    """Renders container/compose.yaml's own .env shape
    (container/.env.example). SSH_KEY_FILE/KNOWN_HOSTS_FILE are the only
    two places the key/known_hosts HOST paths appear anywhere this
    script writes - never in config.yaml (see render_config_yaml) -
    matching .env.example's own documented convention that this file
    "is not a secret: it only points at where secrets live on the
    host."""
    config_dir = str((Path(args.deploy_dir) / "config").resolve())
    lines = [
        f"PUID={args.puid}",
        f"PGID={args.pgid}",
        f"STATE_DIR={args.state_dir}",
        f"BACKUP_DIR={args.backup_dir}",
        f"CONFIG_DIR={config_dir}",
        f"SSH_KEY_FILE={args.ssh_key}",
        f"KNOWN_HOSTS_FILE={args.known_hosts}",
        f"LISTEN_PORT={args.listen_port}",
        f"VERSION={args.image_version}",
        f"COMMIT={args.image_commit}",
    ]
    return "\n".join(lines) + "\n"


def _write_if_changed(path: Path, content: str) -> None:
    """Writes content to path only if it differs from what's already
    there (or the file doesn't exist yet) - part of what makes
    re-running this script converge rather than needlessly touching
    mtimes/triggering an unnecessary `docker compose` recreate."""
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists() and path.read_text() == content:
        return
    path.write_text(content)


def _run_docker_compose(args: argparse.Namespace, env_file: Path) -> None:
    cmd = [
        "docker", "compose",
        "-p", args.project_name,
        "-f", str(COMPOSE_FILE),
        "--env-file", str(env_file),
        "up", "-d",
    ]
    result = subprocess.run(cmd, cwd=str(REPO_ROOT), capture_output=True, text=True)
    if result.returncode != 0:
        raise DeploymentError(
            "docker compose up failed:\n"
            f"stdout: {result.stdout}\n"
            f"stderr: {result.stderr}"
        )


def main(argv: list[str]) -> int:
    try:
        args = parse_args(argv)

        # Validate BEFORE anything else: no directory is created, no file
        # is rendered, no docker command runs until both pass.
        validate_ssh_key(args.ssh_key)
        validate_known_hosts(args.known_hosts)

        deploy_dir = Path(args.deploy_dir)
        config_path = deploy_dir / "config" / "config.yaml"
        env_path = deploy_dir / ".env"

        _write_if_changed(config_path, render_config_yaml(args))
        _write_if_changed(env_path, render_env_file(args))

        if args.no_start:
            print(f"Rendered {config_path} and {env_path} (--no-start: not starting the container).")
            return 0

        _run_docker_compose(args, env_path)
        print(
            f"Deployed project {args.project_name!r} from {COMPOSE_FILE} "
            f"(config: {config_path}). Re-run this same command to converge "
            "an existing deployment instead of duplicating it."
        )
        return 0
    except DeploymentError as exc:
        print(f"deploy_generic: {exc}", file=sys.stderr)
        return 1
    except SystemExit:
        # argparse's own --help / required-argument handling already
        # printed its own message and picked its own exit code (2); let
        # it through unchanged rather than swallowing it into a generic 1.
        raise


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
