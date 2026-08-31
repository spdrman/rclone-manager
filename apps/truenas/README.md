# TrueNAS provider package

Tier B (§4A): a provider package/catalog wrapper. TrueNAS gets no lifecycle
engine and no plugin. Everything in this directory is metadata pointed at the
exact canonical OCI image, so a TrueNAS install runs byte-identical software to a
generic Docker install, a UGOS install, or an Unraid install.

**Status: build-supported and uncertified (§68).** Nothing here has been installed
on a TrueNAS system. Run
[docs/acceptance/truenas-provider-acceptance.md](../../docs/acceptance/truenas-provider-acceptance.md)
to change that.


## Converted to a thin adapter (issue #169)

Everything here is now **derived** from the one authoritative Compose runtime
definition rather than authored beside it. `distribution/packaging`'s
derivation gate holds this platform's image reference, runtime profile,
mounts, published port, health check and architectures to `canonical.json`,
and a deliberate mismatch fails the build naming the field that drifted. What
that means in practice for this directory:

- both services select the `truenas` runtime profile with `--profile=`, so
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

## What is here

| Path | What it is |
| --- | --- |
| `compose/backup-manager.yaml` | The custom-app deployment. Paste it into Apps, Discover Apps, Custom App, Install via YAML. Usable today. |
| `catalog/app.yaml` | Catalog entry metadata: title, version, categories, icon, sources, run-as context. |
| `catalog/questions.yaml` | The install wizard: image reference, five storage paths, the published port, and the uid/gid. |
| `catalog/ix_values.yaml` | A default for every question. |
| `catalog/templates/docker-compose.yaml` | What the catalog renders. The same two containers as `compose/backup-manager.yaml`, with the answers substituted. `distribution/packaging` renders it against `ix_values.yaml` on every commit and puts the result through every rule the paste-in compose file gets: the canonical image, the five storage roles and their host paths, read-only mounts, the single published port, the commands, and the full hardening set. The template stays loop-free and conditional-free so that stays possible. |
| `frontend/platform.ts` | The shared platform bridge (§3.5). Provider identity and storage expectations only, no lifecycle behaviour. |

There is deliberately no fourth thing. No Go, no shell, no install hook, no
TrueNAS-specific service. `distribution/packaging` fails the build if any appears.

## Two containers, one image

`backup-manager` runs `/backup-manager-web serve`: local authentication, the
versioned `/api/v1` API and the backup scheduler in one process sharing one
shutdown context. It holds the state database and the credentials, and it
publishes no port.

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

| Role | Host default | In the container | Mode |
| --- | --- | --- | --- |
| State | `/mnt/tank/backup-manager/state` | `/data/state` | rw |
| Backups | `/mnt/tank/backup-manager/backups` | `/data/backups` | rw |
| Config | `/mnt/tank/backup-manager/config` | `/etc/backup-manager/config` | rw |
| SSH key | `/mnt/tank/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | ro |
| Known hosts | `/mnt/tank/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | ro |

`config` is a writable **directory** holding `config.yaml`, not a read-only single
file (issue #196). Adding a backup set, saving settings and first-run setup all
replace that file through a temp file created in its own directory, and the engine
keeps `ssh_keys/` and `known_hosts.d/` beside it, so a single-file mount silently
disables all three. It may be empty on a fresh install. The SSH key and
`known_hosts` stay read-only single files: nothing in the container writes those.


`tank` is the pool name the defaults assume. Substitute yours.

The state, config and secrets paths sit outside the backup root, and that is a
rule rather than a preference: §19.2 makes private application state and the user
backup root separate security domains, and the backup root must never contain SSH
private keys or authentication state. `distribution/packaging` checks the
containment in both directions on every commit.

Create all five paths, and own them by the uid/gid you install with, before first
start. The runtime image is distroless: no shell, no root step, no init process,
so nothing inside the container can fix ownership for you.

## Authentication

Local accounts only (§13A), the same reusable local authentication the generic
Web host provides: first-run administrator enrollment through a single-use token
the engine prints to its own log, Argon2id password hashing, an HTTP-only session
cookie, CSRF protection and per-IP rate limiting.

TrueNAS accounts cannot log into Backup Manager and Backup Manager accounts cannot
log into TrueNAS. Nothing in this directory ships a credential, and
`distribution/packaging` scans for one on every commit.

## config.yaml

The engine calls `core/service.Open`, which loads **and validates** the config file
before the HTTP listener starts. A missing or invalid file is a hard start failure,
not a first-run wizard, so write it before installing. The container-side paths
below are fixed by this package and must not be changed:

```yaml
poll_interval: 1h
state:
  database: /data/state/state.db
sources:
  - id: example-source
    backup_sets:
      - id: nightly
        remote:
          type: sftp
          host: "sftp.example.internal"
          user: "backup"
          key:
            file: "/etc/backup-manager/id_ed25519"
          known_hosts: "/etc/backup-manager/known_hosts"
        remote_path: "/srv/backups"
        local_path: /data/backups
        include:
          - "*"
        completion:
          strategy: rename
        stale_after: 26h
retention:
  timezone: UTC
  week_starts_on: monday
```

`scripts/deploy/deploy_generic.py`'s `render_config_yaml` is the authoritative
shape; this is the same thing with TrueNAS's paths.

## The image reference

No registry is configured for this repository yet, so
`ghcr.io/spdrman/backup-manager:1.0.0` is the intended publish target rather than
something that resolves today. `distribution/packaging/canonical.json` records that
honestly, and step 0 of the acceptance procedure covers pushing to your own
registry or side-loading a saved image in the meantime. The reference is one
question in the wizard and one line in the compose file, so substituting it is a
one-place change.

## Contributing this to the TrueNAS catalog

Copy `catalog/` into the TrueNAS apps repository as
`ix-dev/community/backup-manager/` and run that repository's own validation and
render tooling. That validator cannot run here, so it is step 8 of the acceptance
procedure rather than a CI check. What CI does check on every commit: every
question is consumed by the template and given a default, the rendered image is
the exact canonical one, no path escapes the backup-root rule, and no lifecycle
code exists.
