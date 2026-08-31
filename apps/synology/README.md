# Synology DSM provider app

Issue #85 / work package B4.4, `docs/EPIC-B-multi-nas.md` §72, the
Synology entry in §4A, and D-5 in §5.

Synology is the one platform in Phase 4 that cannot consume the canonical
OCI image: DSM's Package Center installs a native `.spk`. §3.7's release
hierarchy puts the SPK as a sibling of the OCI image under the same root,
carrying "the exact same core binary digest", so everything here is a
wrapper around release binaries that were built elsewhere.

Nothing in this directory compiles the product. `spkctl build` wraps the
two release executables; `spkctl verify` reads a finished package back and
re-derives their SHA-256 against `container/release-manifest.json`.

## Supported architectures and models

| SPK | INFO `arch` | Go target | DSM platforms | Status |
|---|---|---|---|---|
| `BackupManager-x86_64-<version>.spk` | `x86_64` | `linux/amd64` | apollolake, avoton, braswell, broadwell, broadwellnk, broadwellntb, broadwellntbap, bromolow, cedarview, coffeelake, denverton, geminilake, grantley, kvmx64, purley, skylaked, v1000 | build-supported, **uncertified** |
| `BackupManager-armv8-<version>.spk` | `armv8` | `linux/arm64` | rtd1296, armada37xx, rtd1619, rtd1619b | build-supported, **uncertified** |

Family names and members come from Synology's Appendix A platform/arch
mapping table, and are pinned in `spk/arch.go` so the table above and the
package's own validation cannot drift apart.

**Not supported, and not installable:** `i686` (evansport), `armv7`
(alpine, alpine4k) and `armv5` (628x). The canonical release builds no
32-bit binary, so there is nothing honest to package for them. DSM
refusing the install on those units is the correct outcome.

"Uncertified" is §68's own word. A provider/architecture stays
build-supported but uncertified until its acceptance procedure has been
executed on real hardware, and §68 requires a representative DSM 7.x model
**per claimed architecture** - so certifying both rows needs two machines.
The procedure is
[`docs/acceptance/synology-dsm-package-lifecycle.md`](../../docs/acceptance/synology-dsm-package-lifecycle.md).

Minimum DSM is `7.0-40314`, which is where the package FHS documents
`/var/packages/<pkg>/var` as available. Everything this package must keep
across an upgrade lives there.

## Building a package

The release binaries are the two executables inside the canonical OCI
image, and they are extracted exactly the way
`scripts/release/record-release-hashes.sh` extracts them to produce the
manifest in the first place:

```sh
mkdir -p release/amd64
cid=$(docker create --platform linux/amd64 backup-manager:<version> /backup-manager version)
docker cp "${cid}:/backup-manager"     release/amd64/backup-manager
docker cp "${cid}:/backup-manager-web" release/amd64/backup-manager-web
docker rm "${cid}"
```

The package also carries this provider's own UI bundle (issue #180,
packaged by #169), which is built from the shared UI rather than extracted
from the image:

```sh
cd ui/shared && npm ci && npm run build:bundles synology
```

Then:

```sh
cd apps/synology
go run ./cmd/spkctl build --arch amd64 --version 1.0.0-1 \
    --binaries ../../release/amd64 \
    --ui-bundle ../../ui/shared/dist-bundles/synology \
    --out ../../dist
go run ./cmd/spkctl verify --spk ../../dist/BackupManager-x86_64-1.0.0-1.spk \
    --manifest ../../container/release-manifest.json
```

`--ui-bundle` is required, not optional. A package built without one would
install, run, and show the generic Docker bridge on a Synology NAS, and
nothing about the finished package would say so. `build` also refuses a
bundle whose `bundle.json` names another provider, and one with no
`index.html` in it.

`verify` exits non-zero unless all ten checks pass. Run it before handing
anyone a package, and keep its output with the release: it is the evidence
for the "SPK contains the exact release core binary hash" criterion.

