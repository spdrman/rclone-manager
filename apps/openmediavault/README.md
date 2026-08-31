# OpenMediaVault provider profile

Tier C (§4A): a supported provider deployment profile, one tier below TrueNAS and
Unraid. OMV gets no lifecycle engine, no app-store package and, deliberately, no
native plugin. Everything in this directory is metadata pointed at the exact
canonical OCI image, so an OMV install runs byte-identical software to a generic
Docker install, a UGOS install, a TrueNAS install or an Unraid install.

**Status: build-supported and uncertified (§68).** Nothing here has been installed
on an OMV system. Run
[docs/acceptance/openmediavault-provider-acceptance.md](../../docs/acceptance/openmediavault-provider-acceptance.md)
to change that.

## There is no native plugin, on purpose

Section 4A defers a native OMV Workbench plugin and WP4.3 says not to build one in
v1. Concretely that means no entry in OMV's navigation tree, no Workbench form, no
RPC service, no salt state and no `openmediavault-backupmanager` Debian package.

A deferral that nothing enforces quietly stops holding, so
`apps/common/packaging` scans this directory for plugin material (a `debian/`
tree, a `salt/` tree, a `workbench/` or `rpc/` directory, an `omv-mkconf` hook) and
fails the build if any turns up. The frontend bridge's own comment says the same
thing from the other side: a future native OMV shell replaces
`frontend/platform.ts` only, with no shared-page changes.

The practical consequence is that OMV will not show you a link to this app.
Everything an operator needs to find it has to be here instead, which is what the
"Reaching the Web UI" section below is for.

## What is here

| Path | What it is |
| --- | --- |
| `compose/backup-manager.yml` | The deployment. Paste it into the File field of Services, Compose, Files, Add. |
| `compose/backup-manager.env` | Every host path, the image reference, the uid/gid and the port. Paste it into the Environment field. This is the only file an operator edits. |
| `frontend/platform.ts` | The shared platform bridge (§3.5). Provider identity and storage expectations only. |

No Go, no shell, no install hook.

## Prerequisites

The `openmediavault-compose` plugin, from omv-extras:

```bash
apt-get install openmediavault-compose
```

## The one substitution that matters

`backup-manager.env` sets `DISK=/srv/dev-disk-by-uuid`, and that is a placeholder.
A real OMV system mounts data filesystems at `/srv/dev-disk-by-uuid-<UUID>/`, with
the UUID differing per machine, so no checked-in default can be literally correct.
It matches what `frontend/platform.ts` already declares, so the two stay
consistent, and `apps/common/packaging` pins them together.

Find yours:

```bash
ls -d /srv/dev-disk-by-uuid-*
```

Every host path in the compose file comes from a variable in the env file, so the
UUID is substituted once rather than five times.

## Two containers, one image

`backup-manager` runs `/backup-manager-web serve`: local authentication, the
versioned `/api/v1` API and the backup scheduler in one process sharing one
shutdown context. It holds the state database and the credentials, and it publishes
no port.

`backup-manager-ui` runs `/backup-manager-web serve-ui`: the shared static UI plus
a reverse proxy to the engine. It is the only container with a published port, and
it mounts nothing at all.

Same image, different argv. The image ships no `ENTRYPOINT` and no `CMD` on
purpose, because no single default would be right for both of its binaries.

The Web UI container overrides the image's baked-in healthcheck. The image runs
`/backup-manager status`, which needs a config file and a state database that
container does not have, so left inherited it would report unhealthy forever while
working perfectly.

## Storage

| Role | Env variable | Host default | In the container | Mode |
| --- | --- | --- | --- | --- |
| State | `STATE_DIR` | `$DISK/appdata/backup-manager/state` | `/data/state` | rw |
| Backups | `BACKUP_DIR` | `$DISK/backups` | `/data/backups` | rw |
| Config | `CONFIG_FILE` | `$DISK/appdata/backup-manager/config/config.yaml` | `/etc/backup-manager/config.yaml` | ro |
| SSH key | `KEY_FILE` | `$DISK/appdata/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | ro |
| Known hosts | `KNOWN_HOSTS_FILE` | `$DISK/appdata/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | ro |

Appdata holds private state; `$DISK/backups` holds retained artifacts. That split
is a rule rather than a preference: §19.2 makes them separate security domains, and
the backup root must never contain SSH private keys or authentication state.
`apps/common/packaging` checks the containment in both directions on every commit.

Every persistent path is a bind mount to a host path you chose, never a named
volume, which is precisely why `docker compose down -v` cannot reach your backups.

Create all five paths and own them by `PUID:PGID` before first start. The runtime
image is distroless: no shell, no root step, no init process, so nothing inside the
container can fix ownership for you.

## Reaching the Web UI

```
http://<omv-host>:<WEB_PORT>/
```

`WEB_PORT` is set in one place, `backup-manager.env`, and defaults to 8080. If that
collides with something already on the host (OMV's own Workbench, or another
container), change it there and re-run **Up**; nothing else needs editing.

There is no OMV navigation entry, no dashboard widget and no service list row for
this app, for the reason at the top of this file. Bookmark the URL.

## Authentication

Local accounts only (§13A), the same reusable local authentication the generic Web
host provides: first-run administrator enrollment through a single-use token the
engine prints to its own log, Argon2id password hashing, an HTTP-only session
cookie, CSRF protection and per-IP rate limiting.

The OMV Workbench login cannot log into Backup Manager and Backup Manager's
administrator cannot log into the Workbench. Nothing in this directory ships a
credential, and `apps/common/packaging` scans for one on every commit.

## config.yaml

The engine calls `core/service.Open`, which loads **and validates** the config file
before the HTTP listener starts. A missing or invalid file is a hard start failure,
not a first-run wizard. See `apps/truenas/README.md` for an annotated example; the
only difference here is the host path it lives at. The container-side paths in it
are fixed by this profile and must not be changed.

## The image reference

No registry is configured for this repository yet, so
`ghcr.io/spdrman/backup-manager:1.0.0` is the intended publish target rather than
something that resolves today. `apps/common/packaging/canonical.json` records that
honestly, and step 0.2 of the acceptance procedure covers pushing to your own
registry or side-loading a saved image in the meantime. It is the `IMAGE` variable
in the env file, and nothing else.
