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
| 40 | sudo has no terminal to prompt on |
| 41 | the sudo password was not accepted |
| 42 | this account may not run that as root |
| 43 | bridge networking is broken and was not repaired (`--fix-network=never`, or `diagnose`) |
| 44 | the correction was applied and a bridged container still cannot originate traffic |
| 45 | bridge networking is broken and the responsible rule could not be identified |

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

**A host prerequisite the installer now repairs.** That same NAS could not pass
container-originated TCP. Containers received connections from the host perfectly and
could not open one to a peer on their own bridge, to their own bridge gateway, or to
the internet; a container in the host network namespace had full connectivity, and so
did the host.

It matters twice over. The Web UI reaches the engine over exactly that hop, so the page
loaded and every API call hung. And the engine reaches an SFTP source over the same hop,
so no backup could run at all. So the installer diagnoses it and fixes it (issue #271).

### How it decides what is wrong

By measurement, never by reading the ruleset and reasoning about it. Two earlier
attempts to diagnose this same host by inference got it wrong in two different
directions: one concluded `iptables` and `nft` were missing when they were at `/sbin`
and the probe's PATH was not, and one blamed forwarding and NAT when half the fault was
in `INPUT`. So:

1. Ask what a bridged container can actually do. No root needed, so an operator who
   declines the escalation still gets an answer. Two separate questions, because they
   traverse different chains: reaching the bridge gateway is `INPUT` (delivered locally
   on the bridge, never forwarded, never NAT'd) and reaching an external endpoint is
   `FORWARD` plus NAT.
2. If either fails, read every DROP rule's counter, generate exactly that traffic, and
   read them again. **The rule whose counter moved by the number of packets sent is the
   rule doing it**, and that is what gets reported. A remediation that cannot name the
   rule it corrects is a guess.

Every privileged tool is invoked by absolute path off an explicit privileged PATH.
Nothing concludes "absent" from a bare name lookup.

### What it does about it

Least invasive first, and the measurement chooses. If Docker's own chains are **missing**,
the host firewall probably started after `dockerd` and flushed them, and `systemctl
restart docker` makes Docker reinstall them. That is one reversible command, and a
positive result says this is a boot-ordering problem rather than a rule problem. When
the chains are all present, a restart reinstalls what is already there, so it is skipped
rather than tried out of ritual.

Otherwise, four scoped rules:

```
iptables -I DOCKER-USER 1 -i docker0 -m comment --comment rclone-manager-bridge -j RETURN
iptables -I DOCKER-USER 1 -i br-+    -m comment --comment rclone-manager-bridge -j RETURN
iptables -I INPUT       1 -i docker0 -m comment --comment rclone-manager-bridge -j ACCEPT
iptables -I INPUT       1 -i br-+    -m comment --comment rclone-manager-bridge -j ACCEPT
```

`RETURN` in `DOCKER-USER`, not `ACCEPT`, and the difference matters. An `ACCEPT` there
ends the `FORWARD` traversal and takes Docker's inter-network isolation with it, which
is the exact property this deployment rests on: the engine is reachable only from the
Web UI because they share a network nothing else joins. `RETURN` skips whatever the host
firewall jumped to from inside `DOCKER-USER` and hands the decision back to Docker's own
chains, isolation included.

`br-+` is iptables' wildcard for Docker's user-defined bridges, which are always
`br-<12 hex>`. It keeps the hyphen so it cannot match a host bridge called `br0`.

### The safety rules it holds itself to

This edits a firewall on a machine reachable only over SSH.

- Inserts only. No `-F`, no `-P`, no `iptables-restore`, no `-X`, no `-Z`. There is a
  test asserting each of those strings is absent from every generated script.
- Every rule is scoped to a Docker bridge interface. Never a blanket ACCEPT.
- Idempotent by construction: each line is `iptables -C … || iptables -I …`.
- Reversible: every rule carries the `rclone-manager-bridge` comment, and
  `network-undo` removes exactly those and nothing else.
- The host's own rules are never touched, replaced or reordered.
- A healthy host is a no-op and is never asked for a password.

### Sudo

One escalation, announced before it happens, with the exact commands printed first. The
password is read by `sudo` from the terminal; this installer never sees it, never stores
it, never puts it in an environment variable, never passes it on a command line and
never writes it anywhere. Three distinct failures with three exit codes: no terminal
(40), wrong password (41), not permitted (42).

Worth stating plainly: on a host where this account is in the `docker` group, it can
already obtain root by running a privileged container, so the password is not what
stands between this installer and the firewall. What it buys is that the escalation is
explicit, announced and auditable rather than arriving quietly through the container
runtime.

### Persistence, which is a decision rather than an oversight

**These rules do not survive a reboot.** They are raw `iptables` inserts, and a host
firewall that rewrites its own ruleset on boot will drop them, as will any later rewrite.
After a reboot the containers come back and the hop is broken again, silently, because
nothing re-runs the installer.

The installer says so at the end of every run that changes anything, and `status`
re-checks the hop with no root and no password so the loss is discoverable:

```
bridge networking: ok (gateway yes, egress yes)
```

The durable fix belongs in the host firewall's own configuration, which is the host
administrator's to make. The installer does not write into it, because a NAS appliance's
firewall configuration is not something an application installer should be editing
behind its owner's back.

### Verified, not assumed

After remediating, the installer re-runs the same probes and refuses with exit 44 if
they still fail. This is the same discipline that turned "the Web UI answered 200" into
"the Web UI reached the engine".
