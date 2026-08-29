# ADR 0003: Pull age-encrypted runs to the UGREEN NAS, replacing the S3 off-host copy

## Status

Accepted, not yet implemented. Supersedes the S3 half of `iasbuilt/IaC`
decision D22 for the Gitea forge. Blocked on SSH being enabled on the NAS.

## Context

The forge at `cicd-pipeline` writes a nightly Gitea backup run to
`/opt/gitea-backups/<RUN_ID>/`. Since issue 254 it also age-encrypts that run
into `/opt/gitea-backups/.offhost-stage/<RUN_ID>/`, pushes the ciphertext to an
append-only S3 bucket, and then deletes the staging directory.

We are replacing S3 with this manager pulling to a UGREEN NAS on the local
network. The decision is the owner's. This ADR records how it should work and,
more importantly, which existing guarantees must survive the change, because
the code being altered is the code that guards production backups.

Three facts constrain everything below.

**The archives are credentials.** The role's own comments record that the
forge's `SECRET_KEY` is empty, so Gitea falls back to a hardcoded public
constant and the 24 stored Actions secrets are plaintext-equivalent to anyone
holding the database. `SSH_PRIVATE_KEY`, which reaches production, is one of
them. An off-host copy of these archives is an off-host copy of production
credentials.

**This manager cannot encrypt.** There is no encryption anywhere in this
codebase. That is deliberate and this ADR does not change it.

**Encryption doubles peak disk on the VPS.** age does not encrypt in place, it
writes a second near-complete copy. The role records a real 9.3 GB run taking
the host from 20 GB free down to 1022 MB, on the same disk that carries
Postgres and `/var/lib/docker`.

## Decision

**Encryption stays upstream, on the VPS, unchanged.** This manager pulls
ciphertext the VPS has already produced and never sees plaintext. Moving
encryption here would mean building it in a program that deliberately has none,
and would put a decryption identity on the machine holding the archives.

**The staging directory becomes a handoff rather than a scratch space.** The
VPS encrypts and stops. It no longer ships, and no longer removes the staging
root itself.

**Our FR-15 delete replaces the VPS's `rm -rf`.** We delete a remote source only
after a verified, durably committed local copy and a re-confirmed remote
identity. The path it replaces deleted unconditionally once rclone reported
success. This is a strengthening, not a like-for-like swap.

**The staging area is a single slot.** It holds the newest run only. A run
starting while the previous ciphertext is still unpulled overwrites it. Losing
intermediate days on the NAS is an accepted compromise, decided by the owner,
and it is what bounds VPS disk to one run's ciphertext rather than letting an
offline NAS fill the disk that Postgres lives on.

**Absence of the previous stage is the acknowledgement.** This is the part worth
reading twice.

The VPS currently prunes superseded local plaintext runs only when
`OFFHOST_STATUS == 0`, and always keeps the newest run for fast restore. That
guard exists so the script can never delete the only copy. A pull model looks
like it breaks the guard, because the VPS cannot observe whether we succeeded.

It does not, because of what our delete already means. We remove a remote source
only after a verified durable commit, so a stage directory that has vanished
was taken by us and by nothing else. The rule becomes:

- previous run's stage already gone when this run encrypts: the NAS has it, so
  superseded local plaintext runs may be pruned exactly as today
- previous run's stage still present: the NAS is behind, so keep the local
  plaintext runs and raise the existing off-host-failure alert

The same guarantee, inferred from the other side, with no new protocol, no
callback, and no state shared between the two machines.

## Consequences

The VPS stops holding credentials for the destination. Today it pushes to S3 and
therefore holds an IAM key. A pulled destination inverts that, and the role's own
README already argues a pulled or write-only destination beats anything the cicd
host can delete.

We become a load-bearing part of the estate's backup path rather than a
convenience. If this manager stops running, ciphertext accumulates for exactly
one slot and then starts being overwritten, so the NAS silently stops gaining
new restore points while the VPS looks healthy. Our own freshness and health
surfaces (FR-24) are what must catch that, and a stalled pull is now also a VPS
disk story, so it belongs in metrics as well.

Restores now need the age identity. Nothing in this repository references it and
nothing should. It is the operator's, it must live somewhere that survives
losing both the VPS and the NAS, and `docs/recovery.md` should say so plainly,
because a decryption key that exists only inside the thing it decrypts is not a
key, it is a souvenir.

Intermediate days are genuinely lost when the NAS is late. That is the accepted
trade and it should not be quietly re-litigated later: the alternative was
unbounded ciphertext on a disk whose exhaustion takes the forge down.

## Alternatives considered

**Pull plaintext and encrypt here.** Rejected. It puts encryption in a program
that has none, and either the NAS holds a decryption identity or the plaintext
crosses the network and lands at rest unencrypted. The archives are credentials,
so neither is acceptable.

**Keep S3 as well as the NAS.** Not rejected on merit, and it is a strictly
better posture, but it is not what was asked for and it keeps an IAM key on the
VPS. Worth revisiting if the NAS turns out to be less reliable than expected.

**Let ciphertext accumulate and alert on disk pressure.** Rejected by the owner
in favour of the single slot. Accumulation protects intermediate days at the
cost of the one failure that takes the forge offline, and an alert at 03:30 that
nobody acts on before the disk fills protects nothing.
