# UGOS Pro `.UPK` acceptance procedure

The hardware procedure for the UGOS App Center package (issue #83, D1.2),
written before the package was built and then run against a real UGREEN
NAS. It is the same shape as every other acceptance procedure in
`docs/acceptance/`: numbered steps, each one saying what is done, what is
observed, and what would make it a failure.

Section 4 records the run. Where a step could not be executed it says so,
names why, and leaves the claim open rather than describing what would
probably have happened.

## 0. What this procedure is holding the package to

The one rule everything else hangs off:

> The `.UPK` packages the exact canonical image and binary content for
> that release and architecture. It may not compile a behaviourally
> different UGOS build.

So this procedure is not mainly about whether the app installs. It is
about whether the bytes inside the package are the release's bytes, and
the install steps are what make that claim matter rather than what makes
it true.

### 0.1 What you need

- A developer-authorized UGOS Pro NAS with Docker and `ugcli`
  (`tools/ugcli-install/` puts `ugcli` on it).
- A machine with Docker and Go, to stage and verify from. `ugcli` is
  Linux-only, so packing happens on the device; staging deliberately does
  not, because that is where the verifier runs.
- Host directories, created and owned before the first start. The runtime
  image is distroless: it has no shell, no root step and no init, so
  nothing inside the container can create or chown anything for you.

```
/volume1/docker/backup-manager/state
/volume1/docker/backup-manager/config
/volume1/docker/backup-manager/secrets/id_ed25519      (mode 0600)
/volume1/docker/backup-manager/secrets/known_hosts
/volume1/backups/backup-manager
```

### 0.2 The unpublished release

0.3.0 is cut and not pushed. `distribution/packaging/canonical.json` says
`image.published: false` and `container/release-manifest.json` records
`registry_digest: null` per architecture, so there is no published digest
for the package to be fetched at.

That splits the identity claim in two, and the procedure treats the halves
differently rather than pretending they are one:

- **Binary content parity runs today.** The release manifest records each
  architecture's binary SHA-256 from the build, not from a push, so the
  strongest half of the claim is checkable now.
- **Registry digest parity is deferred**, visibly, by name, with the
  reason recorded in the report. It binds itself the moment the push fills
  the digest in, with no edit to any check.

Step 0 of every acceptance procedure in this repository already covers a
deployment that cannot reach the canonical reference. Here that step is
`build-upk.sh --source <reference>@sha256:<digest>`, which refuses to
guess and records verbatim whatever you name.

## 1. Stage and verify (off the device)

```
apps/ugos/upk/build-upk.sh --arch amd64
```

**Observe.** The script pulls by digest, tags the pull as the canonical
reference, saves it into `rootfs_amd64/images/`, writes
`image-source-amd64.json`, draws the icon, and runs
`distribution/cmd/upkverify` over the staged tree.

**Pass.** Every check that can run passes, and the verdict is `PASS` or
`PASS WITH DEFERRALS`.

**Fail.** Any `FAIL`. The script stops and does not leave a stage anyone
could pack. In particular `binary-content-parity` failing means the bytes
are not the release's bytes, and no amount of packaging correctness
excuses it.

## 2. Prove the check can fail

A check that only ever passes on the good package says nothing. Before
trusting step 1, watch it refuse a bad one.

```
docker build -t ghcr.io/spdrman/backup-manager:0.3.0 -            <<'EOF'
FROM ghcr.io/spdrman/backup-manager:0.3.0
COPY tampered /backup-manager
EOF
docker save ghcr.io/spdrman/backup-manager:0.3.0 -o <stage>/rootfs_amd64/images/backup-manager-0.3.0-amd64.tar
go run ./distribution/cmd/upkverify -stage <stage> -arch amd64 -manifest container/release-manifest.json
```

**Pass.** `binary-content-parity` fails naming the binary and both
hashes, the verdict is `FAIL`, and the command exits non-zero.

**Fail.** Anything else, including a pass, and including a failure that
names a different check: this control exists to prove the content rule
bites specifically, and a stage that is rejected for being malformed
proves nothing about content.

## 3. Pack (on the device)

```
scp -r <stage> <nas>:~/upk        # or tar over ssh
ssh <nas> 'cd ~/upk && ugcli check && ugcli pack --arch amd64 --build 1'
```

**Observe.** `ugcli check` passes. `ugcli pack` writes
`build_dir/pkgs/upk/amd64_com.spdrman.backupmanager_<version>.<build>.upk`.

**Pass.** A signed `.upk` exists.

**Fail.** `ugcli check` refuses the project, or `pack` produces nothing.

## 4. Install through the App Center

**This step is outstanding.** See section 6.

## 5. Open, authenticate, configure, run, update, uninstall

Also gated on section 6, except for the parts that can be observed
without an App Center install, which are steps 5.1 to 5.5 below and which
were run.

### 5.1 The bundled image loads and is the release's

```
docker load -i rootfs_amd64/images/backup-manager-<version>-amd64.tar
cid=$(docker create ghcr.io/spdrman/backup-manager:<version> /backup-manager version)
docker cp $cid:/backup-manager .            && sha256sum backup-manager
docker cp $cid:/backup-manager-web .        && sha256sum backup-manager-web
```

**Pass.** Both hashes are the ones `container/release-manifest.json`
records for this architecture. This is the identity claim closed on the
device rather than on the staging machine: the bytes the NAS actually
loaded, hashed on the NAS.

### 5.2 The deployment comes up and answers

```
docker compose -f rootfs_common/docker-compose.yaml up -d
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:<port>/health/live
```

**Pass.** Both containers report healthy, the engine's health gate
releases the Web UI, and `/health/live` answers 200.

### 5.3 The UGOS runtime profile is the one that is running

```
curl -s -H 'X-Ugos-User: <admin>' http://127.0.0.1:<port>/api/v1/system/capabilities
```

**Pass.** `"platform":"ugos"` and `"native_auth":true`, from the canonical
binary with no UGOS build anywhere.

### 5.4 A forged identity header from a direct peer is refused

The negative control, and the positive one beside it, because either
alone is unreadable.

```
# negative: a peer on the app's own network that is not the gateway
docker run --rm --network <project>_internal busybox \
  wget -S -O /dev/null --header='X-Ugos-User: <admin>' http://backup-manager-ui:8080/api/v1/system/capabilities

# positive: the same header from the gateway address
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Ugos-User: <admin>' \
  http://127.0.0.1:<port>/api/v1/system/capabilities
```

**Pass.** The direct peer gets 401 with the header and 401 without it,
and the gateway peer gets 200. A pass on the negative case alone would
also be produced by an app that rejects everything, which is why the
positive control is part of the same step.

### 5.5 The deployment needs no host privilege

```
docker inspect <engine> <webui> --format '...'
```

**Pass.** For both containers: `Privileged=false`, no host network, PID
or IPC namespace, `ReadonlyRootfs=true`, `CapDrop=[ALL]`,
`no-new-privileges:true`, a non-root user, and no Docker socket among the
binds. The engine publishes no port at all; the Web UI publishes exactly
one, bound to loopback.

### 5.6 Container replacement preserves data

```
docker compose down            # no -v
docker compose up -d
```

**Pass.** Everything under the backup root and the state directory is
still there afterwards, and the deployment comes back healthy.

## 6. What is outstanding, and why

**The App Center install click.** The account available on the device is
SSH-only. `/ugreen/@appstore/` and `/var/ugreen/` are root-owned, `sudo`
needs a password nobody has entered, and `ugcli` has no install
subcommand: it creates, checks and packs, and nothing more. So there is no
non-interactive path from a built `.upk` to an installed app, and no
write call was made against any App Center interface.

Everything gated on that click is therefore open, and stays open:

- installing the `.UPK` through the App Center;
- the desktop tile and its icon;
- `open_type: inner` opening inside the UGOS desktop;
- authentication through the real UGOS gateway rather than through the
  synthetic gateway peer step 5.4 uses;
- update, disable, enable and uninstall as App Center operations, and
  what each of them does to `/volume1/docker/backup-manager` and
  `/volume1/backups/backup-manager`.

What that leaves is a package proven correct in content and in runtime
behaviour, and unproven in App Center lifecycle. Those are different
claims and this document does not merge them.

**arm64 hardware.** The package stages and verifies for arm64 (section
4's record includes it), and no arm64 UGREEN device was available to run
it on. The architecture claim comes from the release, once; the hardware
claim is per device, and only amd64 has one.

## 7. The recorded run

Run on 2026-09-05 against a real UGREEN NAS (amd64, UGOS Pro, Docker
29.4.3, `ugcli` 1.1.0.13 installed from `tools/ugcli-install/`). The
device address is deliberately not recorded here.

### 7.1 Stage and verify, both architectures

`build-upk.sh` first refused, correctly, with no `--source`:

```
ERROR: ghcr.io/spdrman/backup-manager:0.3.0 records no amd64 registry digest.
The release is cut and not pushed ... Nothing here will guess one.
```

Staged from the release's own published bytes, both architectures:

```
amd64  fetched at sha256:11998a454102707bc84d66a2a6b6445438361e93d63130d1a4c9a6cf6561caec
  PASS     binary-content-parity: /backup-manager=af9ab4b40141, /backup-manager-web=8987d7348b69
arm64  fetched at sha256:ab1f27c9c1394de3094e65392709a96363a4d3b9f40ea4038745bc87cf2babbf
  PASS     binary-content-parity: /backup-manager=6cce5d45885e, /backup-manager-web=27dd58b4bffe
  DEFERRED registry-digest-parity: ... is cut and not pushed ...
VERDICT: PASS WITH DEFERRALS.
```

Both sets are exactly what `container/release-manifest.json` records for
those architectures.

### 7.2 The check refusing a divergent package

A real image built `FROM` the canonical one with `/backup-manager`
replaced, staged with the correct tag, the correct architecture and an
honest provenance sidecar:

```
  PASS     stage-layout / package-version / image-reference / image-architecture
  FAIL     binary-content-parity: /backup-manager hashes to 09eb32f0c0788fe0...
           and the release recorded af9ab4b4014161eb... — this package would
           ship a build the canonical release did not produce
VERDICT: FAIL. This package must not be packed.
```

Exit status 1. Every other check passed, which is the finding: a
divergent image is invisible to all of them.

### 7.3 `ugcli check` and `ugcli pack`

`ugcli check` passed, with five advisory warnings, all the same one:

```
[WARN] service 'backup-manager': volume mounts absolute host path
       '/volume1/backups/backup-manager', it's not recommended
```

Not followed, deliberately. UGREEN's recommendation is that an app keep
its data inside its own install directory; the backup root is user data
on a share and must outlive an uninstall of the app, so putting it inside
the app directory is the one thing §19.2 says not to do. Private state and
configuration sit under `/volume1/docker/backup-manager` for the same
reason in reverse: they are the app's, not the user's.

`ugcli pack --arch amd64 --build 1` produced
`amd64_com.spdrman.backupmanager_0.3.0.0001.upk`, 17,158,322 bytes,
signed by `ugcli`.

The packed artifact begins with the bytes `UGREEN-PKG-FORMA...` and is
neither a tar nor a zip. That is why the verifier runs over the staged
tree: the last moment the packaged bytes are readable is before `pack`,
which is also the last moment a refusal can still prevent a bad package
rather than describe one.

### 7.4 On the device

`docker load` of the staged tar, then the binaries copied straight back
out of the loaded image and hashed on the NAS:

```
af9ab4b4014161eba8f8fb8cf072229a96a80e5fde5a907414d5d15542b47bbe  /backup-manager
8987d7348b690a1bc48e7e81f0ec69a6ab88781c5df8057c072598cfa4d43fd8  /backup-manager-web
```

The release manifest's amd64 row, byte for byte, hashed on the machine
that ran them.

`docker compose up -d` on the package's own wrapper:

```
backup-manager-1     Up (healthy)
backup-manager-ui-1  Up (healthy)
health/live HTTP 200
network 172.29.83.0/24 gw=172.29.83.1
```

The pinned subnet came up as declared, so `TRUSTED_GATEWAY_CIDRS`
(`172.29.83.1/32`) names the address the gateway's traffic actually
arrives from.

Capabilities, through the gateway peer:

```
{"platform":"ugos","native_auth":true,"native_notifications":false,
 "storage_picker":false,"embedded_window":false,"app_store_packaging":false}
```

The trusted-gateway boundary, both directions:

| peer | header | result |
| --- | --- | --- |
| `172.29.83.1` (gateway, loopback-published port) | `X-Ugos-User: adm1n` | **200** |
| `172.29.83.4` (a container on the app's own network) | `X-Ugos-User: adm1n` | **401** |
| `172.29.83.4` | none | **401** |
| `172.29.83.1` | none | **401** |

Host privilege, both containers:

```
Privileged=false NetworkMode=<project>_internal PidMode=[] IpcMode=private
ReadonlyRootfs=true CapAdd=[] CapDrop=[ALL]
SecurityOpt=[no-new-privileges:true] User=1000:1000
engine  binds: five named paths, no docker socket   ports: 8080/tcp -> null
web-ui  binds: null                                 ports: 8080/tcp -> 127.0.0.1:28080
```

Container replacement (`down` without `-v`, then `up -d`): both
containers healthy again, `/health/live` 200, and the file written under
the backup root before the cycle still there afterwards. The state
directory was empty across the cycle because the app was never enrolled,
which is a fact about this run rather than about persistence, and is
recorded as one.

### 7.5 One measurement worth keeping

On this device `uid 1000` is `adm1n`, the first admin account, and its
primary group is `admin` (`gid 10`); `gid 1000` is a user group named
`Family`. The wrapper runs `1000:1000`, which worked here because UGOS
leaves `/volume1/docker` and `/volume1/backups` world-writable, so the
container could create and write its own directories regardless of group.
It is not proof that `1000:1000` is right on every UGOS device, and the
App Center install is what would settle what the app actually runs as.

### 7.6 Cleanup

Every container, network and image this run created was removed, and the
device was left with exactly what was running before: one unrelated
`postgresql` container, the three default Docker networks, and no
`backup-manager` image. Nothing already on the device was touched, and no
write call was made against any App Center interface.
