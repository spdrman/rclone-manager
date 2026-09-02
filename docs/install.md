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

Six subcommands: `preflight` checks and creates nothing, `install` checks then
installs, `status` reports, `uninstall` removes what the installer made,
`network-doctor` diagnoses (and, asked to, repairs) Docker bridge networking, and
`network-undo` removes exactly what a repair added. See
[Known-good, and known-bad](#known-good-and-known-bad) below for what the last two
are for.

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

### Persistence

`--fix-network=auto`, the default, inserts runtime rules and they are **lost on reboot**,
and lost again whenever the host firewall rewrites its own set. The containers come back
either way, so the deployment stops working silently and nothing re-runs the installer.

`--fix-network=persist` fixes that. It does everything `auto` does and additionally
installs a systemd unit and timer that re-assert the same four rules.

`auto` is the default and `persist` has to be asked for, because installing a systemd
unit is a larger commitment than inserting a runtime rule and an operator should get to
choose. There is deliberately no second flag: persistence is a value of `--fix-network`,
not a knob that can disagree with it.

#### Why not `netfilter-persistent save`

The mechanism is right there. On the target host `netfilter-persistent.service` is
installed, enabled and active, and `/etc/iptables/rules.v4` exists.

It is also zero bytes, and that is the whole argument in one fact. `netfilter-persistent
save` snapshots the **entire live ruleset**, so the first save takes `UG_INPUT`,
`UG_FORWARD`, `UG_SSH_INPUT` and everything else UGOS owns and writes them into a file
this project would then restore at every boot. That is taking ownership of a copy of
somebody else's firewall. It fights the Control Panel the moment anything is changed
there, it silently restores stale UGOS rules after a UGOS update, and it breaks the one
property the rules above are careful to keep: never own, reorder or replace a ruleset
this installer did not create.

A test asserts no generated script ever invokes it, or `iptables-save`.

#### What is installed instead

```
/etc/systemd/system/rclone-manager-bridge.service
/etc/systemd/system/rclone-manager-bridge.timer
```

The service owns exactly the four tagged rules and nothing else. Each is one `ExecStart`
running the same `iptables -C … || iptables -I …` the interactive path uses, so it is
idempotent, and one line per rule so systemd names the one that failed. `systemctl cat`
shows every command that will ever run.

It is ordered `After=net_serv.service netfilter-persistent.service nftables.service
docker.service`, so it runs on top of whatever those build rather than underneath it.
`After=` only, never `Requires=`: a host without one of them should still get its rules
rather than a failed unit.

**oneshot plus timer**, and the alternatives were weighed rather than skipped:

- `Restart=` needs a process to restart, and there is nothing here that stays alive: the
  work is four checks that take milliseconds. A `Type=oneshot` unit with `Restart=always`
  and a `RestartSec` is a timer built out of the wrong primitive, and it reports as
  perpetually activating rather than as a scheduled job anyone can read.
- A `.path` unit watches the filesystem, and netfilter rules are not a file. There is no
  path whose modification corresponds to the host rewriting the live ruleset, and
  `/etc/iptables/rules.v4` is zero bytes and never written here, so watching it would
  watch nothing happen.
- oneshot plus timer is legible afterwards: `systemctl cat` for what runs,
  `systemctl list-timers` for when it last did and next will. For something that edits a
  firewall unattended, that matters more than being clever.

**The interval is two minutes**, chosen rather than inherited. The work is four
`iptables -C` calls, single-digit milliseconds, so cost is not what sets it. The failure
mode is: the host rewriting its ruleset is human-triggered, plausibly whenever the
Control Panel is touched, so the question is how long someone may be left with a
deployment that has quietly stopped working. Two minutes is well inside the time it takes
anyone to notice, and a small fraction of the engine's own default one-hour poll interval,
so a gap cannot swallow a backup cycle.

The service is also enabled in its own right, so at boot the rules are in place as part
of the boot sequence rather than up to a timer interval later; the timer's `OnBootSec` is
the safety net for a host where that ordering turns out not to be enough.

#### Quiet when there is nothing to do

`iptables -C` prints nothing when the rule is already there, and `LogLevelMax=warning`
keeps systemd's own start and finish lines out of the journal too. Measured on the target
host: `journalctl -u rclone-manager-bridge.service --since -12min` is empty across
several fires.

#### What it still does not guarantee

The host can rewrite its ruleset between fires, and the hop is down until the next one.
Two minutes is a bound, not a promise. The durable fix is in the host firewall's own
configuration, which is the administrator's to make: this installer does not write into
it, because an application installer editing a NAS appliance's firewall config behind its
owner's back is a worse failure than the one it is fixing.

**A lead worth following.** `netevent.service`, described as "ugos netlink_route event
listener", is in `failed` state on this host. That is UGOS's own network-event watcher,
and a plausible reason its firewall never reacted to `docker0` appearing in the first
place. Nothing here touches it, and anyone chasing the root cause of this whole class of
problem should start there.

#### Removing it

```
python3 install_docker_host.py network-undo
```

Stops and disables the timer and the service, deletes both unit files, reloads systemd,
and deletes the four tagged rules, in that order. Units first, because the other order
leaves a window in which the timer fires and puts back rules that were just deleted.
Every step tolerates the thing already being gone, so undo works on a half-installed
machine too.

`status` reports whether persistence is in place, unprivileged:

```
bridge networking: ok (gateway yes, egress yes)
  persistence: rclone-manager-bridge.timer enabled, next fire Mon 2026-08-31 22:26:26
```

### Verified, not assumed

After remediating, the installer re-runs the same probes and refuses with exit 44 if
they still fail. This is the same discipline that turned "the Web UI answered 200" into
"the Web UI reached the engine".
