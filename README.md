<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="rclone-manager mark: a broken ring standing for a transfer cycle in progress, next to the rclone-manager wordmark" width="240">
  </picture>
</p>


A backup lifecycle manager for a NAS. It pulls completed backup artifacts off a remote
server over SFTP, verifies them, commits them durably, and only then deletes the remote
copy.

It is a standalone Go binary that **embeds pinned rclone Go packages**. It does not fork
rclone, and it does not shell out to the `rclone` CLI for normal data movement.

**If you're here because a backup didn't arrive and it's 3am:** skip to
[Recovery](#recovery-when-a-backup-did-not-arrive) below, or go straight to
[`docs/recovery.md`](docs/recovery.md).

## The rule everything else serves

> A remote backup artifact MUST NOT be deleted until a verified and durably committed NAS
> copy exists. If state is uncertain, preserve the remote copy.

Every section below is either explaining how that rule is enforced or admitting where the
enforcement doesn't exist yet.

## Status: what actually runs today

Read this before the rest of the document, because it changes how to read everything else.
I rewrote this section against the code at the commit this branch was cut from, not against
the specification and not against the previous draft, and I made the parts of it that a
machine can decide into tests rather than sentences (see
[How this document is kept honest](#how-this-document-is-kept-honest)).

### The engine and the CLI are real

`core/` is a working backup engine with a working command line. `backup-manager` registers
fifteen commands, and the list below is checked against the dispatch table in
`core/cmd/backup-manager/main.go` on every run of the gate, so it cannot quietly go stale
the way its predecessor did.

<!-- BEGIN CLI-COMMANDS -->

| Command | What it does |
|---|---|
| `run` | perform one processing cycle and exit |
| `daemon` | repeat the processing cycle at `poll_interval` |
| `check` | validate config and the state database, then exit |
| `status` | report process and backup-set health (FR-24), exiting non-zero unless every set is HEALTHY |
| `sources` | list configured sources and backup sets |
| `backup-set` | `backup-set create <source/backup-set>` creates one, through the same service layer `POST /api/v1/backup-sets` uses, and writes this deployment's first configuration when there is none yet (issue #356). `backup-set patch <source/backup-set> [flags]` changes one in place, and only the flags you pass are changed (issue #350). `backup-set remove <source/backup-set>` takes one out of the configuration; the backups it collected stay on storage and stay listed by `artifacts`, and creating the set again with the same source and name takes them back (issue #391) |
| `artifacts` | list journal artifacts, optionally filtered by `--source` and `--backup-set` |
| `fetch` | run one backup set's cycle on demand |
| `retention` | preview GFS and last-known-good retention decisions, with per-run policy overrides |
| `reconcile` | run FR-17 reconciliation for every backup set |
| `validate` | re-check one artifact's durable local copy |
| `catalog` | `catalog rebuild` reconstructs a lost or corrupted state database from the sidecar recovery manifests |
| `quarantine` | act on one quarantined artifact: `revalidate`, `retry`, or `reinstate` (issue #277) |
| `settings` | report the live retention/capacity settings, or `settings patch` to change one in place (issue #277) |
| `backup-set` | `backup-set retention <source/set>` reports which retention policy that set is retained under and where it came from, gives the set a whole policy of its own, or `--inherit` takes that policy back off (issue #333) |
| `restore` | `restore <source/backup-set/artifact> --medium M [--days N] --acknowledge` asks the storage provider to make one archived copy readable again (EPIC E, FR-34). `--acknowledge` is required rather than a `--force` to skip, because a restore is billed and takes hours; `--days` defaults to 7 and is bounded to 1 to 30. `artifacts <id>` lists which medium each copy is on (issue #241) |
| `version` | report the binary, Go and embedded rclone versions |

<!-- END CLI-COMMANDS -->

Every command except `version` takes `--config`, defaulting to
`/etc/backup-manager/config.yaml`. `backup-manager` with no arguments prints that same list
and exits 2.

The lifecycle engine, the SQLite journal, discovery, verification, durable commit, remote
delete with TOCTOU protection, GFS retention, last-known-good protection, local prune,
reconciliation, disk capacity guards, scheduled revalidation, sidecar recovery manifests,
quarantine reporting, proactive alerting, health computation and structured logging are all
real, implemented, unit- and integration-tested Go packages under `core/internal/` and
`core/service/`. Each of them is now reachable from the command line, from the web host, or
from both, which is the gap the previous version of this section described honestly and
which has since been filled.

### The API and the web UI meet in the middle

`apps/common/webhost` serves a versioned `/api/v1`, authenticated, CSRF-protected, with a
destructive-operation gate in front of anything that can destroy data.
`apps/generic/cmd/backup-manager-web` is the binary that hosts it: `serve` runs the engine,
the scheduler and the API in one process, and `serve-ui` serves the built static UI and
reverse-proxies the API to it. `ui/shared` is the React application both of them exist for.

Until #211 the browser client asked for fourteen `(method, path)` pairs that neither
`api/v1/openapi.json` nor `apps/common/webhost/router.go` had, so against a real backend
the dashboard, the backups list, the activity feed and the quarantine page all failed
outright with "The backup service returned an unexpected response." Every suite in the
repository stayed green while that was true, for the reason the next paragraph gives.

Four of those were the wrong path for an operation that existed, and are now the right one.
The other ten are real surfaces, added spec-first (contract, regenerate, then handlers) over
reads core has always computed and `backup-manager artifacts`, `status` and `catalog`
already print: the backups list and one backup, the activity feed over the append-only
lifecycle record, quarantine plus its revalidate, retry and reinstate actions, the operations list,
enabling and disabling a backup set, the FR-24 health verdict, and catalog scan and rebuild.

**What keeps it that way is a check, not this paragraph.**
`scripts/api/check-client-paths.sh` reads `ui/shared/src/api/client.ts` statically, reduces
every request path it builds back to a `(method, path)` pattern, and requires each one to be
an operation the contract declares. It has no allowlist, it fails closed (a path it cannot
reduce, or a client method whose request it cannot find, is a failure rather than a skip),
and `scripts/ci-local.sh` runs it on every commit. Ten of `scripts/api/selftest.sh`'s
mutation controls exist to prove it can actually fail.

The same drift had been recorded before, exactly, in
`ui/shared/src/api/contract.conformance.test.ts`'s own list of unserved paths, described
there as "recorded debt, not an exemption mechanism". That description was accurate and the
suite was still green: an allowlist asserted exactly is a gate reporting the drift it was
built to catch as a pass. The list is empty now and says why it must stay so.

**The Playwright suite is still not evidence about the API, because it never talks to the
runtime.** `ui/shared/src/app/createApp.tsx` substitutes `createMockApi` whenever
`import.meta.env.DEV` is set, and the browser suite runs against
`npm run dev`. The mock implements whatever the client asks for, which is precisely why it
was green throughout. The suite is a real test of what the browser renders and of how the
pages behave, and it is no evidence at all about the API. Do not read a green e2e run as an
end-to-end proof.

That is no longer only an argument. Suite C in `rclone-manager-tests` boots the real
engine, serves the production bundle in front of it and drives the real pages, and four of
the six pages cannot load: `ui/shared/src/api/client.ts` requests fourteen `/api/v1`
operations that are in neither `api/v1/openapi.json` nor `apps/common/webhost/router.go`.
Written up as #211. The contract gate does not catch it because it compares the generated
bindings, and `client.ts` is hand-written on top of them.

### What is built but not exposed

- `core/internal/metrics` renders an already-computed health report as Prometheus text
  exposition format, and nothing imports it. There is no `/metrics` endpoint on any
  listener. `docs/adr/0002-phase-5-scope.md` is the reasoning for stopping there.
- Restore execution is out of scope by design, not by omission: there is no `restore`
  command and no restore endpoint. [Recovery](#recovery-when-a-backup-did-not-arrive) below
  is the manual procedure, and it is the whole of it.
- A release build does select a provider frontend, since #167 and #169. `serve-ui`
  resolves its bundle at run time (`--ui-dir`, then `--ui-root/<profile>`, then the
  compiled-in one) and fails to start rather than falling back to the generic bridge, and
  the canonical image carries the five provider bundles the shipped adapters name. What is
  still not exposed is a sixth: the image budget has room for these five and not another,
  and `ugos` carries its own in EPIC D's UPK, which does not exist here.
- This bullet used to say `serve` refuses to start without a valid config file and that an
  app-store install could not reach a setup screen. #176 fixed that (merged as #195):
  `core/service.Open` still refuses a config file that exists and does not validate, but no
  config file at all is read as a fresh install, and `serve` serves the first-run setup flow
  from `core/service.FirstRun` instead of exiting. The CLI has never had this problem in the
  first place, since it has no server to start: see
  [Doing everything from the CLI](#doing-everything-from-the-cli-issue-277) below for why
  "no config file yet" is not a distinct first-run case there at all.
- A packaged container can write its own configuration, since #169 carried #196's
  mount-role change: every adapter now bind-mounts the config DIRECTORY, writable, at
  `/etc/backup-manager/config` rather than the single `config.yaml` read-only. The three
  merged write paths that go through that file (creating a backup set, saving settings,
  first-run setup) reach a writable filesystem in a packaged container. What an operator
  still has to do by hand is make the host directory writable by the container's uid/gid
  before the first start: a bind mount does not chown its source, and the runtime image is
  distroless with no root step, so each acceptance procedure's step 0 says so.

### Doing everything from the CLI (issue #277)

The requirement is plain: everything must be doable from the CLI, and the Web UI must be
completely optional. This section is #277's own investigation, confirmed by actually
running each of these against a real deployment rather than by reading the code, and it is
the answer for every capability that turned out to already exist. Two gaps #277 found real
and unreachable got their own CLI commands in the same change (`quarantine` and `settings`,
both documented in their own sections above); one gap turned out to need real new product
work and is out of scope here (see the bottom of this section).

**Creating a backup set is `backup-set create`, and changing one is `backup-set patch`.**
This paragraph used to say a create verb was unnecessary rather than missing, and that a
hand-edited `config.yaml` plus `check` was the CLI's answer to `POST /backup-sets`. Issue
#356 is what changed that reading: proving a fresh install can actually pull a backup means
saying what to back up over SSH with no browser anywhere, and "edit a YAML file by hand" is
the absence of a command written as though it were a feature.

```bash
backup-manager backup-set create production/postgres \
    --host db.example.internal --user backup \
    --ssh-key-file ./id_ed25519 --trust-host-key \
    --remote-path /srv/backups --local-path /volume1/backups/postgres \
    --completion-strategy rename --read-only
```

It calls the same `core/service.BackupService.CreateBackupSet` the API route does, so the
two surfaces cannot drift; `suites/equivalence` in `rclone-manager-tests` drives both over
identical work directories and compares what each persisted. `--ssh-key-file` imports the
key the same way `POST /ssh-keys` does, and `--trust-host-key` probes the host and prints
the fingerprint before trusting it, which is what the wizard's Verify-server step does.
`--known-hosts-line` is the alternative when the key is already known and trust on first use
is not wanted.

Writing `config.yaml` by hand still works and is still worth knowing
(`core/internal/config`'s own doc comments are the fullest explanation of the schema in this
tree, and `core/internal/config/testdata/full.yaml` is a worked, if terse, example).
However the file got written, the same loop checks it:

```bash
backup-manager check --config ./config.yaml      # validates the file and the state database
backup-manager sources --config ./config.yaml    # renders what was actually understood
backup-manager fetch --config ./config.yaml --source S --backup-set B --dry-run
                                                  # proves it really reaches the host (see below)
```

That is a create-and-verify loop with no browser in it: `validate` and `check` exist
specifically so a hand-edited file does not have to be trusted blind.

Changing a set that already exists is a different question, and until issue #350 the answer
was the same file edit, which meant opening an editor on the NAS itself. `backup-set patch`
is what replaces that:

```bash
backup-manager backup-set --config ./config.yaml patch production/postgres-primary \
  --remote-path /var/backups/postgresql --include "*.dump,*.tar.zst"
```

Only the flags you pass are changed; anything you leave out is left exactly as it is, which
is the same sparse contract `PATCH /api/v1/backup-sets/{source}/{set}` carries and the same
one the Web UI's per-box Save rests on. Both surfaces call the same service method, so they
cannot drift. The change is validated against the same `config.Validate` a hand-edited file
goes through at boot and written through the same atomic replace.

One thing to be plain about, because it is the same for `settings patch` and is easy to
assume otherwise: the hot reload is in-process. A change made through the API takes effect
immediately in the engine that served it, because that engine is also the thing running the
schedule. A change made by a separate `backup-manager backup-set patch` invocation writes
`config.yaml` and reloads that invocation's own view of it, and a `daemon` already running
in another process keeps using the configuration it loaded at start until it is restarted.
There is no config watcher and no SIGHUP reload in this build.

A set's name and source are deliberately not patchable: they key every journal row, artifact
id and recovery manifest the set has ever produced, so renaming one is a migration rather
than an edit.

Three fields that *are* patchable ask first, once the set has artifacts on record:
`--host`, `--remote-path` and `--local-path`. Together they are what "the data this set is
about" means, and the artifacts already on record stay with the set rather than moving with
them:

- A remote root pointed at a **different** dataset whose file names match ones already on
  record makes every candidate come back already-known. The cycle reports success, health
  stays green, and nothing is fetched. That is a backup that has silently stopped happening.
- Artifacts stored under the **old** `local_path` stop matching what retention computes for
  them, so retention refuses them rather than pruning them from then on, and `catalog
  rebuild` stops seeing them.

Neither destroys anything, and pointing the field back restores both, which is why this is
an acknowledgement rather than a refusal: an operator whose NAS got a new address, or whose
volume moved, has a real change to make. Add `--acknowledge-repoint` (or
`"acknowledge_repoint": true` on the API, or **Save anyway** in the Web UI) once the message
has been read. If the new location holds a *different* dataset, make it a separate backup
set instead. `--port` and `--user` are not in that list: neither changes which directory on
which machine holds the data.

**First-run setup is the identical answer, not a separate case.** `POST /system/first-run`
exists because the Web UI has no config file to read yet and needs an in-browser wizard to
produce its first one; `core/service.FirstRun.CreateInitialConfig`'s own doc says plainly
that it writes exactly the config a hand-edited file would, with retention and alerting left
at their zero values "exactly as a configuration nobody has edited yet should mean." The CLI
has never needed a first-run ceremony at all: `check`, `sources`, `fetch`, every other
command just reads `config.yaml`, whether that file is the first one ever written for this
deployment or the hundredth edit of an existing one. Write the file, run `check`; there is no
"unconfigured" state for the CLI to be in.

`backup-set create` holds that line rather than breaking it. On a machine with no
`config.yaml` it writes the first one, through the same `FirstRun.CreateInitialConfig` the
wizard's route calls, and `--state-database` names the journal that first configuration
points at (defaulting to `/data/state/state.db`, the packaged mount). An operator standing
at a freshly installed NAS therefore has one command to type, not a wizard to open, and the
two surfaces still reach the same code.

**Enabling or disabling a backup set is a config-file field.** `POST
/backup-sets/{source}/{set}/enabled` flips `config.BackupSet.Disabled`. Set `disabled: true`
(or remove the key, or set it `false`) in `config.yaml` and confirm it with `sources`, which
prints `status=enabled` or `status=disabled` for exactly this field.

**Testing a connection is `fetch --dry-run`, and it is a good one.** `POST
/backup-sets/test-connection` authenticates, verifies the host key and lists the remote.
`fetch --config ./config.yaml --source S --backup-set B --dry-run` does the same real
authenticate-and-list, against the exact transport code path a real cycle would use, and
prints every object it finds with its size, which is strictly more than the API route
returns. It only works for a backup set already in `config.yaml`; to check a *candidate*
before committing to it, add it to the file (nothing is destructive about an entry that is
merely present) and run `check` then `fetch --dry-run` against it, removing or fixing the
entry if it does not check out.

**Provisioning an SSH key and capturing a host key are already fully documented, in
`docs/ssh-setup.md`, and this is the missing cross-reference.** `POST /ssh-keys` exists so a
browser, which cannot write a file to the NAS's own disk, can hand backup-manager a pasted
private key over HTTP; an operator with a shell already has filesystem access and does not
need that indirection; `docs/ssh-setup.md`'s ["Generate a dedicated SSH key
pair"](docs/ssh-setup.md#1-generate-a-dedicated-ssh-key-pair) section is the CLI-native
answer: `ssh-keygen`, then point `config.yaml`'s `key.file` straight at the result. Likewise
`POST /ssh/host-key-probe` exists so that same browser can show a fingerprint before an
operator trusts it; `docs/ssh-setup.md`'s ["Capture the server's host key, verified, not
just trusted"](docs/ssh-setup.md#4-capture-the-servers-host-key-verified-not-just-trusted)
section is `ssh-keyscan` plus `ssh-keygen -lf` to verify the fingerprint out-of-band, which
is the identical outcome (a real, readable `known_hosts` file) through tools every NAS ships
with already.

**Quarantine actions and settings were the two gaps #277 found real, and both now have a
command.** See [Quarantine](#quarantine) above for `quarantine revalidate`, `quarantine
retry` and `quarantine reinstate`. `backup-manager settings` reports the live, resolved
FR-18/FR-19 retention policy and FR-21 capacity settings (the [CLI-COMMANDS](#status-what-actually-runs-today)
table above has both), and `backup-manager settings patch [flags]` changes one in place,
hot-reloaded the same way `PATCH /api/v1/settings` already is. A full retention tier-chain
replacement of the *deployment's* policy stays a config-file edit; every other retention and
capacity field is reachable through `settings patch` without a restart.

**A backup set's own retention policy is not a config-file edit either (issue #333).**
`backup-manager backup-set retention` shows which policy a set is retained under, gives the
set a whole policy of its own, and `--inherit` takes it back off. It is the same three
operations `GET`/`PUT`/`DELETE /api/v1/backup-sets/{source}/{set}/retention` expose and the
same three the Web UI draws, all through one method in `core/service`. See [One backup set
on its own retention policy](#one-backup-set-on-its-own-retention-policy).

**What is not covered here: authentication and account management.** `/auth/enroll`,
`/auth/login` and `/auth/password` are genuinely out of scope for a CLI wrapper, not merely
undocumented. They are `apps/common/auth/local`'s session/cookie/CSRF/rate-limit subsystem,
constructed fresh inside the running web server process (the single-use enrollment token
itself lives in that process's memory, not on disk), so there is no config file or
already-open state database a separate `backup-manager` invocation could act on the way
every command above does. An operator who never intends to use the Web UI never needs any of
this, since the CLI talks to `config.yaml` and the state database directly and never makes
an HTTP request at all. An operator who does want the Web UI available later still has no
way to provision that first administrator account without opening a browser at least once
today; that is real, and it is #322 rather than something built here.

### What has actually been exercised on real hardware

Nothing. Not one of the acceptance procedures in [`docs/acceptance/`](docs/acceptance/) has
been executed, because nobody working on this repository has a TrueNAS, Unraid,
OpenMediaVault, Synology or Proxmox VE machine to execute them on. The procedures are
written, reviewed and specific, and they are prose until somebody runs them.

[`docs/conformance/phase-4-matrix.md`](docs/conformance/phase-4-matrix.md) is the generated
record and it says the same thing from the other side: twenty cells across five providers
report `PENDING_OPERATOR`, which is that matrix's word for "the automated half held and the
hardware run has not happened". In section 68's own words, every one of those providers is
**build-supported and uncertified**. A green conformance matrix proves the packaging
metadata is well-formed and mutually consistent, and it proves nothing whatsoever about how
any of these platforms behaves.

The image itself has never been published either. `ghcr.io/spdrman/backup-manager` is the
settled target and `distribution/packaging/canonical.json` records `published: false`, so
that reference resolves to nothing today and every acceptance procedure opens with a step 0
covering how to make it resolvable in the meantime.

### There are no screenshots in this document

There should be, and issue #112 asks for them per provider. I did not add any, and I would
rather say so than ship something that looks like evidence and is not. `docs/assets/` holds
the two logo files and nothing else. The only screenshots I could produce from this tree
would be of the mock API in a dev server, which is exactly the kind of picture that makes a
reader believe a claim this document has just spent a section retracting. Real screenshots
need a running packaged deployment, and a running packaged deployment needs #196 and #166
first. Provider logos are a separate question and a trademark one, so they are the project
owner's call rather than mine.

## Installing it

### The canonical Compose runtime is the install path

There is one product here, not eleven. Every platform below wraps the same multi-architecture
OCI image and the same Compose topology, and the differences between them are host paths and
metadata formats. `container/compose.yaml` is that topology, and
[`docs/deployment.md`](docs/deployment.md) is the reasoning behind every setting in it.

Two services, one image. `rclone-manager` runs `/backup-manager-web serve`: the core service,
the scheduler, local authentication and `/api/v1`, in one process on one shutdown context,
with **no published port at all**. `web-ui` runs `/backup-manager-web serve-ui`: the static
UI plus a reverse proxy to the engine, and it is the only service with a LAN-facing port.
They meet on a private project-scoped bridge network, which is what makes the engine's
isolation a topology rather than a convention. `/backup-manager` (no `-web`) is the same
image's headless binary for a deployment that wants no web listener at all.

```bash
cd container
cp .env.example .env      # then edit: PUID/PGID, the host paths, LISTEN_PORT
docker compose up -d
```

Both containers run non-root as `PUID:PGID`, with a read-only root filesystem, all
capabilities dropped and `no-new-privileges`. The image has no shell and no init step, so
**the host paths have to exist and be owned by that uid/gid before the first start**;
nothing in the container will chown them for you.

### Which mount holds what, and why they are never the same directory

Every platform mounts three separate places for three different jobs, and conflating any two
of them is the mistake this section exists to prevent.

| Mount | Holds | Written by | `.env` key |
|---|---|---|---|
| Private application state | the SQLite journal and its `-wal`/`-shm` files | the app, constantly | `STATE_DIR` |
| Backup data | the retained artifacts and their sidecar recovery manifests | the app, on commit | `BACKUP_DIR` |
| Credentials and configuration | `config.yaml`, the SSH private key, the pinned `known_hosts` | you, out of band, read-only | `CONFIG_FILE`, `SSH_KEY_FILE`, `KNOWN_HOSTS_FILE` |

The SSH private key is the one that matters. It lives with the configuration, mounted
read-only, and it must **not** be inside the backup root: put it there and every backup of
that directory carries the key that can read and delete the source. The backup root on every
platform below is a dedicated child directory rather than a share you already use, for the
same reason.

`distribution/packaging/canonical.json` is the single source of truth for these paths, and
this repository's own test suite fails the build if any platform's metadata disagrees with
it.

### What "supported" means for each target

<!-- BEGIN SUPPORT-MODEL -->

| Target | Tier | What ships in this repository today | Where the paths are defined |
|---|---|---|---|
| Generic Docker and Linux | Tier C | the canonical image, `container/compose.yaml`, and `apps/generic`'s own Go module for the web host | `container/compose.yaml` |
| TrueNAS | Tier B | a custom-app Compose file plus a TrueNAS Apps catalog entry, metadata only | [`apps/truenas/README.md`](apps/truenas/README.md) |
| Unraid | Tier B | two Community Applications Docker templates, metadata only | [`apps/unraid/README.md`](apps/unraid/README.md) |
| Synology DSM | Tier B | a real `.spk` built by `apps/synology`, wrapping the release binaries unchanged and checking their digest against `container/release-manifest.json` | [`apps/synology/README.md`](apps/synology/README.md) |
| OpenMediaVault | Tier C | a Compose deployment profile, metadata only | [`apps/openmediavault/README.md`](apps/openmediavault/README.md) |
| Proxmox VE | Tier C | the same Compose profile for a dedicated container-host guest, metadata only | [`apps/proxmox/README.md`](apps/proxmox/README.md) |
| Portainer CE | Tier B | a version 3 App Template plus the Compose stack it deploys, metadata only | [`apps/portainer/README.md`](apps/portainer/README.md) |
| Dockge | Tier C | no packaging at all, by design: Dockge imports `container/compose.yaml` itself, and the deliverable is the workflow that keeps that true | [`apps/dockge/README.md`](apps/dockge/README.md) |
| CasaOS | Tier B | one `docker-compose.yml` carrying an `x-casaos` block, which is both the runtime definition and the store submission | [`apps/casaos/README.md`](apps/casaos/README.md) |
| ZimaOS | Tier B | the same `x-casaos` compose file again, for the CasaOS-derived store ZimaOS ships | [`apps/zimaos/README.md`](apps/zimaos/README.md) |
| UGREEN UGOS Pro | Tier A | the frontend bridge and nothing else: no `.UPK`, no packaging | EPIC D, issue #83 |

<!-- END SUPPORT-MODEL -->

The tiers come from `docs/EPIC-B-multi-nas.md`'s support-tier list, from `canonical.json`
for the four container profiles it declares, and from `conformance.json`, which declares all
seven targets with their tiers. The gate checks every row of this table against those two
files rather than trusting the table, and it checks in both directions: a row here that
neither file declares is a failure, and so is a target they declare that this table has
dropped.

Two things about the Proxmox row are worth saying out loud. Its paths are inside the guest,
not on the PVE host: the supported model is a dedicated container-host guest with one host
directory or dataset shared into it, and running the app on the PVE host itself is ruled
out. And the Unraid row is the one profile where the engine's isolation is weaker than the
others, because both Unraid templates join a durable, host-wide, generically named bridge
the operator creates by hand; every container on such a bridge can reach every port of every
other one, so the engine does not trust forwarded headers there and rate-limits on the
proxy's own address instead. `apps/unraid/README.md` says so too.

### What is deliberately not being built

EPIC B commits to a support model, and the deferrals are part of it. New Synology `.spk`
work, native DSM SSO, a native OpenMediaVault Workbench plugin, a Proxmox Web UI plugin, a
Portainer plugin or API extension, a Dockge plugin, a second application server for any
provider, provider-specific backup engines and provider-specific copies of the React
application are all explicitly out of scope unless one of them is later proven necessary.

The Synology line reads like a contradiction and is not one. #85 shipped an `.spk` in Phase
4 because Phase 4 shipped as written, and the deferral is about **new** `.spk` work; #169
adds a Container Manager Compose path alongside the shipped package rather than replacing
it. Retiring shipped packaging would be a product decision and nobody has made one.

Portainer, CasaOS, ZimaOS and Dockge appear in EPIC B's Phase 6 support model as targets
that get a documented deployment profile. None of them exists in this tree yet, so this
document does not list them as installable.

## Who owns what

rclone owns the data plane: SFTP and local backends, listing, copying, hashing, deletion
primitives, transfer accounting. This project owns the control plane: backup-set config,
artifact discovery, the durable lifecycle journal, copy/verify/commit/delete sequencing,
GFS retention, validation and quarantine, and reconciliation after a crash.

```text
rclone:
    move bytes reliably

backup-manager:
    decide what those bytes mean,
    when they are safe,
    when the source may be destroyed,
    and which restore points must survive
```

That boundary is the central architectural constraint. Application packages outside
`core/internal/transport/rclone` do not import rclone packages, so upstream API churn stays
contained in one adapter. `core/internal/transport/rclone/backends_test.go` fails the build if
that ever stops being true.

## Why rclone is embedded, not forked, not shelled out to

The short version: writing our own SFTP client is a bad use of time, and rclone already
does the data-plane part of this job well. That leaves three ways to consume it: fork it,
embed it as a library behind our own interface, or shell out to its CLI. We embed it.

We didn't fork it because a fork doesn't remove maintenance cost, it relocates it onto us
forever, for a large actively-developed project where we use maybe five percent of the
surface. We don't normally invoke the `rclone` CLI as a subprocess because every result
would come back as text we'd have to parse to recover typed errors and transfer statistics,
context cancellation would become process-signal management instead of a `context.Context`,
and the delete call, the most dangerous line in this codebase, would become a subprocess
invocation with no compile-time check that we passed the arguments we meant to.

The full reasoning, every alternative considered, and what embedding actually costs (not
just what it buys) is in
[`docs/adr/0001-embed-rclone-behind-transport-adapter.md`](docs/adr/0001-embed-rclone-behind-transport-adapter.md).
Read it if you're deciding whether this architecture fits a similar problem; this README
only summarizes it.

### The adapter

Every rclone import in this repository lives under `core/internal/transport/rclone`. Everything
else in the codebase depends only on the manager-owned interface in
`core/internal/transport/transport.go`:

```go
type Transport interface {
    List(ctx context.Context, source Source) ([]RemoteArtifact, error)
    Stat(ctx context.Context, source Source, remotePath string) (RemoteArtifact, error)
    CopyToLocal(ctx context.Context, source Source, remotePath, localPartialPath string) (TransferResult, error)
    RemoteHash(ctx context.Context, source Source, remotePath string, algorithm HashAlgorithm) (string, error)
    DeleteRemote(ctx context.Context, source Source, remotePath string) error
}
```

Notice what's missing: there is no `Move`. Copy, verify, commit and delete are four
separately owned steps on purpose. A `Move` would collapse them and take the delete
decision away from the lifecycle manager.

### The pinned version, and the backend count that surprised us

`core/go.mod` pins `github.com/rclone/rclone v1.75.0`. `core/internal/transport/rclone/adapter.go`
blank-imports exactly two backend packages, `backend/local` and `backend/sftp`. But the
adapter also needs `operations.Copy` from `fs/operations`, and that package itself imports
`backend/crypt` for an unrelated feature (decrypting filenames for `--show-encrypted`).
Backends self-register via `init()`, so importing `fs/operations` registers `crypt` too,
silently, as a side effect nothing in a casual read of the blank imports would reveal. So
importing two backends registers three. This is measured, traced to the exact import chain,
and pinned by `TestRegisteredBackendsExactSet` in
`core/internal/transport/rclone/backends_test.go`, so the registered set can't widen again
without the build failing.

If you need to confirm what's actually registered in a built binary rather than trust this
paragraph: `go mod why github.com/rclone/rclone/backend/crypt` shows the chain, and
`go version -m ./backup-manager | grep rclone/rclone` reads the exact linked rclone version
back out of a compiled binary, which is a faster sanity check than trusting whatever
`core/go.mod` said at build time actually got shipped.

### Upgrading the pin

No rclone dependency bump auto-merges, ever, not even a patch version with green CI. A
human reads the release notes. `docs/rclone-upgrade.md` is the actual checklist: what the
CI gate (`rclone-upgrade-gate.yml`) enforces today versus what's still manual, how to run
the regression set locally, and how to check what got registered instead of only what got
imported. Read it before touching the version in `core/go.mod`.

## Connecting to a remote: SSH/SFTP and the restricted account

`docs/ssh-setup.md` is the full walkthrough: generating a dedicated key, creating a
shell-less, chrooted SFTP-only account that can list/read/delete eligible artifacts but
can't overwrite a completed one, and verifying the server's host key out-of-band instead of
trusting whatever answers first. `core/internal/transport/rclone/ssh.go` refuses to build a
connection at all without both a real key file and a real `known_hosts` file; there's no
password fallback and no way to disable host-key checking.

That hardening has a direct consequence for verification and delete safety, and it's
important enough to state here instead of only in the setup doc: **rclone's SFTP hashing
works by running a hash command over the SSH session, and a shell-less
`ForceCommand internal-sftp` account has no shell to run one in.** So the account this
project's own setup guide recommends cannot supply a remote hash. I re-checked this against
the code for this rewrite and it is unchanged:
`core/internal/transport/rclone/adapter.go` still treats an absent hash capability as a
correct outcome rather than a failure, and `errors_test.go` still fails if `RemoteHash` ever
starts succeeding against a shell-less account, so the capability cannot be silently
downgraded into a weaker check. See [Verification](#verification) and
[TOCTOU protection on delete](#toctou-protection-on-delete) below for what that means in
practice, but the short version is: against the recommended deployment, remote deletes are
usually refused, and that's not a bug.

## An artifact is identified by its basename, and that has a consequence

`model.ArtifactID` is a backup set plus a plain basename. It refuses anything containing a
path separator, which is deliberate: a remote filename is untrusted input, and the cheapest
place to stop a name like `../../etc/passwd` is the moment it first becomes an identity
rather than at whichever later call site forgets to check.

The cost shows up now that discovery recurses. Two remote paths that end in the same
filename collapse to one identity, so `gitea-runs/run-1/backup.dump` and
`gitea-runs/run-2/backup.dump` are the same artifact as far as the journal is concerned.
The journal's `UNIQUE (source, backup_set, artifact_name)` refuses the second one, and
discovery reports it as a conflict naming both paths rather than dropping it silently or
failing the whole batch.

Listing is sorted by remote path so that outcome is repeatable. Before that fix it was not:
`walk.GetAll` returns backend order, so whichever path the backend happened to yield first
won, and the pair swapped places between runs. One cycle ingested `run-1` and reported
`run-2` as a conflict, the next did the reverse, and neither was reliably backed up.

Sorting makes the conflict stable, not absent. If your producer writes one directory per
run with a fixed filename inside, you will get exactly one artifact ingested per backup set
and a conflict for every other run, which is almost certainly not what you want. Until
identity carries more than a basename, give the artifacts distinct names, for example by
putting the run stamp in the filename rather than only in the directory. I re-checked this
for the rewrite: the separator ban in `core/internal/model/ids.go` and the `UNIQUE`
constraint in `core/migrations/0002_quarantined_lost.sql` are both still exactly as
described.

## The lifecycle

An artifact moves through twelve states, defined in `core/internal/lifecycle/state.go` and
`machine.go`, which are the single source of truth; the table below is a summary, not a
substitute.

```text
DISCOVERED -> TRANSFERRING -> TRANSFERRED -> VERIFYING -> VERIFIED
    -> COMMITTING -> COMMITTED -> REMOTE_DELETE_PENDING -> COMPLETE

FAILED         reachable from any state before COMMITTED; exits to
               DISCOVERED (retry) or QUARANTINED (retry budget spent)

QUARANTINED    reachable from VERIFYING, COMMITTED, REMOTE_DELETE_PENDING;
               exits to DISCOVERED only (a fresh attempt might recover it)

QUARANTINED_LOST   reachable only from COMPLETE; TERMINAL, no exit at all
```

`COMPLETE` and `QUARANTINED_LOST` are the two terminal states, and they mean opposite
things. `COMPLETE` is the only state that confirms the remote source is already gone, which
is exactly why it's the only predecessor of `QUARANTINED_LOST`: if the durably committed
local copy is later found corrupted and the remote copy is already deleted, there is no
copy of that artifact left anywhere, and no automatic path recovers it. `QUARANTINED`, by
contrast, means the content looked bad while a remote copy still exists or hasn't been
confirmed gone, so retrying from `DISCOVERED` has a real chance of fixing it.

This twelfth state isn't in the original FR-10 list; it was added because the eleven-state
version had no way to represent "the source is confirmed gone and the only copy we have is
bad," and sending that case back to `DISCOVERED` the way `QUARANTINED` does would just
livelock against a source that no longer exists. I re-checked the transition table for this
rewrite: `{From: Complete, To: QuarantinedLost}` is still the only edge into it, pinned by
`TestOnlyCompletePrecedesQuarantinedLost`, and it still has no edge back into the pipeline
and no automatic exit of any kind. Issue #220 gave it exactly one operator-triggered exit,
back to the `COMPLETE` it came from, for the case where the local copy turns out to be intact
and the finding was the mistake: an unmounted volume makes every `COMPLETE` artifact in a set
fail its local check, and before that there was no way back at all.
Whether an artifact is currently `QUARANTINED_LOST` matters enough operationally that it's
checked first, unconditionally, in health computation (see
[Status and health](#status-and-health)).

### Verification

`core/internal/lifecycle/verify.go` runs three layers, each with a different failure shape:

1. **Transfer verification**, always performed: the local file opens, reads without an I/O
   error, and its size matches what the transfer step recorded. Failing this means the copy
   didn't actually happen the way the journal claims; it's an operational failure, so it
   produces `FAILED`.
2. **Hash verification**, gated by `validation.hash` in config. If the operator hasn't
   asked for it, transfer verification is the whole guarantee for that backup set, and
   that's a legitimate choice. If they have asked for it, the manager either trusts an
   already-verified transfer-time checksum or asks the backend directly, and if the backend
   can't answer (see the shell-less SFTP account above), verification **fails explicitly**
   rather than silently falling back to a size check. A confirmed mismatch produces
   `QUARANTINED`, not `FAILED`, because that's a positive finding about the content, not
   about the copy mechanics.
3. **Application validation**, gated by `validation.command`, an optional external program
   (for example, something that opens a database dump and confirms it restores). A required
   validator's failure or timeout also produces `QUARANTINED`. The validators an operator
   can pick from are a registered catalog served by `GET /api/v1/validators`, not an
   arbitrary command the browser can supply.

`core/internal/revalidate` re-runs this against artifacts that already passed, on a cadence
and at a scope `config.Revalidation` sets, because a backup that verified six months ago is
not a backup that is good today.

### Durable commit

`core/internal/lifecycle/commit.go` implements the FR-14 sequence between `VERIFIED` and
`COMMITTED`:

1. record `COMMITTING` in the journal, before touching any file;
2. `fsync` the `.partial` file's content;
3. atomically promote it to its final name without clobbering an unrelated collision
   (`linkWithoutClobbering`, a hard-link-then-remove, not a plain rename);
4. `fsync` the containing directory, because the directory entry that now points at the
   final name is a separate inode from the file's data, with its own separate write-back
   state, and skipping this step is the mistake that lets a crash leave content that was
   genuinely fsynced sitting under a name nothing in the directory points at yet;
5. record `COMMITTED`.

Every step is idempotent and safe to resume after a crash at any point in the sequence. A
non-secret sidecar recovery manifest lands next to the committed artifact as well
(`core/internal/recovery`), carrying exactly enough metadata to reconstruct that artifact's
journal row and nothing that could leak a credential. That is what `catalog rebuild` reads.

### TOCTOU protection on delete

`core/internal/lifecycle/remotedelete.go` is, by its own doc comment, "the most dangerous line
in the project on purpose," and it is the only call site allowed to invoke
`Transport.DeleteRemote`. Before issuing a delete it revalidates, from scratch, every time:

1. the journal artifact is `COMMITTED` or `REMOTE_DELETE_PENDING`;
2. the artifact has never been reinstated out of quarantine;
3. the expected local final file exists;
4. the local file's identity is consistent with what the journal recorded;
5. the remote object still matches what was captured at discovery, via
   `model.CompareIdentity`.

Check 2 is what pays for the state machine's reinstatement edges (issue #220,
`docs/adr/0004-reinstating-a-quarantined-backup.md`). An operator can return a quarantined
backup whose durable local copy is provably intact to service, which matters most when the
remote source is already gone and re-ingesting is impossible, and the price is that this
manager will never delete that backup's remote source afterwards. The refusal is permanent,
reads the append-only transition log rather than a column, and derives the edges it looks for
from the state machine's own table, so a future exit from quarantine into a durable state is
covered the moment it is declared.

That last check is the TOCTOU defense: `RemoteIdentity` (path, size, mtime, hash where
available, a backend stable identifier where available) is captured once at discovery and
compared again immediately before delete. `CompareIdentity` can only reach
`ConfidenceStrong` through a hash match, a stable-identifier match, or an outright mismatch;
everything else, including "size and mtime both agree, nothing else was available," only
reaches `ConfidenceWeak`, and `IdentityComparison.Preserve()` is true for that outcome, same
as for a confirmed change. **Given the hardened SFTP account this project's own setup guide
recommends has no hash capability, that weak case is the normal, expected outcome, not an
edge case.** So in that deployment, `DeleteRemote` routinely refuses to delete, on purpose,
per the rule at the top of this document. That is not free: an archive that never prunes
its remote side will eventually fill the source disk. Every refusal is a typed
`*RemoteDeleteRefusalError` and gets written back into the journal's `remote_delete_error`
column, specifically so it's a queryable fact and not just a log line. See
[Recovery](#recovery-when-a-backup-did-not-arrive) for what to actually watch for this.

### Reconciliation

On startup, before normal processing touches anything, `core/internal/reconcile` compares what
the journal believes against what the local filesystem and the remote backend actually show
right now, for every scenario FR-17 names:

| Remote  | Local         | Journal               | Behavior                          |
|---------|---------------|------------------------|------------------------------------|
| exists  | absent        | `DISCOVERED`           | transfer (no-op here, next cycle's job) |
| exists  | partial       | `TRANSFERRING`         | safe retry/restart (no-op here)    |
| exists  | final         | `COMMITTED`            | verify and proceed toward delete   |
| absent  | final         | `REMOTE_DELETE_PENDING`| reconcile to `COMPLETE`            |
| absent  | final         | `COMPLETE`             | no-op                              |
| exists  | invalid final | any                    | preserve remote; quarantine local  |
| absent  | invalid final | any                    | quarantine, unrecoverable: `QUARANTINED_LOST` |
| changed identity | final | delete pending    | refuse delete; investigate         |

The last two rows are why `QUARANTINED_LOST` exists: the original FR-17 table had no row
for "remote already gone and the local copy is bad," and that case can't be treated the same
as "remote still there, local copy is bad," because there's nothing left to re-fetch from.
Every reconciliation transition is idempotency-keyed so a crash mid-reconciliation is safe
to retry. `backup-manager reconcile` runs it for every configured backup set, and `run` and
`daemon` run it for each backup set before touching that set (`core/internal/app/cycle.go`
is the ordering).

### Quarantine

Quarantine is a state, not a place. There is no quarantine directory and no file gets
moved; only the `artifacts.state` column changes, to `QUARANTINED` or `QUARANTINED_LOST`.
The file stays exactly where it was, its `.partial` path if quarantined before commit, or
its final committed path if quarantined afterward by reconciliation. See
[The lifecycle](#the-lifecycle) above for the states themselves. `core/internal/quarantine`
turns those rows into a countable, actionable picture, and `backup-manager quarantine` is
how an operator acts on one by hand, in one of three ways (issue #277):

- `quarantine revalidate <source/backup-set/artifact>` re-runs the durable-local-copy
  checks and reports the verdict, moving nothing either way. **This is not `validate` under
  a new name.** `backup-manager validate` only ever re-checks a *healthy* restore point
  (`COMMITTED`, `REMOTE_DELETE_PENDING` or `COMPLETE`) and refuses a `QUARANTINED` or
  `QUARANTINED_LOST` artifact outright; `quarantine revalidate` is the mirror image, and
  only ever accepts one of those two.
- `quarantine retry <source/backup-set/artifact>` puts a `QUARANTINED` artifact back into
  `DISCOVERED` so the ordinary pipeline attempts it again from a fresh fetch.
  `QUARANTINED_LOST` is refused: the remote source is already gone, so there is nothing
  left to re-fetch from.
- `quarantine reinstate <source/backup-set/artifact> [--note TEXT]` is #220's reinstatement
  lever: it re-checks the durable local copy and, if the evidence is enough, trusts the
  artifact again in place (`QUARANTINED` back to `COMMITTED`, `QUARANTINED_LOST` back to
  `COMPLETE`) without re-fetching anything. A reinstated artifact never authorises a remote
  delete again, ever.

## Retention

### GFS retention

`core/internal/retention` implements deterministic GFS (grandfather-father-son) classification
for every managed, completed backup in a set:

| Tier    | Default            |
|---------|---------------------|
| Daily   | 7 days              |
| Weekly  | 3 calendar months   |
| Monthly | 12 calendar months  |

`KEEP` is the union of whatever the daily, weekly and monthly tiers each retain (the newest
valid backup in every calendar bucket their look-back window covers). The calculation takes
"now" as a plain argument rather than calling `time.Now()`, specifically so the same journal
state always produces the same verdict regardless of when or where it runs. One thing worth
knowing if you're comparing this against `docs/EPIC.md`: the EPIC's example default
timezone is `America/Vancouver`, but `core/internal/config`'s actual validated default is `UTC`.
This package defers to whatever config supplies rather than hardcoding the EPIC's example,
so the honest current default is UTC; set `retention.timezone` explicitly if you want
something else.

### One backup set on its own retention policy

`retention:` at the top level is the deployment's policy and every backup set is retained
under it. A set that needs a different one writes its own `retention:` block, at the same
level as `remote_path` and `include`:

```yaml
retention:
  timezone: America/Vancouver
  daily_days: 90
  weekly_months: 24
  monthly_months: 60

sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        # no retention block: retained under the deployment's policy above
        ...
      - id: scratch-analytics
        retention:
          daily_days: 3
          weekly_months: 1
          monthly_months: 1
        ...
```

**A set-level block replaces the deployment's whole chain.** Writing two of the three
scalars is refused, not merged: `daily_days: 120` on its own would resolve weekly and
monthly to the product defaults (3 and 12) rather than to the 24 and 60 three lines up the
file, which is a set retaining four years less than the operator who wrote the deployment's
policy believes. So a set-level block names either a `tiers:` list or all three scalars, and
`backup-manager check` says which one is missing if it does not.

**Everything that is not the chain is inherited.** `timezone`, `week_starts_on` and
`protect_last_known_good` come from the deployment's resolved policy when the set-level
block leaves them out, because they decide how *any* chain is reckoned rather than what the
chain says. That is why the example above keeps `America/Vancouver` without repeating it: a
set falling back to UTC inside a deployment that deliberately set something else would
silently move which civil day a restore point belongs to.

**To go back to inheriting, remove the key.** A set inherits when it has no `retention:`
key at all, and equally when it has an explicitly null one (`retention:` with nothing after
it, `retention: null`, `retention: ~`). An empty block (`retention: {}`) is refused rather
than read as either, because "wrote nothing" and "wrote an empty policy" should not resolve
to the same thing.

`backup-manager retention` marks a set that decides for itself and names the chain it
decided with; a set with no marker inherited the deployment's. The
`backup-manager retention` override flags (`-tier`, `-daily-days` and the rest) override
the *deployment's* policy for that one invocation, so they move every inheriting set and
leave a set that declares its own alone.

**None of this needs a config-file edit any more (issue #333).** The three operations are
show, set and clear, and they are the same three on every surface:

```bash
backup-manager backup-set retention production/scratch-analytics
# which policy is in force, where it came from, and (for a set that overrides)
# the deployment's policy beside it, so you can see what clearing returns you to

backup-manager backup-set retention production/scratch-analytics \
    --daily-days 3 --weekly-months 1 --monthly-months 1

backup-manager backup-set retention production/scratch-analytics --policy-file ./policy.yaml
# the contents of a retention: block, key omitted; the only way to name a tiers chain,
# because a compact command-line grammar for one would be a second spelling of something
# this project already spells exactly one way. "-" reads standard input.

backup-manager backup-set retention production/scratch-analytics --inherit
# back to the deployment's policy, with no residue of the chain it declared
```

Over HTTP the same three are `GET`, `PUT` and `DELETE` on
`/api/v1/backup-sets/{source}/{set}/retention`. `PUT` rather than `PATCH` because an
override replaces the whole chain and is never merged with it; `DELETE` because "go back to
inheriting" has no spelling on a request where an absent field already means "leave this
alone". In the Web UI it is the Retention section of a backup set's own page, which names
the policy in force on both branches and shows the deployment's chain beside an override
before you clear it.

All three surfaces reach one method in `core/service`, and none of them validates anything
itself: a submitted policy goes through the identical `config.Validate` a hand-edited
`config.yaml` goes through at boot, so half a chain is refused with the same sentence in
the browser, at the terminal and in the file.

One rollback note: unknown keys are a parse error, so a config file carrying a set-level
`retention:` block cannot be read by a build from before this feature. Writing one is a
one-way door for a deployment that might need to go back.

### Which timestamp puts a backup in a bucket

Two of them, and `KEEP` is the union. Each tier runs its selection twice over the same
artifacts: once placing each one by the **discovery timestamp** (when this manager
first saw it on the remote), once by the **producer timestamp** (the remote object's own
modification time, captured at discovery). Both passes use the same windows and the same
"newest in the bucket" rule.

That matters the moment you point a new backup set at a directory that already holds a
year of dumps, or bring a manager back up after a week down. Every one of those artifacts
arrives in the same cycle, so by discovery timestamp alone they all land in one daily
bucket, one weekly bucket and one monthly bucket, each tier keeps one, and everything else
is a delete candidate on the first pass. Reading the producer timestamp as well puts them
in the buckets their own backup dates belong to, and the chain keeps the shape you
configured it to keep.

The producer timestamp is untrusted input (FR-8), so it is admitted only where being
wrong is survivable. It has to exist, be non-zero, and not be later than the discovery
timestamp (a completed artifact cannot have been produced after this manager first saw
it; such a timestamp is refused, not clamped). And because the two passes are unioned
rather than merged, a producer timestamp can only ever move an artifact from DELETE to
KEEP. A NAS whose clock says 1990 makes retention keep more than you asked for; it can
never make it delete something it would otherwise have kept. There is no setting for
this, deliberately: distrusting a remote's clock is a capacity question, not a safety one.

Two consequences worth knowing. A chain can retain up to twice its nominal bucket count,
since one bucket can contribute one artifact from each pass. And an artifact older than
your longest configured window is still a delete candidate, however it arrived, so extend
the chain if you want it kept.

`GFSDecide` only classifies. A verdict of `Keep: false` is a delete *candidate*, and
`core/internal/retention/prune.go` is what turns candidates into deletions. That is a change
from the previous version of this document, which said no code path deleted a local file:
one does now, and it is the second file in this repository whose doc comment calls itself
the most dangerous line in the project.

### A restore point written as several files

GFS classifies one artifact at a time and selects at most one representative per bucket per
tier. That is also, implicitly, an assumption that one artifact IS one restore point. Most
producers write that way, but not all of them: a producer that writes a portable archive and
a native database dump of the same backup run, both carrying the run's own timestamp, hands
this manager two artifacts that only restore together as one thing, in one directory, in one
backup set.

Point a single backup set's `include` at both files and GFS still has no concept of "these two
belong to one restore point." Each tier's bucket admits both as separate candidates competing
on the same timestamp, picks one as its representative by the same deterministic name
tie-break `gfsIsNewerRepresentative` always uses for any tie, and the loser comes back
`Keep: false`, `tiers=[]` (issue #292). Applied, that deletes half a restore point and keeps
the half that happens to sort last, and nothing about a bare `tiers=[]` says whether that
artifact is genuinely older than every configured window or lost only because a sibling
artifact from its own run won the tie-break.

**The configuration this manager actually supports today is one backup set per file
pattern.** Point one backup set's `include` at the archive (`include: ["gitea-dump-*.tar.gz"]`)
and a second backup set's `include` at the dump (`include: ["gitea-db-*.dump"]`), and each
set has exactly one artifact per bucket, so GFS classifies correctly within each set. Know
the trade-off before leaning on it: the two sets retain independently, so nothing keeps them
selecting the *same* run, a verification failure quarantining one day's file in one set can
leave the two sets' retained runs drifting apart over time, and `status` reports each set's
health on its own, with no line connecting the two halves of one restore point. Modelling a
restore point as more than one artifact end to end, so retention, last-known-good and
`status` all reason about the group rather than the file, is a real question and a
substantially bigger one than this section's scope; splitting by `include` pattern is the
narrower answer this manager gives today.

What issue #292 *does* add: `retention --dry-run` no longer lets that split through silently.
When two artifacts in one backup set tie on the exact same discovery or producer instant (in
practice, the same run, captured as more than one file) and the tie-break sends one of them to
KEEP and the other to DELETE, the losing artifact's line grows an indented `! sibling
collision:` warning naming the sibling it tied with, so `tiers=[]` because "older than every
window" and `tiers=[]` because "a sibling in the same bucket won" no longer print the same way.
`core/internal/retention/prune.go`'s own `PruneVerdict.Reason` (and, through it, the
`POST .../retention/apply` an administrator would review before it runs) carries the identical
warning. Nothing about the KEEP/DELETE decision itself changes: the artifact still is deleted
by policy exactly as before, and it still would be, with a per-run backup set split, if that's
the workaround chosen above. This is a narrower fix on purpose, refusing the *silence*, not the
split itself; see the issue for why the full remodel is out of scope here.

### Last-known-good protection

**Implemented**, which is also a change from the previous version of this document. FR-19
says the newest known-good restore point must never be deleted solely for exceeding
retention age, and `core/internal/retention/lastknowngood.go` is that rule.
`config.Retention.ProtectLastKnownGood` defaults to `true` when the key is omitted, and
turning it explicitly off is reported by name as "a materially more dangerous
configuration" rather than accepted quietly.

"Newest" means newest by the backup's own date, resolved the same way the producer pass
above resolves it, not the most recently ingested artifact. The preview line names the
resolved date and says which of the two timestamps produced it, so you can tell at a
glance whether a remote reported a usable modification time or the manager fell back to
when it first saw the file. (Note that the recovery sidecar's own `received_timestamp`
is a different instant: that one is when the artifact finished committing locally. The
field matching the discovery timestamp is `retention_timestamp`.)

Two ways to see what a policy would do before it does it:
`backup-manager retention --dry-run`, which also takes per-run overrides for the timezone,
the week start and each tier so you can compare policies without editing config; and
`GET /api/v1/backup-sets/{source}/{set}/retention/preview` in the web UI, whose apply
counterpart refuses a plan that has gone stale rather than silently recomputing a wider one.

## Status and health

`core/internal/health` computes two structurally separate things and enforces, by test, that
they never share a field: process health (is the binary alive, what version is it, what
rclone version is embedded) and backup-set health, one of four states:

- **HEALTHY** – a known-good backup exists within the freshness threshold, and nothing else
  is wrong.
- **DEGRADED** – either no history yet, or a known-good backup is still fresh enough but
  something less than ideal just happened (a quarantined newest arrival, a failure still
  being retried).
- **STALE** – no known-good backup inside the freshness threshold, and nothing suggests
  one is imminent.
- **FAILING** – checked first, unconditionally: any `QUARANTINED_LOST` artifact, or a
  `FAILED` artifact with no retry scheduled.

`backup-manager status` renders all of it, including the `QuarantinedCount` and
`QuarantinedLostCount` aggregates FR-24 asks for, and exits non-zero unless every configured
set reports HEALTHY, which is what makes it the container healthcheck the image bakes in
rather than only something to read. It is deliberately not what any container START waits on:
a fresh install has backed nothing up, so the verdict is negative and gating on it would keep
the web UI from ever coming up. The packaged runtime definitions ask `/health/live` for that
instead. The API side is `GET /health/live` and `GET /health/ready` on the engine,
deliberately outside `/api/v1` and outside authentication. What does not exist is a
`/metrics` endpoint, as [Status](#status-what-actually-runs-today) says above.

`core/internal/alert` turns those same computed signals into at most one operator-facing
notification per condition, and it delivers through a platform capability rather than
inventing one. The generic Docker adapter declares no notification capability at all, so on
that platform alerting refuses at wiring time and says so on startup, rather than being
discovered later as silence.

## Recovery: when a backup did not arrive

This is the section to read under pressure. The fuller version, with more of the "what if"
branches, is [`docs/recovery.md`](docs/recovery.md); this is the part you shouldn't have to
click through to get.

Start with `backup-manager status --config <path>` and `backup-manager artifacts --config
<path> --backup-set <set>`, which is faster than a query and does not need you to know the
schema. When you want the raw truth, or the binary is not to hand: **the SQLite journal at
`state.database` is the truth, and it's a plain SQLite file.** Query it directly:

```bash
sqlite3 /path/to/state.db "
  SELECT artifact_name, local_path, state, updated_at, remote_delete_error
  FROM artifacts
  WHERE source = 'production' AND backup_set = 'postgres-primary'
  ORDER BY updated_at DESC
  LIMIT 20;
"
```

Only three states are ever a valid restore point: **`COMMITTED`, `REMOTE_DELETE_PENDING`,
`COMPLETE`**. That's not a convention, it's the exact set `core/internal/health` calls
`knownGood`. Everything else, `DISCOVERED` through `COMMITTING`, `FAILED`, `QUARANTINED`,
`QUARANTINED_LOST`, or any `.partial` file you find sitting on disk regardless of what the
journal says, is not a restore point. Take the newest row in one of the three good states;
its `local_path` is the file, already fsynced and atomically committed (see
[Durable commit](#durable-commit)). Copy it wherever you're restoring to. There is no
`restore` command to do that for you and there is not meant to be: restore execution is out
of scope, so the last step is yours.

If the newest row for that backup set is `QUARANTINED_LOST`: that specific backup is gone
for good. The remote copy was already deleted before the local corruption was found, and no
automatic path recovers it (see [The lifecycle](#the-lifecycle)). Look at the next-newest
row in a known-good state and treat the loss as real when deciding what to tell whoever
needs the data, not as something to retry.

If it's `QUARANTINED` (not `_LOST`): the remote copy may still exist, so this can self-heal.
`backup-manager reconcile` and the next `run` or `daemon` cycle against that backup set are
what try automatically. To act on it yourself right now, without waiting for a cycle, see
[Quarantine](#quarantine) above: `quarantine revalidate <source/backup-set/artifact>`
re-checks the durable local copy and reports the verdict without moving anything,
`quarantine retry` re-enters the pipeline from a fresh fetch, and `quarantine reinstate`
trusts the local copy again in place. (`backup-manager validate` is a different command: it
only ever re-checks a *healthy* restore point and refuses a `QUARANTINED` artifact outright.)

If a row has been sitting at `REMOTE_DELETE_PENDING` for longer than you'd expect, look at
its `remote_delete_error` column before assuming something is stuck. Given the deployment
this project recommends, a persistent refusal there is the expected behavior described in
[TOCTOU protection on delete](#toctou-protection-on-delete), not necessarily a bug, and it
means the remote copy is very likely still sitting on the source server, still recoverable,
just not pruned. Left unattended, that also means the remote source disk isn't being
freed by this project on that backup set; monitor it directly rather than assuming pruning
is happening in the background.

If the state database itself is gone or corrupt, `backup-manager catalog rebuild --dry-run`
reports what it could reconstruct from the sidecar recovery manifests sitting next to the
committed artifacts, and dropping `--dry-run` does it.

## Toolchain

Go 1.27, Node for the frontend workspaces, and Docker for the disposable SFTP server the
integration tests use.

This repository is four Go modules stitched together by `go.work`: `core/`, `apps/common/`,
`apps/generic/` and `apps/synology/`. The engine's own commands run from `core/`:

```bash
cd core
go build ./...
go vet ./...
go test -race ./...
```

`-race` rather than a bare `go test`, because that is what the gate runs (see below) and
because this engine's core loop is a scheduler handing a config snapshot to a cycle while
service methods swap that snapshot underneath it. Drop the flag for a quick single-package
loop if you like; do not form an opinion about a change from a run without it.

### The local gate

`scripts/ci-local.sh` is the gate for this repository. `.github/workflows/ci.yml`,
`rclone-upgrade-gate.yml` and `nightly-e2e.yml` are all `workflow_dispatch`-only, so
**nothing runs on push or on a pull request**, and `.husky/pre-commit` runs this script on
every commit instead. It mirrors those workflows job for job, which makes it slow: the whole
`core/` suite including the Docker-backed crash matrix and the SFTP integration tests, both
cross-compiles, every Go module's build/vet/test/lint, the frontend
lint/typecheck/eslint/vitest/build set, the cross-provider conformance suite, and the
repository-structure dependency proofs.

Install the JS workspaces before the first full run in a new clone or `git worktree`:

```bash
(cd ui/shared && npm ci)
(cd apps/common/tests && npm ci)
```

`node_modules/` is gitignored, so a fresh checkout has none. A full run refuses to start
until every JS workspace present in the tree is installed, and prints the exact command
for each one that is not. It used to skip those checks and still print
`==> ci-local: ok`, which is what made the gate's own success line unreliable (#160).

The Docker daemon is the same rule with a bigger blast radius. With it stopped, the crash
matrix, the SFTP integration suite, `distribution/tests/adapterstacks` and the whole
`apps/generic/tests/dockercli` package call `t.Skip`, `go test` still exits 0, and nothing
would reach the ledger. A full run refuses to start without a reachable daemon.

That refusal is about the start of the run, and the start of the run is not the run (#457).
Docker Desktop's Resource Saver stops the hypervisor after five idle minutes, and this gate
has several Docker-free stretches longer than that, so every run was cold-starting the VM
somewhere in the middle and two runs died of it, both looking like skips rather than
failures. Two things stop that now. A sentinel container (`alpine sleep infinity`) is
started after the preflight and removed on the way out, including when the run fails or is
interrupted, so the daemon is never idle and Resource Saver never fires, on any machine and
without depending on a GUI setting. And every Docker-dependent step re-probes the daemon
immediately before it runs, which costs about 100ms and turns "the VM died at minute 18"
into `==> ci-local: FAILED` naming the step that needed it. The preflight also warns, and
only warns, when it can see that Resource Saver is on.

Everything the gate starts gets `CI_LOCAL=1` in its environment, which is how the Docker
fixtures in `core/tests` tell "this laptop has no Docker", an honest skip when you are
running one suite by hand, from "the daemon this gate already used has gone away", which is
a failure.

Every `go test` the gate runs carries `-race` (#417). Until that landed it ran none at all,
anywhere, which is the same shape as every other hole this gate has had to close: `go test`
exits 0 whether the detector looked or not, so "this tree has no data race" and "nobody
asked" were the same output. On this product that gap sat over the code most likely to have
one. The `{inner, revision}` pair, the edit-holds registry and the journal are all shared
across goroutines, and one test in `core/service` says in its own doc that it proves nothing
except under the detector, which until now it had never once been run under.

It is a flag on the steps that already exist rather than a step of its own. A separate step
would run the same suites a second time and buy nothing, since `-race` replaces no
assertion: everything a plain run checks, the instrumented run checks too, plus the
detector. And a separate step is one more thing that can be commented out while the suites
still run and still report `ok`. So Group K of the gate's own self-test pins the rule that
follows: not "there is a race step" but "no `go test` in this gate runs without the
detector", which is a rule a new module cannot be added around by accident.

RACE_COST_TABLE_PLACEHOLDER

Turning it on found two things in `core/internal/transport/rclone` on the first run, and
neither was a flake. One is a real data race, in rclone v1.75.0's `lib/atexit` rather than
here: it publishes its signal channel in a plain package-level variable and writes `nil`
over it in `IgnoreSignals` while the goroutine `Register` started is reading it, which
`DisableSignalExit` reaches. Nothing on this side can add a synchronisation edge between two
accesses in another module, and the shipped daemon disables signals before its first
transfer so it never installs that handler at all, so the one row that provokes it runs
under a suppression that `TestDisableSignalExit` holds to account: the child says what it
suppressed, and the test asserts that exactly the provoking row used it and no other row
did. When rclone fixes it, that assertion goes red and the file gets deleted. The other was
a sampling assertion whose odds move with machine load, which the detector's slowdown pushed
over; the claim it carried now lives in a row where no coin is tossed.

`scripts/race/selftest.sh` is the control for all of it, in the shape #242 established for
the compatibility and conformance cells: it plants a real data race in real product source
in a copy of the tree, requires the detector to catch it and to name the write that planted
it, and then runs the same mutant with the flag off and requires it to go green. That last
cell is the one that makes the other two mean anything.

Which tests get a container at all is a rule, not a habit: `docs/architecture/test-tiers.md`
says which tier a test belongs to (unit, integration, or a machine reached through
`core/tests/machines`), and `core/internal/testtier` holds the tree to it.

Three environment variables change what runs:

| Variable | Effect |
|---|---|
| `CI_LOCAL_FAST=1` | Fast iteration loop: skips `core/`'s `./tests/...` (the crash matrix and the SFTP integration tests), both cross-compiles, the production builds, the conformance suite, the structure proofs and the gate's own self-test. It does not skip `apps/generic`, whose tests bring a compose stack up, so a FAST run is not a Docker-free run. Always ends INCOMPLETE. |
| `CI_LOCAL_SKIP_JS=1` | Proceeds past the preflight with uninstalled JS workspaces instead of failing, for a change that only touches Go. Ends INCOMPLETE whenever it actually left a workspace out; with everything installed it changes nothing and the run can still be `ok`. |
| `CI_LOCAL_SKIP_DOCKER=1` | Proceeds past the preflight with the daemon down instead of failing. Ends INCOMPLETE, because the Docker-backed suites will have reported `ok` without running. |
| `CI_LOCAL_SKIP_TWO_MACHINE=1` | Leaves out the two-machine end-to-end backup proof (#356), which is the only test anywhere that a fresh install pulls a real backup off a real machine. Ends INCOMPLETE. |
| `CI_LOCAL_SENTINEL=0` | Does not start the sentinel container that keeps the Docker daemon out of Resource Saver's idle timer (#457). The per-step daemon probes still run, so a daemon that dies is still a named failure rather than a skip. |
| `CI_LOCAL_SENTINEL_IMAGE` | The image the sentinel runs, `alpine:3.20` by default, chosen because the SFTP fixture already builds from it so every machine that can run this gate has it cached. |

A run that skipped anything ends with `==> ci-local: INCOMPLETE`, lists what did not run,
and exits 3. A run that performed every check it invoked ends with `==> ci-local: ok` and
exits 0, and that pair is what makes the gate readable as merge evidence by a human and by
a script. A run that failed ends with `==> ci-local: FAILED` naming the step, and exits
with whatever failed. `.husky/pre-commit` allows 3 and says so out loud, so the fast
iteration loop still commits; nothing that merges on this gate's word may accept anything
but 0.

Playwright e2e used to be the qualification on `ok`: it was not in the gate at all, so
`ok` meant every check the gate invoked, which did not include the browser. It is in the
gate now (#197), from outside the repository. The suite moved to
[`spdrman/rclone-manager-tests`](https://github.com/spdrman/rclone-manager-tests) in #158,
and a non-FAST run checks that repository out at the sha in `scripts/e2e/tests-repo.pin`
and runs two things against the working tree: its CLI contract smoke slice, 55 black-box
cases against a `backup-manager` built from this tree, and its browser suite, 165 tests
against this tree's `ui/shared`. About half a minute together. A red spec exits nonzero,
this script is `set -e`, so the commit is refused.

On a machine with no Playwright browser the step refuses and names the install command;
`CI_LOCAL_SKIP_E2E=1` is the out-loud opt-out that ledgers the skip, so that run ends
`INCOMPLETE` rather than `ok`, the same way a stopped Docker daemon does.
`scripts/e2e/README.md` has the mechanics, including how to move the pin and what to do
when the pin and the working tree legitimately disagree.

A non-FAST run also performs the two-machine end-to-end backup proof (#356): two throwaway
containers on a temporary network, the real installer, a backup set created through the
CLI, and the artifact compared to the source by SHA-256. It has three outcomes rather than
two, and the third is the point. A machine with no Docker, or one whose daemon refuses a
privileged container so docker-in-docker cannot start, cannot perform the proof at all: the
script says `CANNOT RUN` and exits 3, this gate ledgers that, and the run ends `INCOMPLETE`
naming the proof it could not perform. Reporting `ok` for a backup nobody proved would be
the worst version of the failure this whole ledger exists to prevent.

A component that is not in the tree at all is not a skip: its checks are inapplicable, and
the run can still be `ok`. Today `apps/ugos/backend` and `apps/ugos/frontend/upk-proof`
are the absent ones; `apps/generic` and `apps/synology` are present and are built, vetted,
tested and linted on every run.

## How this document is kept honest

The previous version of this README described a binary with one subcommand, eleven
subcommands after that stopped being true. It listed eleven packages under `core/internal`
when there were seventeen. Both survived because prose does not fail a build, so the claims in here
that a machine can decide are now decided on every run, by
`distribution/packaging/readme_claims_test.go`:

- every markdown link and every backticked repository path in this file resolves, with the
  handful of paths this document names *because* they are absent kept in an explicit list
  with a reason each, so admitting what is missing stays possible;
- the command table above matches the dispatch table in `core/cmd/backup-manager/main.go`,
  and that dispatch table matches the help text the binary prints, so all three move
  together or the build goes red;
- the `core/internal/` inventory in [Layout](#layout) matches the packages that are actually
  on disk;
- whether the browser client and the router still agree about the version route is
  re-derived from `client.ts` and `router.go` on every run, in both directions, so a claim
  about it in this document cannot outlive the drift it describes (and could not survive
  the repair either, which is how #211 found out this section needed rewriting);
- the "build-supported and uncertified" statement holds for exactly as long as the generated
  conformance matrix still reports an unexecuted operator cell, in both directions;
- the support tiers in the table above come from `distribution/packaging/canonical.json`.

Each of those carries its own positive control, because a check that cannot fail is
decoration. What is deliberately *not* checked there, and why, is written at the top of that
test file: anything needing real hardware, and the measured binary size. The client's
request paths used to be on that list as "not decidable by reading TypeScript string
concatenation"; #166 landed the contract that made the question answerable and #211 answered
it, in `scripts/api/check-client-paths.sh`.

## Layout

Since #165 (Phase 6) the repository has **three product layers**, declared once in
`scripts/architecture/layers.conf` and enforced rather than described: a
provider-neutral **core** (plus the application services, the `/api/v1` host and the
shared UI), a **runtime platform** layer of per-host profiles, and a **distribution**
layer of packaging, metadata, templates and store presentation.
[`docs/architecture/layers.md`](docs/architecture/layers.md) is the full account: what each
layer owns, the dependency direction, which check proves which claim, how each of those
checks was shown to be able to fail, and the map from the old layout for rebasing an
in-flight branch. The rest of this section describes the same tree from the inside.

`core/` is its own Go module (`core/go.mod`), separate from the repository root, drawn
that way by #106/B1.1 so the engine has never heard of a provider or a UI (see
`docs/EPIC-B-multi-nas.md` §7 for why). `core/cmd/backup-manager/` is the entry point,
`core/service/` is the process-lifetime service layer the web host and the CLI share, and
`core/internal/` holds every application package, with every rclone import staying inside
`core/internal/transport/rclone/`:

<!-- BEGIN CORE-INTERNAL -->

```text
core/internal/
  alert/         at-most-once operator notifications, delivered through a platform capability
  app/           the presentation-agnostic application service every command and handler calls
  archive/       what a storage class means for getting bytes back, and the restore that has to be asked for
  artifactstore/ where a committed artifact's bytes live, and the seam that lets that be somewhere else later
  capacity/      disk-space admission checks
  config/        YAML config schema, loading, validation (Load takes any path)
  discovery/     turns a raw remote listing into artifacts proven complete
  health/        process and backup-set health computation
  lifecycle/     the state machine plus every step: transfer, verify, commit, delete
  metrics/       a health report rendered as Prometheus text (built, exposed nowhere)
  model/         shared identity types: ArtifactID, BackupSetID, RemoteIdentity, CompareIdentity
  obs/           structured event logging
  placement/     the verification ladder: what each class of check proves about a durable copy, and what it costs
  quarantine/    the operator-facing view of what is quarantined and why
  recovery/      the non-secret sidecar manifest written beside every committed artifact
  reconcile/     startup reconciliation against the journal, filesystem and remote
  retention/     GFS classification, last-known-good protection, and the local prune
  revalidate/    scheduled re-verification of artifacts that already passed
  state/         the SQLite journal: durable, idempotent transition recording
  testenv/       the environment a test has to be in before it may conclude anything from file permissions
  testtier/      which tier a test belongs on, and the guard that refuses one written in the wrong place
  transport/     the manager-owned Transport interface and the rclone adapter behind it
```

<!-- END CORE-INTERNAL -->

Config example: `core/internal/config/testdata/full.yaml` has a complete, valid config with
every field populated; that's a better reference than hand-writing one here, since it's
exercised by the config package's own tests and won't silently drift out of sync with the
schema the way a README example would.

`apps/common/` is a second Go module (`apps/common/go.mod`) and is no longer the mostly
empty boundary-drawing exercise the previous version of this document described. It holds
`platform/capabilities/` (the `PlatformCapabilities`/`PlatformAdapter` contract every
provider composes over, §3.4), `webhost/` (the whole `/api/v1` surface, its handlers, its
auth middleware and its destructive gate), `auth/local/` (local-account authentication,
enrollment and password rotation), `csrf/`, and `packaging/` (the canonical packaging
description plus the checkers that hold every provider to it). `apps/common/tests/` is a
separate small TS package: the one place in the repo that legitimately imports every
provider's frontend bridge at once (the provider-conformance matrix, §63A), kept outside
`ui/shared/` specifically so removing a provider never breaks `ui/shared`'s own build.

`ui/shared/` is the one shared frontend every provider app builds against
(`ui/shared/src/`), never providing its own product UI (see `docs/EPIC-B-multi-nas.md` §11):
pages, components, the `PlatformBridge` contract (`ui/shared/src/platform/`,
`ui/shared/src/types/platform.ts`), and the single causl-ts state graph
(`ui/shared/src/state/graph.ts`). A provider app under `apps/<provider>/frontend/`
supplies a `PlatformBridge` implementation and little else; `ui/shared` never imports a
provider, only the reverse. Two providers carry a Go module of their own: `apps/generic/`
is the generic Web host (#82/B4.1), and `apps/synology/` is the DSM `.spk` packaging and
conformance module (#85/B4.4), which ships no product binary of its own and instead wraps
the release binaries unchanged and checks their digest against
`container/release-manifest.json`.

Four providers carry packaging metadata next to their bridge: `apps/truenas/` (a custom-app
Compose file plus a TrueNAS Apps catalog entry), `apps/unraid/` (two Community Applications
Docker templates), `apps/openmediavault/` (a Compose deployment profile) and
`apps/proxmox/` (the same Compose profile again, for a dedicated container-host guest,
because Proxmox VE has no application store to package into at all). All four are metadata
and templates only, wrapping the exact canonical OCI image with no lifecycle code of their
own, and `distribution/packaging/` holds them to that on every commit: one shared source of
truth in `canonical.json`, plus scanners for the Phase 4 gate checks that are decidable from
the repository alone.

The same package runs the cross-provider conformance matrix (§63A) across all seven
providers at once, reporting an outcome per provider per capability rather than one
verdict per run, with `UNSUPPORTED`, `NOT_APPLICABLE` and `BLOCKED` as first-class
results a provider has to declare rather than reach by omission. The recorded run is
[`docs/conformance/phase-4-matrix.md`](docs/conformance/phase-4-matrix.md), generated and
then checked, so it cannot drift from what the suite actually finds. The half that is
not decidable here, installing and updating and removing on the real platform, lives in
[`docs/acceptance/`](docs/acceptance/) as prewritten operator procedures, and until one
is executed its provider is build-supported and uncertified.

EPIC B's Phase 6 reorganises this into explicit core, runtime-platform and distribution
layers and reduces every platform package to a thin adapter. That work is #184, #194, #199
and #169, none of them merged as this document is written, so the layout above is what is
here today rather than what it is becoming.

This project was originally scoped as `tools/backup-manager/` inside `iasbuilt/iac`. It
lives here instead; nothing in the design depended on the location.

## Documentation index

- [`docs/deployment.md`](docs/deployment.md) – the container build, the two-service Compose topology, the read-only rootfs and uid/gid rules, and release hashes
- [`docs/adr/0001-embed-rclone-behind-transport-adapter.md`](docs/adr/0001-embed-rclone-behind-transport-adapter.md) – why embed, why not fork or shell out, what it costs
- [`docs/adr/0002-phase-5-scope.md`](docs/adr/0002-phase-5-scope.md) – why observability stops where it does
- [`docs/adr/0003-pull-encrypted-runs-to-the-nas.md`](docs/adr/0003-pull-encrypted-runs-to-the-nas.md) – the pull model, and why the NAS is the initiator
- [`docs/rclone-upgrade.md`](docs/rclone-upgrade.md) – the pinned-version upgrade procedure and its CI gate
- [`docs/ssh-setup.md`](docs/ssh-setup.md) – the dedicated key, the restricted SFTP account, host-key verification
- [`docs/recovery.md`](docs/recovery.md) – recovery and the restore procedure, in full
- [`docs/storage-mediums.md`](docs/storage-mediums.md) – configuring an S3 medium per retention tier, what the disclosure commits you to, what each verification class proves and costs, and what an archive class means for the day you need the file back
- [`docs/conformance/epic-e-matrix.md`](docs/conformance/epic-e-matrix.md) – which of EPIC E's gate lines are checked by something that has been watched to fail, which are checked by nothing, and which issue owns each gap
- [`docs/phase-1-gate.md`](docs/phase-1-gate.md) – the embedding proof-of-concept verdict and what it did and didn't prove
- [`apps/synology/README.md`](apps/synology/README.md) – the Synology DSM `.spk`: supported architectures/models, how to build and verify one, and what is still uncertified
- [`apps/truenas/README.md`](apps/truenas/README.md) – the TrueNAS custom app and catalog entry
- [`apps/unraid/README.md`](apps/unraid/README.md) – the two Community Applications templates, and the one place this profile is weaker than the others
- [`apps/openmediavault/README.md`](apps/openmediavault/README.md) – the OMV Compose deployment profile
- [`apps/proxmox/README.md`](apps/proxmox/README.md) – the Proxmox VE deployment profile: the one supported model, what the PVE host contributes, and what is deliberately absent
- [`docs/conformance/phase-4-matrix.md`](docs/conformance/phase-4-matrix.md) – the cross-provider conformance matrix (§63A), per provider and per capability, including what is blocked and on what
- [`docs/acceptance/`](docs/acceptance/) – the provider acceptance procedures (§68), written and not yet executed
- [`docs/architecture/layers.md`](docs/architecture/layers.md) – the three layers (core, runtime platform, distribution), what each owns, the dependency direction, and the checks that enforce it
- [`docs/perf/README.md`](docs/perf/README.md) – the Phase 6 performance baselines, the benchmark host and workload, and the concrete regression thresholds
- [`distribution/README.md`](distribution/README.md) – the distribution layer: what makes an adapter an adapter, and where the rest of that layer still lives
- [`docs/compliance/release-provenance.md`](docs/compliance/release-provenance.md) – what a release records, how the SBOM and checksums are produced, and how an image is signed without this project ever holding a key
- [`docs/compliance/`](docs/compliance/) – the store-facing compliance materials: privacy policy, support, and the written offer of source
- [`docs/EPIC.md`](docs/EPIC.md) – the full specification this project is built against, including where it and the code have since diverged
- [`docs/EPIC-B-multi-nas.md`](docs/EPIC-B-multi-nas.md) – the multi-NAS provider architecture, the support tiers, and the Phase 6 refactor

## Licence

Apache License 2.0. The full text is in [`LICENSE`](LICENSE).

Every third-party component that reaches a shipped artifact is permissive
(MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, CC0-1.0) except two:
`go-cleanhttp` and `go-retryablehttp` are MPL-2.0 and arrive under
rclone's `s3` backend, which cannot be registered without them. MPL-2.0 is
file-level weak copyleft and §3.3 permits this Larger Work to ship under
Apache-2.0, so the choice stands, and the §3.2 obligation it carries is
recorded in `compliance.json` and discharged in `NOTICE` and
[`docs/compliance/source-offer.md`](docs/compliance/source-offer.md), which
name both modules at their exact versions and give the immutable address their
source is served from.

All of that is checked rather than remembered: the inventory is re-derived from
the live module graph and the frontend lockfile on every gate run, a licence
that is neither permissive nor accepted fails the build, and so does an
accepted licence whose offer those two files stop carrying.

- [`NOTICE`](NOTICE) – the attribution file Apache-2.0 §4(d) refers to, grouped by licence
- [`provenance/third-party-licenses.json`](provenance/third-party-licenses.json) – the machine-readable inventory, with the SHA-256 of each component's licence text
- [`provenance/sbom.spdx.json`](provenance/sbom.spdx.json) – an SPDX 2.3 SBOM of the same set
- [`provenance/checksums.txt`](provenance/checksums.txt) – `sha256sum -c` over the whole release

All four are generated, never hand-edited:

```
cd distribution && go run ./cmd/provenance -write
```
