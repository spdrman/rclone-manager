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


## Converted to a thin adapter (issue #169)

Everything here is now **derived** from the one authoritative Compose runtime
definition rather than authored beside it. `distribution/packaging`'s
derivation gate holds this platform's image reference, runtime profile,
mounts, published port, health check and architectures to `canonical.json`,
and a deliberate mismatch fails the build naming the field that drifted. What
that means in practice for this directory:

- both services select the `openmediavault` runtime profile with `--profile=`, so
  the platform this deployment reports itself as is a selection rather than a
  build-time constant;
- the Web UI container sets `UI_ROOT=/ui/bundles`, so it serves this
  platform's own frontend bridge out of the canonical image rather than the
  generic one (issue #180). A missing bundle is a hard start failure, never a
  silent fall back;
- the configuration mount is a writable **directory** holding `config.yaml`
  instead of a read-only single file (issue #196).

The upgrade path from the Phase 4 packaging, including the one renamed mount,
is in
[docs/runtime-contract.md](../../docs/runtime-contract.md#migrating-a-phase-4-installation).
No state or backup data moves.

## There is no native plugin, on purpose

Section 4A defers a native OMV Workbench plugin and WP4.3 says not to build one in
v1. Concretely that means no entry in OMV's navigation tree, no Workbench form, no
RPC service, no salt state and no `openmediavault-backupmanager` Debian package.

A deferral that nothing enforces quietly stops holding, so
`distribution/packaging` scans this directory for plugin material (a `debian/`
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
consistent, and `distribution/packaging` pins them together.

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

Both containers override the image's baked-in healthcheck, for two different
reasons. The image runs `/backup-manager status`, which needs a config file and a
state database the Web UI container does not have, so left inherited there it
would report unhealthy forever while working perfectly.

The engine's override is the one that decides whether you get a page at all. The
Web UI will not start until the engine reports healthy, and `backup-manager
status` is FR-24's backup-freshness verdict: it exits non-zero on any DEGRADED,
STALE or FAILING set, and on a fresh install, which has backed nothing up yet. So
the engine asks `/health/live` instead, a liveness probe that needs no
configuration. Backup freshness is still reported, by the image's own HEALTHCHECK
for a plain `docker run`, by the alerts block, and by
`docker exec backup-manager /backup-manager status`; it just no longer decides
whether a container starts.

## Storage

| Role | Host path | In the container | Mode |
| --- | --- | --- | --- |
| State | `$DISK/appdata/backup-manager/state` | `/data/state` | rw |
| Backups | `$DISK/backups/backup-manager` | `/data/backups` | rw |
| Config | `$DISK/appdata/backup-manager/config` | `/etc/backup-manager/config` | rw |
| SSH key | `$DISK/appdata/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | ro |
| Known hosts | `$DISK/appdata/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | ro |

`config` is a writable **directory** holding `config.yaml`, not a read-only single
file (issue #196). Adding a backup set, saving settings and first-run setup all
replace that file through a temp file created in its own directory, and the engine
keeps `ssh_keys/` and `known_hosts.d/` beside it, so a single-file mount silently
disables all three. It may be empty on a fresh install. The SSH key and
`known_hosts` stay read-only single files: nothing in the container writes those.


`DISK` is the only variable in any of these, and it is the only line of
`backup-manager.env` you have to change. The compose file writes every host path
as `${DISK:?...}/...`, so an unset or misspelled `DISK` stops the deployment
rather than creating five directories somewhere plausible. There is deliberately
no per-path variable: five knobs whose values all repeated the same placeholder
is how the UUID ended up needing five substitutions while the documentation
promised one.

Appdata holds private state; `$DISK/backups/backup-manager` holds retained
artifacts. That split is a rule rather than a preference: §19.2 makes them
separate security domains, and the backup root must never contain SSH private
keys or authentication state. `distribution/packaging` checks the containment in
both directions on every commit.

The backup root is a directory of the app's own inside `$DISK/backups`, not that
directory itself, so installing and removing this profile only ever touches paths
it created, even when you already keep other things there.

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
credential, and `distribution/packaging` scans for one on every commit.

## config.yaml

The engine no longer needs a configuration to start: as of issue #176 an instance
with no `config.yaml` serves a first-run setup flow in the web UI that writes one
for you. A config file that EXISTS and does not validate is still a hard start
failure, deliberately, because replacing a configuration somebody already wrote
is worse than refusing.

That flow does not reach this package yet, and the reason is the mount, not the
engine: `config.yaml` is bind-mounted here as a single **read-only file**, so the
container cannot create it, and a bind mount cannot express "not there yet"
either. Until that becomes a writable directory, write the file before
installing.

See `apps/truenas/README.md` for an annotated example; the only difference here
is the host path it lives at. The container-side paths in it are fixed by this
profile and must not be changed.

## The image reference

The registry is settled and nothing has been pushed to it yet, so
`ghcr.io/spdrman/backup-manager:0.1.0` is the intended publish target rather than
something that resolves today. `distribution/packaging/canonical.json` is the
single source of truth for the reference and records `image.published: false`,
and `container/release-manifest.json` carries a `registry_digest` of `null` per
architecture for exactly as long as that stays false. Step 0.2 of the acceptance
procedure covers pushing to your own registry or side-loading a saved image in
the meantime. It is the `IMAGE` variable in the env file, and nothing else.
