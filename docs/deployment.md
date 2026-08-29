# UGREEN container deployment

This documents the container packaging for `cmd/backup-manager` (A3.9): what's in
`container/`, why it's shaped the way it is, and how I verified each requirement rather
than just asserting it. It's meant to be read next to `container/Dockerfile` and
`container/compose.yaml`, which carry the same reasoning inline as comments.

## Status: packaging ahead of the daemon

`cmd/backup-manager` only implements a `version` subcommand today. Execution modes
(`run`, `daemon`) and the rest of the CLI (`status`, `check`, `fetch`, ...) are separate,
still-open issues. This container and compose file exist to define the deployment shape
(volumes, uid/gid, restart policy, health check) ahead of that work, not to run a working
service today. Grep `container/Dockerfile` and `container/compose.yaml` for
`TODO(daemon)` and `TODO(#26)` for the exact two spots that need to change once the
daemon and `status` subcommand exist.

## rclone is compiled in, not shelled out to

The image contains no `rclone` binary anywhere, and I checked that directly against the
built image rather than trusting the design:

```
$ docker create --platform linux/arm64 backup-manager:0.0.0-a3.9 version
$ docker export <container-id> | tar -tv | grep -i rclone
$ echo $?
1
```

Exit 1 means zero matches, checked case-insensitively against the full file listing of
the exported image filesystem (1447 entries: the distroless base's certs/tzdata/passwd
plus exactly one executable, `/backup-manager`). There's no file named `rclone`, no
`rclone` directory, nothing.

The flip side, that rclone's packages are genuinely compiled into that one binary rather
than the manager silently doing nothing useful, is also checked directly:

```
$ strings backup-manager | grep -c 'rclone/rclone'
2770
$ strings backup-manager | grep 'rclone/rclone' | sort -u | head
 github.com/rclone/rclone/fs/hash
 github.com/rclone/rclone/fs/list
 github.com/rclone/rclone/fs/walk
,github.com/rclone/rclone/fs/config/configmap
!github.com/rclone/rclone/fs/march
...
```

