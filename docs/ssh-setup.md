# SSH/SFTP setup for backup-manager

This is FR-6 from `docs/EPIC.md` ("SSH/SFTP Security") turned into something an
operator can actually follow: how to create the dedicated key, how to lock
down the remote account, and how to capture its host key without just trusting
whatever the network hands you. Read FR-6 and the Security Requirements
section of the EPIC first if you haven't, this file assumes them.

## What backup-manager actually enforces

Before the how-to, the ground truth, because policy documents drift from code
and this one shouldn't get the chance to. `core/internal/transport/rclone/ssh.go`
builds every sftp connection this manager makes, and it refuses to build one
at all unless:

- exactly one of `key_file`, `key.env` or `key.command` names where the SSH
  private key comes from (`key_file` is a deprecated alias for `key.file`;
  see "Choosing a key source" below for all three). There is no fallback to
  an ssh-agent and no password option, because `transport.Source` has no
  password field and this adapter never sets one, even though rclone's sftp
  backend supports both.
- `known_hosts` points at a real, readable file. An empty value is refused,
  and so is the literal string `none`, which is rclone's own way of saying
  "don't check host keys at all." Both would otherwise let rclone accept any
  host key silently, which is the exact failure FR-6 exists to prevent.
- when the key source is `key_file`, the file's mode is exactly `0600`, the
  same check on every connection attempt, not only at import (issue #293).
  Neither rclone's embedded sftp backend nor the `golang.org/x/crypto/ssh`
  library underneath it looks at a key file's permissions at all, unlike a
  real OpenSSH client, so without this check a key that drifted wider after
  import (an operator's own `chmod -R`, a bind mount shared over SMB/AFP,
  troubleshooting on the host) would go on authenticating exactly as well
  as one still at `0600`, silently. A drifted key is refused with a
  diagnostic naming the actual and expected mode, never silently
  re-narrowed back to `0600`: see `checkKeyFileMode`'s doc in `ssh.go` for
  why re-asserting the mode automatically was rejected in favour of a loud
  refusal.

Manually provisioning `key_file` yourself, outside the wizard's "Import key"
step, means you own keeping that `0600` correct: `chmod 600` after generating
the key (see step 1 below) and after any change you make to it later.

So the two things this doc walks through, a key source and a known_hosts
file, aren't just good practice, they're the two things you cannot skip.

## 1. Generate a dedicated SSH key pair

Don't reuse a personal or shared key. Generate one that exists for this
backup job and nothing else, so it can be rotated or revoked without touching
anything unrelated:

```bash
ssh-keygen -t ed25519 -f /etc/backup-manager/ssh/backup_key -C "backup-manager" -N ""
```

The empty `-N ""` means no passphrase. That's deliberate, not an oversight:
backup-manager runs unattended and has nowhere to prompt for one. The
passphrase's job (protecting the key if the file leaks) gets done instead by
filesystem permissions and by mounting the key read-only into wherever
backup-manager actually runs:

```bash
chmod 600 /etc/backup-manager/ssh/backup_key
chown root:root /etc/backup-manager/ssh/backup_key
```

Never commit this key, or any real key, to Git. If you generate it inside a
repo checkout by mistake, `git status` before you commit anything.

## 2. Create the restricted account on the remote server

FR-6 asks for an account that's dedicated to backups, SFTP-only, has no
interactive shell, is confined to backup directories, can list/read/delete
eligible artifacts, but can't modify or replace completed ones, and has no
other server privileges. Here's what that looks like as actual commands, on
the remote server, as root:

```bash
useradd --system --create-home --home-dir /srv/backup-manager \
        --shell /usr/sbin/nologin backupsvc
mkdir -p /srv/backup-manager/incoming
```

`/usr/sbin/nologin` blocks every login path that goes through the account's
shell (console, `su`, cron, anything PAM-based). It does not block SFTP,
because the SFTP subsystem never execs the user's shell in the first place,
it runs the sftp-server binary directly. I confirmed this myself in the
Docker fixture `ssh_test.go` builds: the fixture's `backup` user has
`/sbin/nologin` as its shell and SFTP still works, because the config in the
next step is what actually does the confining.

Add your dedicated public key to this account:

```bash
mkdir -p /srv/backup-manager/incoming/.ssh
chmod 700 /srv/backup-manager/incoming/.ssh
cp /etc/backup-manager/ssh/backup_key.pub /srv/backup-manager/incoming/.ssh/authorized_keys
chmod 600 /srv/backup-manager/incoming/.ssh/authorized_keys
chown -R backupsvc:backupsvc /srv/backup-manager/incoming/.ssh
```

OpenSSH is strict about ownership here and will silently refuse to read
`authorized_keys` if the permissions are too loose, so match the modes above
exactly.

## 3. Chroot and force sftp-only in sshd_config

This is the step that actually confines the account, and it's also where the
"delete but don't modify/replace" requirement lives, since that one isn't a
single config line. Add to `/etc/ssh/sshd_config`:

```
Match User backupsvc
    ChrootDirectory /srv/backup-manager/incoming
    ForceCommand internal-sftp
    AllowTcpForwarding no
    AllowAgentForwarding no
    X11Forwarding no
    PermitTTY no
```

`ChrootDirectory` requires the chroot root itself, and every directory above
the writable part, to be owned by root and not writable by group or other:

```bash
chown root:root /srv/backup-manager /srv/backup-manager/incoming
chmod 755 /srv/backup-manager /srv/backup-manager/incoming
```

Put the actual backup artifacts in a subdirectory the account owns, not in
the chroot root itself:

```bash
mkdir -p /srv/backup-manager/incoming/backups
chown backupsvc:backupsvc /srv/backup-manager/incoming/backups
chmod 750 /srv/backup-manager/incoming/backups
```

Now the "list/read/delete eligible artifacts, but never modify or replace a
completed one" requirement. This is a POSIX permission split that's easy to
get backwards, so here it is spelled out: unlinking (deleting or renaming) a
file is controlled by write permission on the *directory* it lives in.
Overwriting a file's *content* is controlled by write permission on the file
itself. Those are two independent checks, which is exactly the lever FR-6
needs:

- Give `backupsvc` write+execute on `backups/` (already done above), so it
  can delete entries.
- Have whatever process produces the completed artifacts write them as a
  *different* user, and land them read-only to `backupsvc`:

```bash
# run as the producer account, after a backup artifact is finalized
chown produceruser:backupsvc /srv/backup-manager/incoming/backups/some-artifact.dump.zst
chmod 440 /srv/backup-manager/incoming/backups/some-artifact.dump.zst
```

With that combination, `backupsvc` can `list`, `get`, and `rm` the artifact
over SFTP (directory permissions allow it), but `open()`-ing it for write
fails (file permissions block it), so a compromised or buggy
`backup-manager` process can delete a stale backup once it's confirmed
durably copied elsewhere, but it can never quietly corrupt or replace one in
place. Also make sure the `backups/` directory does **not** have the sticky
bit set: a sticky directory restricts deletion to the file's owner, which
here is the producer account, not `backupsvc`, and would break the delete
path this account needs.

Finally, `backupsvc` should not be in any admin/sudo group and should have no
other login method on this host. Check with `sudo -l -U backupsvc` (expect
"not allowed to run sudo") and `id backupsvc` (expect only its own group).

Test the sshd config before reloading, then reload:

```bash
sshd -t
systemctl reload sshd
```

## 4. Capture the server's host key, verified, not just trusted

This is the part that matters most, so don't shortcut it.
`ssh-keyscan`/first connection alone is trust-on-first-use: whatever key
answers the first time gets recorded, no matter who's actually answering. To
get real assurance, verify the fingerprint out-of-band, over a channel you
already trust, before you write anything to `known_hosts`:

```bash
# on the remote server itself, over a channel you already trust
# (cloud console, physical access, an already-verified session)
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

Then, from wherever backup-manager will actually connect from:

```bash
ssh-keyscan -t ed25519 -p 22 production.example.internal > /etc/backup-manager/known_hosts
ssh-keygen -lf /etc/backup-manager/known_hosts
```

Compare the two fingerprints by eye. If they don't match, stop, you're either
talking to the wrong host or something is intercepting the connection. Only
once they match does `/etc/backup-manager/known_hosts` mean anything.

From here on, backup-manager itself is what keeps this honest: if that host's
key ever changes, whether from a legitimate server rebuild or from something
worse, every connection attempt gets refused until a human repeats this
verification step and updates the file on purpose. That refusal is exactly
what the integration test in `core/internal/transport/rclone/ssh_test.go` proves:
it stands up a real SFTP server, records its key, swaps in a second server
with a different one on the same address, and checks that the connection is
refused rather than silently reconnecting to whatever answered.

## Choosing a key source (#74)

Everything above produces one thing: a private key file. What you point
backup-manager at is a separate decision, and there are three ways to make
it:

```yaml
remote:
  type: sftp
  host: production.example.internal
  port: 22
  user: backupsvc
  known_hosts: /etc/backup-manager/known_hosts
  key:
    file: /etc/backup-manager/ssh/backup_key
    # env: BACKUP_SSH_KEY
    # command: ["op", "read", "op://infra/backup-manager/private-key"]
```

Exactly one of `key.file`, `key.env` or `key.command` goes in that block (the
bare top-level `key_file: /path` shape from earlier `docs/EPIC.md` examples
still works too, unchanged, as a deprecated alias for `key.file`). Setting
two is a config error backup-manager refuses at startup, not a precedence
order it picks through for you.

**`key.file` is the one to actually use, and it is the default for a
reason.** It is the only one of the three where the key's bytes never enter
backup-manager's own memory at all: rclone opens the file itself, reads it,
and that's the end of this program's involvement. Nothing to validate,
nothing to wrap, nothing that could theoretically end up somewhere it
shouldn't, because it never comes near this process in the first place.
Everything from step 1 through step 4 above is written assuming this is what
you use.

**`key.env` and `key.command` exist for a specific, narrower reason:**
adopting a secrets manager (OpenBao, Vault, SOPS, 1Password, AWS Secrets
Manager, or anything else with a CLI) later, without a second change to this
config's shape. `key.command` is the general form: an argv array, run
directly, never through a shell, so putting `;`, `|`, `` ` ``, or `$(...)` in
one of its elements does nothing but sit there as a literal character in an
argument, exactly like every other byte in that argument. It runs with a
15-second timeout and a fixed, minimal environment (`PATH` only, nothing
inherited from backup-manager's own process), so the command has to be
self-sufficient: an absolute executable path, and whatever authentication it
needs already sitting on disk or baked into a wrapper script, not passed
through an environment variable backup-manager would otherwise have to trust
it with.

Both of these put the resolved key in backup-manager's memory for the
duration of one connection attempt, which is exactly the cost `key.file`
avoids. In exchange, backup-manager validates what came back before it goes
anywhere near rclone: it must parse as an unencrypted SSH private key, or the
connection attempt fails right there, by name, rather than turning into a
confusing rclone dial error somewhere else. A secrets manager CLI that isn't
authenticated, is pointed at the wrong path, or answers with an HTML login
page on stdout is refused for exactly that reason: "this is not a key,"
never silently accepted. A key that needs a passphrase is refused too, with
that named as the problem: backup-manager runs unattended, and there is
nowhere for a passphrase prompt to go. None of this resolved material is
ever logged, at any level, including debug; it is held only long enough to
open the connection.

There is no field anywhere in this configuration for pasting key bytes
directly into YAML, and there never will be: `key.file`, `key.env` and
`key.command` all name WHERE the key lives, none of them carry it.

## Encrypting the key store at rest (#298)

Everything above defends the key in transit and defends the account it
authenticates. It says nothing about the key FILE itself once it's sitting
on the NAS: by default, an imported key (the wizard's "Import key" step, or
`key.file` pointed at a file you provisioned by hand) is a plain,
unencrypted PEM file on disk, protected only by the filesystem permissions
from step 1. That is fine on a host you control end to end. It is not fine
the moment anything else can read that filesystem, and the case that
motivated this section was exactly that: an operator decrypted a
passphrase-protected production key to get it into a form backup-manager
could use, and the resulting plaintext file sat on a volume also reachable
over an SMB/AFP share, entirely outside backup-manager's own permission
model.

`key_encryption` closes that gap, and it is entirely optional:

```yaml
key_encryption:
  file: /etc/backup-manager/secrets/key.dek
  # env: BACKUP_MANAGER_KEY_DEK
  # command: ["op", "read", "op://infra/backup-manager/key-encryption-key"]
```

Exactly one of `file`, `env` or `command` goes in that block, the identical
shape `key` and `key.passphrase` already use, for the identical reason:
nowhere in this configuration can an operator paste the actual encryption
key in directly. Leaving the whole block out, which is every `config.yaml`
written before this feature existed, means exactly what it always has:
an imported key is stored as plain PEM, defended only by permissions.

**What this does and does not defend, stated plainly:**

- It defends the key FILE against being read directly: disk theft, a copy
  taken by a backup of backup-manager's own state directory, and -- the
  case this was filed over -- access through an SMB/AFP share exported
  from the same volume, which can bypass Unix owner-only file permissions
  entirely depending on how the share itself is configured. An attacker
  with any of those three now gets ciphertext, not a usable key.
- It does NOT defend a live process's memory. Authenticating a connection
  still requires the plaintext key in backup-manager's own memory for the
  duration of that attempt, exactly like `key.env` or `key.command`
  already put a resolved key in memory today. A process compromise, a core
  dump, or a debugger attached to a running backup-manager can still reach
  the key. If that threat matters more to your deployment than the
  at-rest one, this feature does not change your risk there either way.
- **It does NOT defend anything if the encryption key (the `key_encryption`
  block above resolves to it) lives inside the same share or backup root
  the key file itself lives in.** If `key_encryption.file` points at a path
  under the SMB/AFP-exported tree, or under whatever directory gets backed
  up alongside the encrypted key, anyone who could previously read the
  plaintext key can now read the encryption key and the ciphertext side by
  side and undo this entirely. Put it somewhere that export, and any backup
  of it, never reaches: a directory local to the host that is not shared
  and not part of the backed-up tree, an environment variable set only in
  backup-manager's own runtime, or a secrets manager via `command`.

**What happens to a key imported before this was configured:** nothing, on
its own. `key_encryption` is opt-in and config-wide, not per-source, so an
existing plaintext key file is picked up automatically the first time a
source that uses it actually connects (a real cycle, or the wizard's "Test
connection" step) once `key_encryption` is set: backup-manager detects the
file is still plaintext, encrypts it in place with the configured key, and
authenticates that same connection with the key it just read, all in memory,
with the plaintext bytes never written back to disk. There is no separate
migration command to run and no window where the key is unreadable; the
first real use after you add the `key_encryption` block IS the migration.

An encrypted key file is easy to tell apart from a plain one if you ever
need to check: a plain key still begins `-----BEGIN `, exactly like
`ssh-keygen` produces; an encrypted one begins with backup-manager's own
`RCLONEMGR-KEYENC-V2:` marker instead (a `RCLONEMGR-KEYENC-V1:` file just
means it predates the DEK derivation hardening below -- it upgrades to V2
automatically on the next real use, no action needed) and is not valid PEM
to any other tool, `ssh-keygen -lf` included, on purpose -- nothing but
backup-manager's own configured `key_encryption` source can read it.

The key that actually encrypts the file (the DEK, "data encryption key") is
never `key_encryption`'s resolved value used raw: it's run through
[Argon2id](https://en.wikipedia.org/wiki/Argon2), the same password-hardening
function this project already uses for the Web UI administrator's own
password, salted per key file. That matters because `key_encryption.env`
and `key_encryption.command` are documented above as accepting "a secrets
manager", and nothing stops that from being a typed passphrase instead --
Argon2id is what makes guessing that passphrase against a stolen encrypted
key file expensive, rather than one hash call per guess.

## 5. Point a source at it

Using the shape from FR-5's config example in `docs/EPIC.md`:

```yaml
sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        remote:
          type: sftp
          host: production.example.internal
          port: 22
          user: backupsvc
          key_file: /etc/backup-manager/ssh/backup_key
          known_hosts: /etc/backup-manager/known_hosts
        remote_path: /backups
```

(Or, per "Choosing a key source" above, `key: {file: ...}`, `key: {env: ...}`
or `key: {command: [...]}` instead of the bare `key_file` line.)

Mount `backup_key` and `known_hosts` read-only into wherever backup-manager
runs, per the Security Requirements section's "credentials mounted read-only
where practical." (`key.env` and `key.command` are the exception: there is
nothing to mount for either, since the key never lives in a file backup-manager
reads on this host at all.)

If this remote's host, port or account name must never appear in a log line
or a journal detail (issue #295), for example a deployment where the port
itself is treated as a credential, add `sensitive_endpoint: true` alongside
the fields above. It defaults to false: most deployments would rather a
connection failure said what it couldn't reach, so this is something a
config asks for, not something backup-manager decides on its own.

### Hosts that cap simultaneous connections

A hardened host often refuses a third simultaneous SSH connection from one
address rather than queueing it, whether through `sshd_config`'s
`MaxStartups` or an iptables `connlimit` rule. Against a host like that,
opening one connection too many is not slow, it is a failed backup, and it
surfaces as a bare `connection refused` that points at nothing (issue #264).

backup-manager stays under such a limit on its own: every operation it
performs (list, stat, copy, hash, delete) opens one connection and hands it
back when the operation finishes. That is not rclone's default behaviour and
it is not free, so it is worth knowing what it costs and where it came from:
rclone walks a directory tree it cannot list recursively with one goroutine
per `--checkers` (eight), and splits a download above 256MiB across
`--multi-thread-streams` (four) concurrent readers. On sftp each of those is
its own connection, so out of the box a plain listing of a nested tree and a
copy of a large dump are eight and four connections respectively. Both are
pinned to one here.

If you want the limit stated to rclone directly as well, add a ceiling to
the remote:

```yaml
        remote:
          type: sftp
          # ... the fields above ...
          max_connections: 2
```

Two things to know before you rely on it. It bounds one *operation*, not the
host: a scheduled cycle and someone clicking "test connection" in the web UI
are two operations against one host, and each gets its own budget. And it is
a different setting from rclone's `concurrency`, which is how many requests
are in flight *inside* one connection (backup-manager pins that at 64, and it
is what keeps a single connection fast). Omit it, or set `0`, for rclone's
own unlimited default, which is what every config that predates this field
means.

## 6. Verify it end to end before pointing it at anything real

Before trusting this setup with production data, do a manual sanity check
with the same files backup-manager will use:

```bash
sftp -i /etc/backup-manager/ssh/backup_key \
     -o UserKnownHostsFile=/etc/backup-manager/known_hosts \
     -o StrictHostKeyChecking=yes \
     backupsvc@production.example.internal
```

You should land in `backups/` with no shell, no password prompt, and the
ability to `ls`, `get`, and `rm` but not overwrite an existing artifact
in place. If any of that isn't true, fix it here before wiring the config in
step 5, since backup-manager itself will fail the same way for the same
reason.

## What this setup deliberately refuses

- `known_hosts: none` or an empty `known_hosts`. Both disable host-key
  verification, and this adapter refuses to build a connection at all rather
  than let that happen.
- No key source configured at all, expecting an ssh-agent or a password
  prompt to cover for it. Neither is offered. Configure `key.file`,
  `key.env` or `key.command` (or the deprecated `key_file` alias) or the
  source doesn't run.
- More than one key source configured at once. Two is a mistake to fix, not
  a precedence order backup-manager guesses through.
- A `key.env` or `key.command` resolver whose output doesn't actually parse
  as an unencrypted SSH private key (an error string, an HTML login page, an
  empty body, or a passphrase-protected key). All of these fail loudly at
  the point the bytes were produced, never as a confusing rclone error later
  and never as a hang waiting on a passphrase prompt.
- Raw key material written directly into config. `key.file`, `key.env` and
  `key.command` only ever name where the key lives; there is no field to
  paste key bytes into.
- A `key.command` invoked through a shell. It always runs as a literal argv
  array; nothing in it is ever interpreted by `/bin/sh` or any other shell.
- A changed host key at a previously-known address, without a human repeating
  step 4 on purpose.
- More than one `key_encryption` source configured at once, on the same
  "a mistake to fix, not a precedence order" reasoning as the key sources
  themselves. A `key_encryption` source that doesn't actually decrypt an
  at-rest-encrypted key file fails that connection loudly rather than
  silently falling back to treating the file as plaintext.
