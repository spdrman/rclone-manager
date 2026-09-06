# UGREEN UGOS Pro

Two things live here, and they belong to different layers on purpose.

`frontend/` is the **runtime profile bridge**: the capability declaration,
the bootstrap and the auth bridge the shared UI loads when the Web UI host
is started with `--profile=ugos`. It is platform-layer code.

`upk/` is the **distribution adapter**: a `ugcli` project that packages the
canonical release as a UGOS App Center Docker app. It is metadata, a
derived Compose wrapper, an icon and a fetch-and-verify script, and it
compiles nothing at all.

There is no UGOS build of anything. One canonical executable serves every
deployment, and `--profile=ugos` is the whole of what UGOS changes:
platform identity, capability reporting, which UI bundle the Web UI host
serves, and a trusted native authentication gateway. It changes no backup
lifecycle, retention or validation semantics, and
`apps/common/platform/profile` is where that is both stated and enforced.

## The rule the package sits under

> The `.UPK` packages the exact canonical image and binary content for
> that release and architecture. It may not compile a behaviourally
> different UGOS build.

`upk/build-upk.sh` pulls the release **by digest** and saves it. It never
builds. Then it runs `distribution/cmd/upkverify` over the staged tree and
refuses to leave a stage anyone could pack when the bytes are wrong:

```
$ apps/ugos/upk/build-upk.sh --arch amd64
...
  PASS     binary-content-parity: every canonical binary in the archive is the
           amd64 release's own byte-for-byte (/backup-manager=af9ab4b40141, ...)
  DEFERRED registry-digest-parity: ... is cut and not pushed ...
VERDICT: PASS WITH DEFERRALS.
```

The check is `distribution/packaging/upk.go`, and its own doc comment
explains why the binary hashes are the claim and the registry digest is
corroboration: `docker save` re-serialises the manifest, so the digest
ghcr.io assigned does not survive into the archive. What does survive is
the bytes that execute, which is the thing that actually matters.

`upk_test.go` runs every rule against a deliberately divergent package as
well as a good one, including the shape that is easy to miss: the
canonical binary present in a lower layer and replaced in a higher one.

## Building the package

```
apps/ugos/upk/build-upk.sh --arch amd64        # or arm64
```

Then, on a developer-authorized UGOS Pro NAS (`ugcli` is Linux-only, and
`tools/ugcli-install/` puts it there):

```
ugcli check
ugcli pack --arch amd64 --build 1
```

While 0.3.0 is cut and not pushed there is no published digest to pull, so
the script refuses rather than guessing, and tells you to name the source
explicitly:

```
apps/ugos/upk/build-upk.sh --arch amd64 --source <reference>@sha256:<digest>
```

Whatever you name is recorded verbatim in `image-source-<arch>.json`, so
the package always says where its bytes came from. The content is still
held to `container/release-manifest.json` either way, which is the half of
the claim that does not depend on the push.

## What the deployment looks like

Two containers from one image, exactly as `container/compose.yaml`
defines them:

- **the engine** holds the state database, the credentials, the `/api/v1`
  surface and the scheduler, and publishes no port at all;
- **the Web UI** serves the UGOS bundle and reverse-proxies to the engine,
  and is the only container UGOS reaches.

Host paths follow UGOS's own layout, checked against a real device rather
than assumed. Private application data lives under
`/volume1/docker/backup-manager`; retained artifacts land in
`/volume1/backups/backup-manager`, a dedicated child of the backups share
rather than the share itself, so an uninstall that offers to remove the
app's data cannot reach a backup.

`ugcli check` warns that those absolute host paths are "not recommended",
and the package does not follow the recommendation. UGREEN's advice is
that an app keep its data inside its own install directory; the backup
root is user data that has to outlive the app, and putting it inside the
app directory is exactly what §19.2 forbids.

## The trusted gateway

The Web UI's published port is bound to `127.0.0.1`, so the only thing on
the NAS that can reach it is a process on the NAS, which for an
`open_type: inner` app is the UGOS gateway. Docker's userland publishing
presents that traffic as coming from the bridge gateway address, and the
compose file pins the project subnet so that address is exactly
`172.29.83.1`, named as a `/32` in `TRUSTED_GATEWAY_CIDRS`.

The engine's own `TRUSTED_UPSTREAM_CIDRS` is a different and wider set,
the internal subnet, because its only possible peer is the Web UI. Those
two hops trust different peers and one value correct for one of them is
wrong for the other, which is why they are two variables.

Measured on hardware (`docs/upk-acceptance-procedure.md` §7.4): a forged
`X-Ugos-User` from a container on the app's own network gets 401, and the
same header from the gateway address gets 200.

## Host privilege

No privileged mode, no Docker socket, no host networking, no host PID or
IPC namespace, no unbounded host filesystem access. Both containers run
non-root with a read-only root filesystem, `cap_drop: ALL` and
`no-new-privileges`. `distribution/compose` runs the prohibition list over
this wrapper like every other derived artifact, and
`TestTheShippedUGOSPackageNeedsNoProhibitedHostPrivilege` restates it by
name.

## What is not proven

The App Center install click. The device available for this work is
SSH-only, `/ugreen/@appstore/` is root-owned, `sudo` needs a password and
`ugcli` has no install subcommand, so there is no path from a built `.upk`
to an installed app that does not go through a person at the UGOS desktop.
Everything downstream of that click is open: the desktop tile, `inner`
opening, authentication through the real gateway rather than a synthetic
one, and App Center update, disable, enable and uninstall.

arm64 stages and verifies; no arm64 UGREEN device was available to run it
on.

`docs/upk-acceptance-procedure.md` is the procedure and the recorded run,
and it says which of its own steps did not execute rather than describing
what they would have done.