Never rebuild the binaries here. A rebuild is precisely what `verify`
exists to detect, and it will detect it - a build of the same source at a
different commit, or with different flags, produces a different digest and
fails the parity check.

## Two ways to install this release on DSM, and how to choose

Since issue #169 there are two, and neither replaces the other.

| | `.spk` (this directory's `spk/`) | Container Manager project (`compose/`) |
|---|---|---|
| What it installs | the two native release binaries | the canonical OCI image, two containers |
| Where it appears | Package Center, with a DSM desktop launcher | Container Manager → Project |
| State lives in | `/var/packages/BackupManager/{etc,var}` | host paths you choose under `/volume1` |
| Engine isolation | loopback bind, enforced by `start-stop-status` | a compose project network, enforced by topology |
| Trusts forwarded headers | no, because any local process can reach loopback | yes, because only the Web UI container can reach the engine |
| Needs Container Manager installed | no | yes |
| Signed | no (Trust Level must allow any publisher) | not applicable |

Take the `.spk` if you want the DSM desktop launcher and Package Center
lifecycle. Take the Container Manager project if you would rather run the
same image every other platform runs, or you already keep your containers
there. The Container Manager path is also the one EPIC B's support table
names for Synology; the `.spk` predates it and is not being retired, which
is a product decision and not this issue's to make.

`compose/backup-manager.yml` and `compose/backup-manager.env` are the
project. Container Manager → Project → Create → "Create docker-compose.yml"
takes the first, and the environment field takes the second. Read
`compose/backup-manager.env` before pasting: two paths in it are yours to
set, and the compose file refuses to start rather than inventing either.
The two installs can run side by side while you compare them, because the
`.spk` publishes 8477 and the project defaults to 8080.

## Installing the `.spk`

Manual install only, through Package Center → Manual Install.

The package is **unsigned**, so DSM refuses it until Package Center →
Settings → Trust Level is set to "Any publisher". Signing requires a
Synology-issued key this project does not hold; publishing through the
Package Center catalogue is a separate exercise with its own review.

## What runs, and where

Two processes, both the same unmodified `backup-manager-web` release
binary, differing only in their command - the same "one artifact, vary
command" split `container/compose.yaml` already ships for the generic
Docker app:

| Process | Command | Listener |
|---|---|---|
| engine | `backup-manager-web serve` | `127.0.0.1:8478`, loopback only |
| web UI | `backup-manager-web serve-ui` | `:8477`, the only LAN-facing port |

Authentication is the reusable `local-auth` from the generic Web host.
There is no DSM-specific auth path anywhere in this directory. Native DSM
SSO and session integration are explicitly a follow-on behind their own
security gate (§4A, §72 WP 4.4).

`--trust-forwarded-headers` is deliberately **off** here, unlike the
Docker deployment. Compose can enable it because the engine sits on a
private network whose only other member is the UI container; a loopback
bind on a NAS is reachable by any local process, so a forwarded header
here is not attributable to one verified peer.

### Filesystem

| Path | Holds | Upgrade | Uninstall |
|---|---|---|---|
| `/var/packages/BackupManager/target` | the two binaries, the DSM UI files, the config seed | replaced | removed |
| `/var/packages/BackupManager/etc` | `config.yaml`, and the SSH key/known_hosts you put there | kept | kept |
| `/var/packages/BackupManager/var` | SQLite journal, `local-auth.json`, logs, pid files | kept | kept |
| `/volume?/backup-manager` | backup data (a DSM shared folder) | kept | kept |

Both daemons' logs live under `var/log`, on the DSM system volume, and
`var/` survives every upgrade and reboot. `common.sh` caps each at
`LOG_MAX_BYTES` (8 MiB) and keeps exactly one older generation, enforced
on start and on every `status` poll DSM makes. The cap is a copy followed
by a truncation rather than a rename, because the daemons hold their log
open and would keep filling a renamed file.

`conf/privilege` sets `run-as: package`, and the package contains **no
`chown`**. `postinst` creates `var/{state,log,run}` and records the uid it
created them as in the install log. Whether that uid is the one the
daemons run as is a hardware question: steps 1.7 and 2.8 of the acceptance
procedure record it, and a `chown` belongs after that answer, not before
it. Under `set -e` a `chown` to a wrongly derived user name would turn a
possible start failure into a certain install failure.

The last row is a `data-share` resource worker, chosen specifically
because Synology documents that such a shared folder "will not be removed
after package uninstallation, since it might delete the user's personal
data as well". `postuninst` deletes nothing at all, and
`spk.ScanForUnsafeDeletes` fails the build if any shipped script grows a
deletion whose target is not provably inside the package's own footprint.

### First start

A fresh install does not start. `postinst` seeds
`etc/config.yaml` with an empty `sources` list, and the core refuses to
run on a configuration it cannot validate - the same behaviour the generic
Docker deployment has, not a Synology quirk. Edit the file, name at least
one source, then start the package. `start-stop-status` tails the engine
log into Package Center's own failure message so the reason is visible
there rather than only in a log file.

No credential of any kind ships in this package. The SSH private key and
the pinned `known_hosts` are paths you populate on the NAS; the seeded
config points at them and nothing more. `spkctl verify`'s bundled-secret
scan reads every file in both archives on every build.

## Known gaps

1. ~~**The embedded UI is the generic provider bridge.**~~ **Closed by
   issues #167 and #169.** It used to be true, and it used to be
   unavoidable: `serve-ui` served a bundle compiled into the release
   binary through `go:embed` with no flag to serve one from disk, so
   shipping the Synology bridge would have meant a Synology-specific
   binary, which §3.7 forbids. #167 made the bundle a run-time choice
   (`--ui-dir`), and #169 packaged it: the `.spk` now carries
   `ui/shared/dist-bundles/synology` in its payload and
   `start-stop-status` serves it, with the release binary unchanged.
   `spk.Build` refuses a package built without that bundle, or with one
   built for another provider, so the failure cannot come back quietly.
2. **No `port-config` in `conf/resource`.** The package's ports therefore
   do not appear in DSM's firewall application selector. It is a
   convenience, and a wrong resource-worker spec fails an install in a way
   nothing here can test, so it is left out until an operator with
   hardware can add it against a real Package Center.
3. **The DSM desktop launcher opens the UI over plain HTTP.** INFO
   declares `adminprotocol=http` and nothing in this package terminates
   TLS, so `ui/index.html` navigates to `http://<nas>:8477/` literally
   rather than mirroring the DSM session's scheme. From an HTTPS DSM
   session (DSM 7's default on 5001) that is a top-level https-to-http
   navigation, which browsers permit outside HTTPS-Only mode, and it is
   one plaintext hop on the LAN. Mirroring the scheme instead would send
   the browser into a TLS handshake against a plain HTTP listener, which
   fails every time. Closing this properly means terminating TLS in the
   UI host, which is a change to the shared Web host rather than to this
   package.
4. **The package is unsigned** (see Installing).
5. **Native DSM SSO is not implemented** and is deliberately out of scope.
6. **Every hardware-dependent acceptance criterion is open.** See the
   acceptance procedure for exactly which, and what executing them needs.

## Layout

```
apps/synology/
├── cmd/spkctl/       build and verify commands
├── compose/          the Container Manager project (#169) and its environment
├── frontend/         the Synology platform bridge for ui/shared (pre-existing)
└── spk/
    ├── arch.go       the claimed architectures and their DSM platforms
    ├── build.go      the packer: pkg_make_spk's documented behaviour, in Go
    ├── verify.go     the ten conformance checks
    ├── manifest.go   container/release-manifest.json
    ├── secrets.go    the bundled-secret scan
    ├── lifecycle.go  the unsafe-delete scan
    ├── icon.go       the Package Center and launcher icons, drawn in code
    └── assets/       everything shipped verbatim: scripts, conf, ui, config seed
```
