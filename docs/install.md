# Installing rclone-manager on a Docker host

Issue #262. `scripts/install/install_docker_host.py` brings the engine and the Web UI
up on a machine you have SSH on, or refuses and tells you exactly which prerequisite
stopped it.

```
python3 scripts/install/install_docker_host.py install \
    --prefix /volume1/backup-manager \
    --ssh-key /volume1/backup-manager/secrets/id_ed25519 \
    --known-hosts /volume1/backup-manager/secrets/known_hosts \
    --image ghcr.io/spdrman/backup-manager:0.1.0
```

Four subcommands: `preflight` checks and creates nothing, `install` checks then
installs, `status` reports, `uninstall` removes what the installer made.

## What it assumes about the machine, and what it does not

It assumes Python 3.8 or newer, Docker, Compose v2 or newer, and an account that can
reach the Docker socket. It assumes nothing else. In particular it does **not** assume
root, and it does not assume `sudo` works: the machine it was proven on is a UGREEN NAS
whose SSH account is uid 1001, has no passwordless sudo, and is in the `docker` group
and nothing more.

That single fact decides most of the design. The installer cannot `chown`, cannot
install a package and cannot bind a privileged port, so a directory owned by somebody
else is a refusal rather than a repair, and it says so in as many words rather than
calling `sudo` and hoping.

Standard library only, deliberately. A NAS appliance may not let you install anything,
and an installer with its own dependencies is an installer that cannot run on the
machines that need it most.

## What it refuses, and what each refusal means

Every one has its own exit code, so a wrapper can branch on the reason instead of
parsing prose.

| exit | refusal |
|---|---|
| 10 | Python is older than 3.8 |
| 11 | this architecture has no released image |
| 12 | Docker is absent, or the daemon is not reachable by this account |
| 13 | `docker compose` is absent, or is v1 |
| 14 | a host directory is missing, is not a directory, or is owned by another uid |
| 15 | the listen port is held by something that is not this project |
| 16 | too little free space on the backup volume |
| 17 | the SSH key or `known_hosts` is missing, is not a file, or the key is readable beyond its owner |
| 18 | the image is neither present, nor loadable from an archive, nor pullable |
| 19 | `container/compose.yaml` is not where the installer was told to find it |
| 20 | an install is already here and `--if-installed=refuse` was given |
| 30 | a Docker command failed |
| 31 | the stack started but did not reach the state that counts as installed |

Compose v1 is refused rather than tolerated because the deployment gates the Web UI on
`depends_on: condition: service_healthy`, and v1 ignores that. It would appear to work
and would start the UI before the engine was listening.

## It derives from the canonical definition, it does not restate it

`container/compose.yaml` is the canonical runtime contract (issue #167), and
`distribution/compose` fails the build when a derived artifact stops matching it. The
installer copies that file byte for byte and lays one override beside it carrying two
keys per service: `image`, and `pull_policy: never`.

`pull_policy` is there because the canonical file has a `build:` block, which is right
for a file written to be built from a checkout and wrong for a host that has no
checkout. `docker compose up` is also run with `--no-build`. Both were needed: with only
the policy, the first real install still tried to build and failed on a context
directory that was not there, with the image already loaded and sitting on the host.

Everything else that varies per host is in the `.env`, which is what
`container/.env.example` documents it for.

## What "installed" means

Not "the container started". Three conditions, and the third exists because a real
install taught me it was a separate claim:

1. Docker reports the engine healthy **by its own liveness probe**. Not
   `backup-manager status`, which is a backup freshness verdict a fresh install
   legitimately fails; gating on that means the Web UI never starts, which is issue
   #206.
2. The Web UI serves its bundle. A fresh install with no config serves a first-run
   setup flow rather than refusing to start, which is issue #176.
3. A request through the Web UI reaches the engine. `/health/ready` answering 503
   `not_ready` is a **pass**: that is the correct answer for an unconfigured instance.
   A request that never completes is not.

## Credentials

The SSH private key is never read, never copied, never generated and never printed.
Only its host-side path reaches the `.env`, which is the convention
`container/.env.example` states in its own first paragraph and the rule
`scripts/deploy/deploy_generic.py` already holds itself to. The installer validates the
key by its filesystem entry alone, so its contents are never in this process's memory
to leak.

A key readable beyond its owner is refused, the same way OpenSSH's own client refuses
one, rather than letting it surface later as an opaque authentication failure inside a
container.

## Pointing it at a real SFTP source

Everything about a source is configuration. Host, port, user, remote path and the key
are inputs; none of them is compiled in and none of them belongs in this repository.

A source on a non-default SSH port is the case worth spelling out, because it is
common and because the port is often deliberately unpublished. Treat it exactly like
the key: supply it, do not commit it. The Ansible configuration that manages the hosts
this product was built for already does this, reading the port from an `SSH_PORT`
environment variable with a default of 22 so the real value never enters the
repository, and this project follows the same convention rather than inventing another.

Two things a non-default port changes:

- The `known_hosts` entry is keyed `[host]:port`, not `host`. An entry pinned without
  the port will not match.
- `POST /api/v1/ssh/host-key-probe` takes the port and opens a real connection, so it
  is the honest way to get the pinned line rather than typing one.

## Known-good, and known-bad

**Proven on**: UGREEN NAS, `x86_64`, `Linux 6.12.30+`, Docker 29.4.3, Compose v5.1.3,
Python 3.11.2, SSH account uid 1001 in the `docker` group with no passwordless sudo.
Install, engine liveness, Web UI, restart survival, converge-on-rerun and uninstall all
behave as documented there.

**A host prerequisite this installer cannot fix.** That same NAS cannot pass
container-originated TCP. Containers receive connections from the host perfectly, and
cannot open one to a peer on their own bridge, to their own bridge gateway, or to the
internet; a container in the host network namespace has full connectivity, and so does
the host. That is a Docker bridge networking fault on the machine, not a property of
this deployment, and it affects every bridged container on it.

It matters twice over. The Web UI reaches the engine over exactly that hop, so the page
loads and every API call hangs. And the engine reaches an SFTP source over the same
hop, so no backup can run at all. The installer now refuses with exit 31 rather than
reporting success, and names the one-line probe that shows the fault:

```
docker run --rm --network <project>_internal alpine wget -T5 -O- http://rclone-manager:8080/health/live
```

If that hangs, fix the host before going further. Nothing in this deployment can work
around it.
