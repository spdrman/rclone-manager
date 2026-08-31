# Unraid provider package

Tier B (§4A): a provider package/catalog wrapper. Unraid gets no lifecycle engine
and no plugin. Everything in this directory is metadata pointed at the exact
canonical OCI image, so an Unraid install runs byte-identical software to a
generic Docker install, a UGOS install, or a TrueNAS install.

**Status: build-supported and uncertified (§68).** Nothing here has been installed
on an Unraid system. Run
[docs/acceptance/unraid-provider-acceptance.md](../../docs/acceptance/unraid-provider-acceptance.md)
to change that.

## Two templates, and why

An Unraid Docker template describes exactly one container. The canonical image
needs two: `/backup-manager-web serve` (the engine: API, scheduler, local
authentication, no published port) and `/backup-manager-web serve-ui` (the static
UI plus a reverse proxy, the only published port). There is no single command that
does both, by design, so this package ships two templates.

| Path | What it is |
| --- | --- |
| `template/backup-manager.xml` | The engine. Install first. |
| `template/backup-manager-ui.xml` | The web interface. Install second. |
| `frontend/platform.ts` | The shared platform bridge (§3.5). Provider identity and storage expectations only. |
| `frontend/webui.json` | WebUI and storage facts, pinned to the templates by `apps/common/packaging` so the two cannot drift. |

No Go, no shell, no install hook, no Unraid plugin. `apps/common/packaging` fails
the build if any appears.

## The one prerequisite

```bash
docker network create backup-manager
```

Both templates target that user-defined network. The Web UI container reaches the
engine by container name, and Docker's embedded DNS resolves container names only
on a user-defined network, never on the default `bridge`. Skip this and the UI
starts, serves the static bundle, and 502s on every API call. Both templates say
so in `<Requires>`.

## Installing

Copy each template to `/boot/config/plugins/dockerMan/templates-user/` on the
flash drive, then add each container from the **user templates** section of
Docker, Add Container.

## Storage

| Role | Host default | In the container | Mode |
| --- | --- | --- | --- |
| State | `/mnt/user/appdata/backup-manager/state` | `/data/state` | rw |
| Backups | `/mnt/user/backups` | `/data/backups` | rw |
| Config | `/mnt/user/appdata/backup-manager/config/config.yaml` | `/etc/backup-manager/config.yaml` | ro |
| SSH key | `/mnt/user/appdata/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | ro |
| Known hosts | `/mnt/user/appdata/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | ro |

Appdata holds private state; the backups user share holds retained artifacts. That
split is a rule rather than a preference: §19.2 makes them separate security
domains, and the backup root must never contain SSH private keys or authentication
state. `apps/common/packaging` checks the containment in both directions on every
commit, and it is what makes the removal criterion meaningful, since Unraid leaves
appdata alone when a container is removed.

Create all five paths and own them by `99:100` (or whatever uid/gid you set in
`ExtraParams`) before first start. The runtime image is distroless: no shell, no
root step, no init process, so nothing inside the container can fix ownership for
you.

## Hardening lives in ExtraParams

Unraid's template schema has no element for a read-only rootfs, dropped
capabilities, `no-new-privileges`, a tmpfs, or a non-root uid/gid, so all five go
through `<ExtraParams>`:

```
--read-only --cap-drop=ALL --security-opt=no-new-privileges:true --tmpfs /tmp:... --user 99:100
```

The compose profiles express the same settings as first-class keys.
`apps/common/packaging` checks both against one another, so the two cannot drift
apart quietly.

`<PostArgs>` carries the container command. That is unusual for an Unraid template
and it is load-bearing here: the canonical image ships no `ENTRYPOINT` and no `CMD`
on purpose, because no single default would be right for both of its binaries, so a
template with an empty `<PostArgs>` would not start at all. The conformance suite
holds `<PostArgs>` to the same rule a compose `command:` gets: a canonical binary,
and nothing that would need a shell the distroless image does not have.

## The Web UI container has no healthcheck

Deliberately. The image bakes in `HEALTHCHECK /backup-manager status`, which needs
a config file and a state database that container does not have, so left inherited
it would report unhealthy forever while working perfectly. The compose profiles
override the test with `/backup-manager-web healthcheck`. Unraid's only seam is
`docker run`'s health-cmd flag, which is shell form, and the runtime image is
distroless with no shell, so an override there would be a healthcheck that can
never pass. Turning it off is honest; a permanently failing one is not.

## Authentication

Local accounts only (§13A), the same reusable local authentication the generic Web
host provides: first-run administrator enrollment through a single-use token the
engine prints to its own log, Argon2id password hashing, an HTTP-only session
cookie, CSRF protection and per-IP rate limiting.

Unraid's root password cannot log into Backup Manager and Backup Manager's
administrator cannot log into Unraid. Nothing in this directory ships a credential,
and `apps/common/packaging` scans for one on every commit.

## config.yaml

The engine calls `core/service.Open`, which loads **and validates** the config file
before the HTTP listener starts. A missing or invalid file is a hard start failure,
not a first-run wizard. See `apps/truenas/README.md` for an annotated example; the
only difference here is the host path it lives at. The container-side paths in it
are fixed by this package and must not be changed.

## The image reference

No registry is configured for this repository yet, so
`ghcr.io/spdrman/backup-manager:1.0.0` is the intended publish target rather than
something that resolves today. `apps/common/packaging/canonical.json` records that
honestly, and step 0 of the acceptance procedure covers pushing to your own
registry or side-loading a saved image in the meantime. It is one
`<Repository>` element per template, editable in Unraid's own template editor.

## Community Applications

CA lists templates from a GitHub repository its feed indexes, which is a
submission step CA maintainers control and nothing here can perform. Step 9 of the
acceptance procedure covers it. `<TemplateURL>`, `<Project>`, `<Support>`,
`<Icon>`, `<Overview>` and `<Category>` are all populated for that submission.
