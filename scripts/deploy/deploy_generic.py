#!/usr/bin/env python3
"""Not yet implemented; see test_deploy_generic.py for the intended
contract (issue #82/B4.1). This stub exists only so that module compiles
and argparse's own required-flag behavior is exercised, while the rest
of the contract is pinned down by failing tests first.
"""

from __future__ import annotations

import argparse
import sys

CONTAINER_KEY_PATH = "/etc/backup-manager/id_ed25519"
CONTAINER_KNOWN_HOSTS_PATH = "/etc/backup-manager/known_hosts"


def parse_args(argv):
    parser = argparse.ArgumentParser(prog="deploy_generic.py")
    parser.add_argument("--ssh-key", required=True)
    parser.add_argument("--known-hosts", required=True)
    parser.add_argument("--host", required=True)
    parser.add_argument("--user", required=True)
    parser.add_argument("--remote-path", required=True)
    parser.add_argument("--state-dir", required=True)
    parser.add_argument("--backup-dir", required=True)
    parser.add_argument("--deploy-dir", default="./deploy")
    parser.add_argument("--no-start", action="store_true")
    return parser.parse_args(argv)


def render_config_yaml(args) -> str:
    raise NotImplementedError


def render_env_file(args) -> str:
    raise NotImplementedError


def main(argv) -> int:
    parse_args(argv)
    raise NotImplementedError


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
