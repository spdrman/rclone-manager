# Installing rclone-manager on a Docker host

Issue #262. `scripts/install/install_docker_host.py` brings the engine and the Web UI
up on a machine you have SSH on, or refuses and tells you exactly which prerequisite
stopped it.

```
python3 scripts/install/install_docker_host.py install
```

That is the whole command on a bare host (issue #347). It installs under
`~/rclone-manager`, generates an SSH keypair and an empty `known_hosts` under
`<prefix>/secrets` if they are not there, and prints the public half with a note that
it belongs in the `authorized_keys` of whichever host you are backing up.

Every flag is still there when you want it, and naming one changes only that one:

```
python3 scripts/install/install_docker_host.py install \
    --prefix /volume1/backup-manager \
    --ssh-key /volume1/backup-manager/secrets/id_ed25519 \
    --known-hosts /volume1/backup-manager/secrets/known_hosts \
    --image ghcr.io/spdrman/backup-manager:0.3.0
```

**One file, and no checkout.** Copy
`scripts/install/install_docker_host.py` to the machine on its own and run it: it needs
no repository beside it, nothing else from this project on disk, and nothing outside the
Python standard library. It used to refuse with exit 19 here, because it copied
`container/compose.yaml` and that file only exists inside a git checkout, which is the
one thing an operator installing onto a NAS does not have. It now carries that
definition itself (see [It derives from the canonical
definition](#it-derives-from-the-canonical-definition-it-does-not-restate-it) below for
what keeps the copy honest).

### Compatibility: `--prefix` no longer defaults to `/volume1/backup-manager`

It defaults to `~/rclone-manager`. If you have a script that relied on the old default
being applied for you, pass `--prefix /volume1/backup-manager` explicitly. The old
default was a guess at one NAS vendor's share layout that was wrong by a directory name
on the actual UGREEN this was proven on, and wrong entirely on anything not
Synology-shaped, so it never once saved anybody a flag.

### What is generated, and what is still a refusal

A **defaulted** credential path that does not exist is created. An **explicitly named**
one that does not exist is still a refusal, and deliberately: generating a different key
under a path you typed would hand you one the far host has never seen, while reporting
success. An existing key is never regenerated over, whatever its age, because replacing
one silently breaks every source already trusting it.

The private half is still never read and never printed. Only the public half is.

Directories the installer creates are born `0700`. Directories that already exist only
lose group and world **write**, so read bits you set on purpose survive. That is not
cosmetic: the engine refuses to use an SSH key if any directory in its whole ancestry is
group- or world-writable, since anyone holding that bit can replace the key whatever the
key file's own mode says. Ancestors *above* `--prefix` belong to whoever set the machine
up, so those are named in a warning with the exact `chmod go-w` rather than changed.

Six subcommands: `preflight` checks and creates nothing, `install` checks then
installs, `status` reports, `uninstall` removes what the installer made,
`network-doctor` diagnoses (and, asked to, repairs) Docker bridge networking, and
`network-undo` removes exactly what a repair added. See
[Known-good, and known-bad](#known-good-and-known-bad) below for what the last two
are for.

Flags are scoped to the subcommand that reads them, so `<subcommand> --help` lists only
what that subcommand actually uses. A flag valid on one is not necessarily valid on
another: `--ssh-key`, `--image` and the rest of the install prerequisites exist on
`preflight` and `install` alone, since nothing else ever reads a credential path or an
image reference. If you have a script from before this scoping, the one rename to know
about is on `status`, which used to take `--fix-network` to decide whether to run its
read-only bridge check and now takes its own `--check-network` instead. Anything else
that moved fails loudly at parse time with "unrecognized arguments" rather than being
quietly ignored.

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
| 17 | an explicitly named SSH key or `known_hosts` is missing, or the installed `.env` names one that is no longer there, either is not a regular file, or the key is readable beyond its owner |
| 18 | the image is neither present, nor loadable from an archive, nor pullable |
| 19 | `--compose-file` names a path that is not there, or this installer's own embedded runtime definition does not match the digest recorded beside it |
| 20 | an install is already here and the mode did not settle what to do about it, a factory reset was not confirmed, or this run's directories are not the installed ones |
| 21 | the install here is newer than the version this installer carries |
| 30 | a Docker command failed |
| 31 | the stack started but did not reach the state that counts as installed |
| 40 | sudo has no terminal to prompt on |
| 41 | the sudo password was not accepted |
| 42 | this account may not run that as root |
| 43 | bridge networking is broken and was not repaired (`--fix-network=never`, or `diagnose`) |
| 44 | the correction was applied and a bridged container still cannot originate traffic |
| 45 | bridge networking is broken and the responsible rule could not be identified |
| 50 | `--release` and `--image` name different versions, or `--image` already pins a digest |
| 51 | `--release` was given together with `--no-pull` or `--image-archive` |
| 52 | the tag being installed no longer points at the digest this release recorded |

Compose v1 is refused rather than tolerated because the deployment gates the Web UI on
`depends_on: condition: service_healthy`, and v1 ignores that. It would appear to work
and would start the UI before the engine was listening.

## Naming a previous release, and proving the one it carries

`--release X.Y.Z` installs a published release other than the one this installer
carries:

```
python3 install_docker_host.py install --release 0.2.0
```

It fills the tag in `--image` and nothing else. It is not `--version`: that name
belongs to "print your own version", and this program's identity is the release
it carries.

When `--image` already names a version, the two have to agree. If they do not,
this refuses with exit 50 rather than pick a winner, because installing a version
other than the one you typed, quietly, is the failure the flag exists to prevent.
An `--image` that already pins an `@sha256:` digest is refused the same way: a
digest names exactly one image and is the stronger claim of the two, so `--release`
will not weaken it and will not decorate it with a tag that may describe something
else. An `--image` with no tag at all is the one case it fills in.

`--no-pull` and `--image-archive` are the offline paths and resolve nothing against
a registry, so `--release` alongside either of them is exit 51. Pick the release on
a machine that can reach the registry, `docker save` it, and bring the tarball over.

`latest` is refused, and so is anything else that is not an orderable version.
A host installed from a moving tag records `VERSION=latest` in its `.env`, which
orders against nothing, so no later installer can tell whether it is moving that
host forwards or backwards. There is no `latest` tag to install anyway.

### The digest, and why the default does not float

Preflight prints the reference it is about to install before anything is created,
and then proves it:

```
  ok   installing ghcr.io/spdrman/backup-manager:0.3.0
  ok   ghcr.io/spdrman/backup-manager:0.3.0 is sha256:..., the identity the release
       manifest records for 0.3.0
```

A registry tag is a mutable pointer, which `scripts/release/publish-image.sh` says
in its own words, so "install this version" is a claim about a name until something
compares the name to a recorded identity. `container/release-manifest.json` records
that identity at push time, the installer carries a copy of it, and one anonymous
HEAD against the registry settles it. If the tag has moved, this refuses with exit
52 and tells you how to install the recorded digest by identity instead. No cosign,
no dependency: the installer is standard library only because a NAS may not let you
install anything.

**Read the version you have before you expect that line.** A release is cut before it
is pushed, and in that window the manifest records `index_digest: null`, the installer
carries no digest, and what preflight prints is this instead:

```
  ok   installing ghcr.io/spdrman/backup-manager:0.3.0
  !!   0.3.0 is cut and not pushed, so container/release-manifest.json records no
       identity for it and there is nothing here to hold
       ghcr.io/spdrman/backup-manager:0.3.0 to.
```

That is 0.3.0 today. It is a warning and never a refusal, and the difference is the
whole design: the alternative was to move the version and leave 0.2.0's digest behind,
which compares a perfectly correct 0.3.0 image against the previous release's identity
and hands every operator exit 52 on a good install. The digest is filled in, and this
installer reissued with it, when the release workflow has pushed and the digests are
recorded back into the manifest. Until then, `--release 0.2.0` installs the last
release this can prove, or `--image-archive` installs a build you made yourself.

That proof only covers the release the installer carries, and a release cut after
this installer was written can never have a digest in it. That is why the `--image`
default is pinned rather than floating onto whatever is newest: a floating default
would install on the registry's word alone, which is the posture the digest exists
to replace.

What the installer does instead is tell you. Preflight lists the published releases,
ordered by version rather than by push order and with prereleases excluded, and says
so when a newer one exists and where to get its installer. That check is read-only,
it never changes what is installed, and if it cannot reach the registry the only
consequence is a missing line.

## Install modes, and what each one keeps

`install` has three modes, chosen with one flag rather than three, because two knobs
for one decision is how they end up disagreeing.

| `--mode` | keeps | destroys | archives first |
|---|---|---|---|
| `fresh` | nothing to keep | nothing | nothing |
| `upgrade` | users, backup sets, catalog, retained backups | nothing | state, users, config, imported keys, pinned host keys |
| `factory-reset` | retained backups | administrator record, catalog, configuration | the same set, moved rather than copied |

`fresh` is the default when nothing is installed, and it refuses when something is:
fresh means the host is empty, so meeting an install contradicts the instruction.

`upgrade` copies the state aside before touching anything and reports where. It does
not copy the retained backups: they are the point of the product, they can be
enormous, and an upgrade does not modify them, so duplicating them would double the
disk usage and protect against nothing. Upgrading onto the version already installed
converges and says so, rather than claiming a version moved. Upgrading onto an
**older** version is refused, because a catalog written by a newer build is not
something this can promise to read back.

`factory-reset` prints what it will destroy, by name and count, **before** it asks, and
it has to be confirmed in as many words: the literal word `factory-reset` typed at the
prompt on a terminal, or `--confirm-factory-reset` where there is no terminal to type
on. `y` is what a finger presses to get past a prompt; the word is what somebody types
having read the list above it. It moves that state into a timestamped archive rather
than deleting it, so the decision stays recoverable. It leaves the retained backups on
disk (it drops the catalog that describes them, not the files), and it leaves
`<prefix>/secrets` alone, so `--ssh-key` and `--known-hosts` survive a reset. The
preview names all three.

Both modes **stop the stack first**, and this is not housekeeping. The engine opens the
state database with `journal_mode=WAL`, so copying it while the engine is running
produces a torn snapshot whose newest committed transactions are in a `-wal` beside it,
and moving it out from under an open file descriptor does not stop the engine writing
to it. A factory reset uses `down` rather than `stop` for a third reason: `docker
compose up -d` against a stack whose configuration has not changed is a no-op, so a
reset back onto the same version with an unchanged `.env` left the old engine serving
the old catalog while the installer printed success.

The archive captures `state.db`, `state.db-wal` and `state.db-shm` together, because
the first without the other two is a database missing its most recent commits. It puts
`state/local-auth.json` **first**, because the plan is executed in order and a failure
part way through a move otherwise leaves the catalog gone and the administrator record
present, which is an engine that reports an administrator already exists, issues no
enrollment link, and locks everyone out. That is the exact failure the archive exists
to prevent.

Archives are not pruned, and they are not unlimited either. Each one holds a copy of
`config/ssh_keys`, which is where the engine keeps the keys imported through the Web UI,
so an unbounded pile is an installer that multiplies private key material every time it
runs. Past five, `install` refuses and names them: deleting one for you would take back
the recoverability that moving rather than deleting is there to provide.

## It will not adopt a layout you did not ask for

Every directory the installer archives, destroys or rewrites used to come from the
current run's flags, and it wrote `<prefix>/.env` without ever reading it back. So an
operator who first installed with `--state-dir /mnt/fast/state` and re-ran without
repeating it got "Archived 0 item(s)", a rewritten `.env`, and a stack pointed at an
empty state directory, while the real catalog sat at the old path with nothing pointing
at it. Every signal was green.

`install` now reads the installed `.env` and refuses (exit 20) when `STATE_DIR`,
`BACKUP_DIR` or `CONFIG_DIR` disagree with this run, naming both sides. It refuses
rather than adopting either: taking the installed paths would ignore flags you typed,
and taking yours would quietly abandon the data.

**With an install already here and no `--mode` given**, the installer asks on a
terminal and refuses without one. It will not guess, because one of the two answers
destroys data and the other does not, and a prompt that blocks a cron job forever is
worse than a refusal that names the flag.

### The credentials in that same `.env` are kept, not re-guessed

`SSH_KEY_FILE` and `KNOWN_HOSTS_FILE` are in the same file and were not held to
anything, and giving `--ssh-key` and `--known-hosts` defaults is what made that bite. A
host first installed with those flags pointing at, say, `~/.ssh/backup_ed25519` and
then re-run bare took the defaults instead, generated a fresh keypair under
`<prefix>/secrets`, rewrote the `.env` to name it, and brought the stack back up
holding a key no source has ever authorised, with the pinned host keys replaced by an
empty file. Nothing refused, the engine came up healthy, and every backup after it
failed to authenticate.

So `preflight` and `install` now read those two keys back and keep them. The asymmetry
with the three directories above is deliberate. There you typed a flag and the two
answers disagree, so naming the disagreement is the only honest move. Here you typed
nothing: one side is a value computed from `--prefix` and the other is what the
deployment actually runs on, and preferring the evidence over the guess ignores nobody.
Each adoption is printed.

A flag you do type still wins, and the line it prints names the path being left behind,
because rotating a key is a real operation and doing it without saying so is not.

The one case that is never filled in is an `.env` naming a path with no file at it:
that refuses (exit 17) and names both the path and the `.env` it came from. Generating
a replacement there is the same silent wrong key by another route, and
`container/compose.yaml` mounts both with `:?` so the stack could not start anyway.

## Compatibility: `--if-installed` is gone

`--if-installed {converge,refuse}` was removed and reconciled into `--mode`:

- `--if-installed converge` is now `--mode upgrade`. Converging is the no-op end of
  upgrading, which is why upgrading onto the same version still runs that path.
- `--if-installed refuse` is now `--mode fresh`.

**This is a breaking change for scripted re-runs.** The old default converged
silently; the new behaviour refuses rather than guess. A script or cron job that
re-runs the installer over an existing deployment must now pass `--mode upgrade`
explicitly.

The flag itself is still registered, hidden from `--help`, for the sole purpose of
saying so. Deleting it outright meant a scripted `--if-installed converge` died at
argparse's own exit 2 with "unrecognized arguments: --if-installed converge", which
names neither `--mode` nor the mapping, so whoever hit it in a cron job had to come and
read the source. It still exits 2, and now it says which flag replaced theirs. A re-run
with no mode flag at all is the other case, and that one exits 20 and names `--mode`.

## It derives from the canonical definition, it does not restate it

`container/compose.yaml` is the canonical runtime contract (issue #167), and
`distribution/compose` fails the build when a derived artifact stops matching it. The
installer stages that file byte for byte and lays one override beside it carrying two
keys per service: `image`, and `pull_policy: never`.

Byte for byte, from one of two places, and never a template. Templating a compose file
here would create a second definition of the runtime that no gate compares to the first,
and the two would drift the moment either changed.

| `--compose-file` | what gets staged |
|---|---|
| not given | the copy embedded in the installer, generated from `container/compose.yaml` |
| a real file | that file, copied verbatim |
| a path with no file there | nothing: exit 19 |

The embedded copy is generated, not written. `scripts/install/embed_compose.py` is the
only supported way to move it, the block it writes carries a `DO NOT EDIT BY HAND`
banner naming that command, and two tests hold it to the canonical file: one compares
the two as **bytes** (not as decoded text, which would normalise line endings and cannot
even be read in a non-UTF-8 locale, since the file has a section sign and em dashes in
it), and one compares the recorded `EMBEDDED_COMPOSE_SHA256` against the same file.

Those tests only exist inside a checkout, and the point of embedding is that the
installer travels without one, so the shipped artifact also checks itself: `preflight`
and `install` verify the embedded definition against that digest before anything is
staged, and refuse with exit 19 if the script has been edited since it was generated.
Truncation would be loud anyway, because Python stops parsing. A changed mount, network
or healthcheck would not be, and that is the one the digest catches.

`<prefix>/compose.yaml` is restaged on every run, which is how a runtime-contract
change reaches an installed host. When the file being replaced is not the one going in,
the installer says so and points at the `.env` beside it, because that is where
anything varying per host belongs. It is a notice and not a refusal: refusing would
block every upgrade carrying a legitimate runtime change, which is most of them.

Running the installer **from inside a checkout does not install that checkout's**
`container/compose.yaml`. It installs the embedded copy, like everywhere else, because
"whichever directory the script happens to sit in" is exactly the location-dependent
behaviour embedding removed. Preflight says so when the two differ, and names the flag:
pass `--compose-file container/compose.yaml` to install an uncommitted runtime change.

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

`--fix-network=auto`, which is `install`'s own default, inserts runtime rules and they
are **lost on reboot**, and lost again whenever the host firewall rewrites its own set.
The containers come back either way, so the deployment stops working silently and
nothing re-runs the installer.

`--fix-network=persist` fixes that. It does everything `auto` does and additionally
installs a systemd unit and timer that re-assert the same four rules.

`auto` is `install`'s default and `persist` has to be asked for, because installing a
systemd unit is a larger commitment than inserting a runtime rule and an operator should
get to choose. There is deliberately no second flag: persistence is a value of
`--fix-network`, not a knob that can disagree with it.

Run stand-alone, `network-doctor` defaults to `--fix-network=diagnose` instead, because
a command named "doctor" should report rather than escalate to root and rewrite a
firewall on its own. Ask for `--fix-network=auto` or `persist` explicitly when you want
it to repair. `install`'s default is unchanged: a healthy host is a no-op there either
way, and `persist` is the value that needs asking for.

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
