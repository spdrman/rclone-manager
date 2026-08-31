# Backup Manager on Dockge

Dockge manages **the canonical Compose stack**. There is no Dockge packaging in
this directory and there is deliberately no compose file here either, because
adding one would create a second definition of the same stack for the same kind
of host, which is the fork issue #170 rules out.

This is new platform support, not a conversion: no Phase 4 issue targeted
Dockge, so there is nothing here that replaces earlier packaging.

## Why this directory holds only a page

Every other adapter in `apps/` ships a metadata artifact, because every other
platform consumes one: a catalog entry, a Docker template, an app-store compose
file, an App Template. Dockge consumes a directory containing `compose.yaml`,
which is what `container/compose.yaml` already is. So Dockge is supported by
**compatibility**, and the deliverable is the import workflow below plus the
checks that keep it true.

That is deliberate, and it is the reason this page exists at all. A reader
scanning `apps/` and finding a Dockge directory with no stack in it should be
able to see immediately that the emptiness is the design.

What holds it in place:

- `TestDockgeShipsNoRuntimeDefinitionOfItsOwn` fails if any compose file,
  template or other runtime definition appears under `apps/dockge/`, and it is
  proved to fire against a fixture that has one.
- `TestTheDockgeStackIsTheCanonicalStack` pins the host paths this page tells an
  operator to use to `container/.env.example`, so the workflow and the canonical
  runtime cannot drift.
- The Dockge column of the release matrix is resolved against
  `container/compose.yaml` itself, not against a copy, so every capability it
  records is a statement about the canonical stack.

## Import and deploy

Dockge keeps one directory per stack under its stacks root, `/opt/stacks` by
default.

1. Create the stack directory and copy the canonical stack into it:

   ```
   mkdir -p /opt/stacks/backup-manager
   cp container/compose.yaml /opt/stacks/backup-manager/compose.yaml
   cp container/.env.example /opt/stacks/backup-manager/.env
   ```

2. Edit `/opt/stacks/backup-manager/.env`. Every host path in it must exist and
   be owned by `PUID:PGID` before the first start, because the runtime image is
   distroless and cannot create or chown anything for you:

   ```
   mkdir -p /volume1/backup-manager/state /volume1/backups \
            /volume1/backup-manager/config /volume1/backup-manager/secrets
   chown 1000:1000 /volume1/backup-manager/state /volume1/backups \
                   /volume1/backup-manager/config /volume1/backup-manager/secrets
   ```

3. In Dockge, the stack appears on its own. Press **Start**, watch the two
   containers come up in the interactive log pane, and open the published port.
   The engine prints a one-time enrollment link on first start; it is in the
   `rclone-manager` container's log.

Dockge's editor writes back to the same `compose.yaml`, so anything changed in
its UI is a change to your copy of the canonical stack, not to this repository.

## One thing to know before you import

`container/compose.yaml` carries a `build:` block, because it is also the file
that builds the canonical image from a source checkout. Dockge imports a stack
as a directory of files with no repository behind it, so on a Dockge host the
build context is not there.

Two ways round it, and both are the operator's ordinary choice rather than a
Dockge-specific workaround:

- pull instead of build: make the image resolvable on the host first, either by
  pushing it to a registry you control or with `docker save` and `docker load`,
  set `VERSION` in `.env` to the tag you loaded, and delete the four `build:`
  lines from your copy. Dockge then starts the stack without a build context.
- or build once elsewhere and load the result, which is what
  `docs/acceptance/dockge-stack-import.md` step 0 does.

This is written down here rather than fixed in code because it is not a Dockge
incompatibility: it is the same choice every pull-based deployment of the
canonical stack makes, and issue #170 asks for a real incompatibility to be
recorded before any code is written for one. Nothing in this repository was
written for it.

## What Dockge does not give this product

No native authentication, no notification bridge, no embedded window, no storage
picker, and no application store. Dockge is a compose manager: it starts,
stops, edits and shows logs. The web UI is the shared one, served from the
bundle compiled into the binary, and it reports the deployment as a Docker
Compose deployment, which through Dockge it is.

## Which mount holds what

The mounts are the canonical stack's, with `container/.env.example`'s defaults:

| `.env` variable | Container path | Holds |
| --- | --- | --- |
| `STATE_DIR` | `/data/state` | the catalogue and the local administrator record. Private. |
| `BACKUP_DIR` | `/data/backups` | retained artifacts, and nothing else. |
| `CONFIG_DIR` | `/etc/backup-manager/config` | `config.yaml`, writable, plus `ssh_keys/` and `known_hosts.d/`. |
| `SSH_KEY_FILE` | `/etc/backup-manager/id_ed25519` | the SFTP private key, read-only. |
| `KNOWN_HOSTS_FILE` | `/etc/backup-manager/known_hosts` | the pinned host key, read-only. |

The full operator procedure, including update, removal and the evidence that
retained backups survived, is
[`docs/acceptance/dockge-stack-import.md`](../../docs/acceptance/dockge-stack-import.md).
Nothing in it has been executed and no criterion in it is ticked.