2770 occurrences of `rclone/rclone` import paths inside a `stripped`, `statically
linked` ELF binary. rclone is a Go module dependency (`go.mod` pins
`github.com/rclone/rclone v1.75.0`), imported as packages by `internal/transport/rclone`,
and compiled straight into `/backup-manager` by the builder stage. `CGO_ENABLED=0`
throughout means this holds without a C toolchain on either target architecture, which is
also why `modernc.org/sqlite` (the state package's SQLite driver, pure Go, no cgo) was
the only option that ever made sense here.

## Reproducible build

- **Base images pinned by digest**, not tag, in both stages of `container/Dockerfile`:
  the `golang:1.27-bookworm` builder and the `gcr.io/distroless/static-debian12:nonroot`
  runtime. A tag can move; a digest can't. The builder's Go version
  (`golang@sha256:ded31c68...`) matches this module's `go 1.27.0` directive exactly, so
  there's no drift between what `go.mod` asks for and what compiles it.
- **`GOTOOLCHAIN=local`** so `go build` never reaches out to fetch a different toolchain
  mid-build if some future `go.mod` bump disagreed with the pinned builder image.
- **`-trimpath`** strips the builder's absolute source paths from the binary. Checked
  directly: `strings backup-manager | grep -E '/Users/rom|/src/'` returns nothing.
- **`-buildvcs=false`** so the build doesn't stamp VCS state read off a `.git` directory
  that may or may not even be in the build context (`.dockerignore` excludes `.git`
  deliberately, for this exact reason).
- **Deterministic build stamps**: `VERSION` and `COMMIT` are Docker build `ARG`s supplied
  from outside (`container/compose.yaml`'s `build.args`, or `--build-arg` on a direct
  `docker buildx build`), never discovered by the Dockerfile shelling out to `git`
  itself. Same two inputs always produce the same `-ldflags -X main.version=... -X
  main.commit=...`. Compute them from a real checkout:

  ```
  VERSION=$(git describe --tags --always)
  COMMIT=$(git rev-parse HEAD)
  ```

  Left unset, they default to `dev`/`none`, which is fine for a local smoke build and not
  what a real release should ship.
- Module and build caches are BuildKit cache mounts (`--mount=type=cache`), which don't
  persist into the image or affect the compiled output's bytes, only how long a rebuild
  takes.

## Minimal runtime image

Multi-stage build: the `golang` builder is discarded, and the final image is
`gcr.io/distroless/static-debian12:nonroot` plus the one binary. No shell, no package
manager, no libc (none needed, see `CGO_ENABLED=0` above).

Built and measured directly, both architectures:

| Architecture  | Built | Ran | Image size |
|---------------|:-----:|:---:|-----------:|
| linux/arm64   | yes   | yes, natively (this is an Apple Silicon host) | 17.3 MB |
| linux/amd64   | yes   | yes, under QEMU emulation (no native amd64 host available here) | 18.5 MB |

Both were built with `docker buildx build --platform linux/<arch> ...` from
`container/Dockerfile`, and both ran `backup-manager version` successfully and printed
the expected version/commit/Go-version line. `docker compose build` (which does not
cross-build; see below) plus `docker compose run --rm backup-manager` was also exercised
end to end on linux/amd64, with the full read-only-rootfs/tmpfs/non-root/bind-mount shape
from `container/compose.yaml` in effect, not just a bare `docker run`.

To publish a genuinely multi-arch tag (a single reference that resolves to either
architecture), build both platforms in one invocation and push:

```
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse HEAD) \
  -f container/Dockerfile \
  -t <registry>/backup-manager:<version> \
  --push \
  .
```

`container/compose.yaml`'s `build:` section, by contrast, builds for whatever machine
runs `docker compose build` (i.e. it's meant to be run **on** the UGREEN NAS itself, or a
dev machine matching its architecture) — compose isn't the tool for cross-building a
multi-arch manifest, buildx directly is.

## Read-only application filesystem, and where SQLite actually needs to write

`container/compose.yaml` sets `read_only: true`. Two things have to still be writable
under that, and I checked both directly against a container built from this exact image
rather than assuming:

**The state directory, not just the database file.** `internal/state/state.go` opens
SQLite with `journal_mode=WAL`, which keeps a `-wal` and a `-shm` file alongside the main
`.db` file while it's open. A single-file bind mount for just the database would leave
those siblings unable to be created. `container/compose.yaml` mounts a whole directory at
`/data/state` for this reason. Verified: a small Go program using the exact same
`sql.Open` + pragma sequence as `internal/state/state.go`, run inside this image with
`--read-only` and only `/data/state` bind-mounted writable, inserted 20,000 rows, built an
index, and read them back successfully.

**Somewhere for temp files.** A genuinely read-only rootfs makes Go's default temp
directory (`/tmp`, when `TMPDIR` is unset) unwritable — I checked this directly with a
throwaway probe binary: `os.CreateTemp("", ...)` fails with "read-only file system" under
`--read-only` with no `/tmp` mount, and succeeds once a `tmpfs` is mounted there.
Interestingly, forcing `PRAGMA temp_store = FILE` and running `VACUUM` (a query that all
but guarantees a real temp file in the C sqlite implementation) through
`modernc.org/sqlite` v1.57.0 *didn't* fail even without a writable `/tmp` in my testing —
its temp-file path apparently doesn't always need real disk backing for a moderate
workload. That's a "didn't reproduce a failure," not a guarantee it never needs one on a
larger journal, a different query shape, or a future driver version, and provisioning a
64 MB `tmpfs` at `/tmp` costs nothing, so `container/compose.yaml` mounts one anyway and
sets `TMPDIR=/tmp` explicitly rather than relying on the fallback search order
(`SQLITE_TMPDIR`, then `TMPDIR`, then a hardcoded list ending in `/tmp`) to land somewhere
writable by luck.

Everything else under `/` is the image's own read-only content (the binary, the TLS root
store, timezone data) and was never meant to be written to.

## Non-root and the NAS uid/gid

`gcr.io/distroless/static-debian12:nonroot` already defaults to a non-root account
(uid/gid 65532, "nonroot" in its `/etc/passwd`), which would be enough in isolation. It
isn't enough here, because this container also has to write into a directory that lives
on the UGREEN NAS's actual filesystem (the state volume, and the backup storage volume),
and that directory's ownership is whatever the NAS's admin account happens to be, not
whatever uid the base image picked at build time.

**Assumption, stated explicitly**: `container/.env.example` defaults `PUID`/`PGID` to
`1000`, the common first-admin-account uid/gid on Linux-based NAS distributions (UGOS
included). This is a default to override, not a hardcoded value: `container/compose.yaml`
reads `user: "${PUID:-1000}:${PGID:-1000}"` from the environment, so a UGREEN host where
the admin account isn't 1000 just needs a different `.env`, not a different image or a
different compose file.

