# Unraid provider package

Tier B (§4A): a provider package/catalog wrapper. Unraid gets no lifecycle engine
and no plugin. Everything in this directory is metadata pointed at the exact
canonical OCI image, so an Unraid install runs byte-identical software to a
generic Docker install, a UGOS install, or a TrueNAS install.

**Status: build-supported and uncertified (§68).** Nothing here has been installed
on an Unraid system. Run
[docs/acceptance/unraid-provider-acceptance.md](../../docs/acceptance/unraid-provider-acceptance.md)
to change that.


## Converted to a thin adapter (issue #169)

Everything here is now **derived** from the one authoritative Compose runtime
definition rather than authored beside it. `distribution/packaging`'s
derivation gate holds this platform's image reference, runtime profile,
mounts, published port, health check and architectures to `canonical.json`,
and a deliberate mismatch fails the build naming the field that drifted. What
that means in practice for this directory:

- both services select the `unraid` runtime profile with `--profile=`, so
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
| `frontend/webui.json` | WebUI and storage facts, pinned to the templates by `distribution/packaging` so the two cannot drift. |

No Go, no shell, no install hook, no Unraid plugin. `distribution/packaging` fails
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
| Backups | `/mnt/user/backups/backup-manager` | `/data/backups` | rw |
| Config | `/mnt/user/appdata/backup-manager/config` | `/etc/backup-manager/config` | rw |
| SSH key | `/mnt/user/appdata/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | ro |
| Known hosts | `/mnt/user/appdata/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | ro |

`config` is a writable **directory** holding `config.yaml`, not a read-only single
file (issue #196). Adding a backup set, saving settings and first-run setup all
replace that file through a temp file created in its own directory, and the engine
keeps `ssh_keys/` and `known_hosts.d/` beside it, so a single-file mount silently
disables all three. It may be empty on a fresh install. The SSH key and
`known_hosts` stay read-only single files: nothing in the container writes those.


Appdata holds private state; a directory of the app's own inside the backups user
share holds retained artifacts. The backup root is deliberately a child of the
share rather than the share itself, because `backups` is one of the likeliest
names for a share you already use, and every step that creates, owns or later
inspects the backup root should only ever touch paths this package created. That
split is a rule rather than a preference: §19.2 makes them separate security
domains, and the backup root must never contain SSH private keys or authentication
state. `distribution/packaging` checks the containment in both directions on every
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
`distribution/packaging` parses this string into flags and checks both against one
another, in both directions: the five hardening flags have to be present, and
nothing on the same line may undo them. `--privileged`, `--cap-add`,
`--pid=host`, `--network=host`, `--device`, `--userns=host`, a
`seccomp=unconfined` security option and a `--user 0:0` are all red tests. That
matters more here than it looks: the template scanner reads the `<Privileged>`
element, so before the flags were parsed a `--privileged` appended to this line
was caught by nothing at all, and `--user` was satisfied by `--userns=host`.

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

The engine container keeps the image's baked-in healthcheck, and here that is the
right answer rather than the same limitation twice. On the compose profiles the
engine has to override it, because their Web UI will not start until the engine
reports healthy and `backup-manager status` is non-zero on a fresh install by
design. An Unraid template declares no start-ordering dependency at all, so
nothing here waits on that verdict and the badge Unraid shows for the engine is
exactly the backup-freshness report FR-24 means it to be: red until the first
backup lands, and red again if one goes stale. The derivation gate allows the
inherited check on this adapter for that reason and refuses it on every adapter
where something waits.

## Authentication

Local accounts only (§13A), the same reusable local authentication the generic Web
host provides: first-run administrator enrollment through a single-use token the
engine prints to its own log, Argon2id password hashing, an HTTP-only session
cookie, CSRF protection and per-IP rate limiting.

Unraid's root password cannot log into Backup Manager and Backup Manager's
administrator cannot log into Unraid. Nothing in this directory ships a credential,
and `distribution/packaging` scans for one on every commit.

### The one place this profile is weaker than the compose ones

The engine here does **not** set `TRUST_FORWARDED_HEADERS`, and the compose
profiles do.

The flag decides whether the engine believes `X-Forwarded-For` and
`X-Forwarded-Proto`. `apps/common/auth/local` allows it only where the Web UI
container is the engine's sole possible direct TCP peer "by network topology, not
merely by convention". A compose project network is created, named and destroyed
with the deployment, and nothing else joins it. The `backup-manager` network
these two templates share is not that: you create it by hand, it outlives both
containers, it has a very reusable name, and every container on a user-defined
bridge reaches every port of every other container on it regardless of what is
published. Anything attached to it later, deliberately or by an unrelated app
reusing the name, would be a direct peer of the engine on 8080 and could rotate
`X-Forwarded-For` per request to defeat the login, enrollment and password rate
limiters, and assert `X-Forwarded-Proto: https` over plaintext.

The cost of leaving it off is real and worth stating: every client is then
counted against one rate-limit bucket, the Web UI container's own address, so a
brute-force attempt from one machine can lock out the rest. That is
over-limiting rather than not limiting, which is the fail-safe direction, and it
is the same call the Synology package makes.

`canonical.json` records the decision per platform in `trustForwardedHeaders`
with the reason beside it, and the conformance suite pins each profile to that
record in both directions: an engine that starts trusting the header here, or
stops trusting it under compose, is a red test. No profile may ever set it on the
Web UI container, which is the internet-facing edge.

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
package and must not be changed.

## The image reference

`ghcr.io/spdrman/backup-manager:0.1.0` is real: it is published, keyless-signed,
and SBOM-attested (`distribution/packaging/canonical.json` records
`image.published: true`, and `container/release-manifest.json` carries a real
`registry_digest` per architecture). Pull it directly; the acceptance
procedure's own step about pushing or side-loading a build is only needed for a
locally-built image, not the released one. It is one `<Repository>` element per template, editable in Unraid's
own template editor.

## Community Applications

CA lists templates from a GitHub repository its feed indexes, which is a
submission step CA maintainers control and nothing here can perform. Step 9 of the
acceptance procedure covers it. `<TemplateURL>`, `<Project>`, `<Support>`,
`<Icon>`, `<Overview>` and `<Category>` are all populated for that submission.