This image has no shell and no root-then-drop-privileges init step (that would need
`privileged`-adjacent capabilities this container deliberately doesn't have), so it
cannot `chown` the mounted directories for you at startup. **Whatever `PUID`/`PGID` you
set has to already own `STATE_DIR` and `BACKUP_DIR` on the host before the first start**,
e.g. `chown -R 1000:1000 /volume1/backup-manager/state /volume1/backups` on the NAS
itself, matching whichever PUID/PGID you put in `.env`.

One honest limitation: I built and ran all of this on macOS with Docker Desktop, whose
bind-mount layer does not enforce Linux file ownership/permission bits the way a native
Linux host (like the UGREEN NAS's own OS) does — a uid/gid mismatch that would fail with
"permission denied" on real Linux went through unchallenged in this sandbox. The
`PUID`/`PGID` + pre-chown guidance above is standard Linux permission semantics, not
something I was able to independently reproduce a failure for in this environment.

The image also doesn't rely on `$HOME`: `internal/transport/rclone/ssh.go` and
`internal/config` pass `known_hosts`/`key_file` paths through rclone's
`env.ShellExpand`, which only expands `~` for a path that literally starts with `~` and
otherwise leaves it alone. Keep config paths absolute (as `container/.env.example`
does) and this doesn't come up, which matters because `/home/nonroot` in the base image
is owned by uid 65532 specifically, not by whatever `PUID` you set.

## Mounted backup storage, and read-only credentials/configuration

`container/compose.yaml` bind-mounts five things:

- `/data/state` (writable): the SQLite journal directory, see above.
- `/data/backups` (writable): the NAS backup volume/share completed artifacts land on.
- `/etc/backup-manager/config.yaml` (`:ro`): the manager's YAML config (FR-5).
- `/etc/backup-manager/id_ed25519` (`:ro`): the SFTP client private key.
- `/etc/backup-manager/known_hosts` (`:ro`): the pinned host keys (FR-6).

All five host paths come from environment variables (`STATE_DIR`, `BACKUP_DIR`,
`CONFIG_FILE`, `SSH_KEY_FILE`, `KNOWN_HOSTS_FILE`), documented with no real values in
`container/.env.example`. None of them have a fallback default in `compose.yaml` itself:
they're declared with `${VAR:?...}`, so `docker compose up` fails immediately with a
clear message if one is missing, rather than silently bind-mounting some made-up default
path. Nothing resembling a credential is ever written into `container/compose.yaml`,
`container/Dockerfile`, or `container/.env.example` itself; the actual key material only
ever exists at the host path the operator points `SSH_KEY_FILE` to.

## No privileged mode

`container/compose.yaml` never sets `privileged: true` (explicitly `privileged: false`),
drops every capability (`cap_drop: [ALL]`), and sets `no-new-privileges:true`. Nothing
this process does (open files, make outbound SSH/SFTP connections, run as a fixed
non-root uid) needs any capability at all.

## Restart policy

`restart: unless-stopped`, which is the right policy for the eventual long-running
`run`/`daemon` process: come back after a crash or a NAS reboot, stay down if an operator
deliberately stops it.

Today, `command: ["version"]` (the only subcommand that exists) exits 0 immediately, so
`unless-stopped` will keep restarting it in a loop — Docker's restart backoff throttles
how fast, but it will not settle into "up." That's expected, not a bug in this file: it's
what "a restart policy exists" looks like before there's a process meant to stay up. Use
`docker compose run --rm backup-manager` for a one-shot check (this is what I used for
end-to-end verification) rather than `up -d`, until `command` is updated to `["run"]` or
`["daemon"]`.

## Health check

`backup-manager status` (issue #26) doesn't exist yet, so there's no way today to ask
"is this service actually healthy" the way FR-24's `HEALTHY`/`DEGRADED`/`STALE`/`FAILING`
states are meant to be read — the failure-safety invariants are explicit that process
liveness and backup freshness are different facts, and a health check can't answer a
question the binary has no way to compute yet.

The `HEALTHCHECK` in `container/Dockerfile` asks the narrower question that's actually
answerable today:

```
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/backup-manager", "version"]
```

This exits non-zero only if the binary itself can't start and run, which is a real (if
minimal) signal, not a placeholder that always reports healthy. It is deliberately not
claiming to check backup health.

**TODO(#26)**: replace this `CMD` with `["/backup-manager", "status"]` (or whatever flag
makes that subcommand exit non-zero on `DEGRADED`/`STALE`/`FAILING`) once it ships. The
Dockerfile carries the same marker inline.

## Building and running it yourself

```
# One architecture, loaded into the local Docker for testing:
docker buildx build --platform linux/arm64 \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse HEAD) \
  -f container/Dockerfile -t backup-manager:dev --load .

docker run --rm --platform linux/arm64 backup-manager:dev version

# The full deployment shape, via compose:
cp container/.env.example container/.env   # then edit the paths for real
docker compose -f container/compose.yaml build
docker compose -f container/compose.yaml run --rm backup-manager
```
