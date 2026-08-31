# TrueNAS provider acceptance procedure

**Status: NOT EXECUTED. TrueNAS is build-supported and uncertified (§68).**

This is the procedure an operator runs on a real TrueNAS system to certify the
package in `apps/truenas/`. I wrote it before the metadata existed, per §68's
"provider acceptance procedures SHALL be written/version-controlled before manual
execution", and nothing in it has been run: no TrueNAS instance, VM or physical,
was available on the machine the package was built on.

Required evidence per §68: a **current supported TrueNAS release VM or hardware**.
TrueNAS Apps have been Docker Compose based since 24.10 (Electric Eel), which is
the floor this package targets; record the exact release you used in the evidence
table.

Everything the repository itself can decide (metadata well-formedness, image
reference parity, backup-root containment, absence of lifecycle code, absence of
bundled secrets) is already checked by `apps/common/packaging` on every commit.
Do not re-check those by hand. This procedure covers only what a laptop cannot
reach.

---

## Terminology

| Term | Meaning here |
| --- | --- |
| `POOL` | The ZFS pool you install into. The package's defaults assume `tank`; substitute yours everywhere. |
| Engine container | `/backup-manager-web serve`: API, scheduler, local authentication. No published port. |
| Web UI container | `/backup-manager-web serve-ui`: static UI plus reverse proxy. The only published port. |
| Canonical image | The single OCI reference in `apps/common/packaging/canonical.json`. |

---

## Step 0 — Prerequisites

### 0.1 Make the canonical image resolvable

No registry is configured for this repository yet (`container/release-manifest.json`
says so, and so does `apps/common/packaging/canonical.json`). Until one is, the
reference in the metadata resolves to nothing. Pick one:

**Option A, your own registry.** Build and push both architectures, then override
the image reference at install time:

```bash
docker buildx build \
  --platform=linux/amd64,linux/arm64 \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -f container/Dockerfile \
  -t <your-registry>/backup-manager:<version> \
  --push .
```

**Option B, side-load.** Build for the NAS's architecture, save, copy, load:

```bash
docker buildx build --platform=linux/amd64 -f container/Dockerfile \
  -t backup-manager:<version> --load .
docker save backup-manager:<version> | gzip > backup-manager.tar.gz
scp backup-manager.tar.gz root@<truenas>:/mnt/POOL/
ssh root@<truenas> 'gunzip -c /mnt/POOL/backup-manager.tar.gz | docker load'
```

Record which option you used and the exact reference in the evidence table. If you
side-loaded, also record the image ID and compare it against
`container/release-manifest.json`'s `local_image_id_sha256` for the matching
architecture.

- [ ] Canonical image resolvable on the NAS, reference recorded

### 0.2 Create the datasets

The package's host-path defaults come from `apps/common/packaging/canonical.json`
(`platforms.truenas.hostPaths`) and are the same values the TrueNAS frontend bridge
declares. Create them as datasets, not directories, so snapshots and quotas work:

```bash
zfs create -p POOL/backup-manager/state
zfs create -p POOL/backup-manager/backups
zfs create -p POOL/backup-manager/config
zfs create -p POOL/backup-manager/secrets
```

Then confirm all four are actually mounted before you install anything:

```bash
zfs list -o name,mountpoint -r POOL/backup-manager
for d in state backups config secrets; do
  mountpoint -q "/mnt/POOL/backup-manager/$d" || echo "NOT MOUNTED: $d"
done
```

This is the check the compose profiles get for free from `${VAR:?message}` and
this one cannot: the catalog and the paste-in compose both carry literal host
paths, so there is no unset variable to refuse. An unmounted dataset is not an
error either, because Docker creates a missing bind-mount source, so the app
would start and write the retained artifacts onto the pool's root filesystem
instead of into the dataset that is snapshotted and quota'd.

- [ ] Four datasets exist
- [ ] All four are mounted, checked immediately before install

### 0.3 Own them by the uid/gid the app runs as

The runtime image is distroless. It has no shell, no root step and no init
process, so nothing inside the container can fix ownership for you at startup
(`container/compose.yaml` documents the same constraint for the generic app).

Pick the uid/gid the app will run as and set it now. TrueNAS's own `apps` account
is `568:568` and is the conventional choice:

```bash
chown 568:568 /mnt/POOL/backup-manager \
  /mnt/POOL/backup-manager/{state,backups,config,secrets}
chmod 700 /mnt/POOL/backup-manager/secrets
```

The mountpoints are chowned, not the trees beneath them. On a first install that
is the same thing, because the datasets are empty. On a reinstall it is not: by
then `backups` is the retained backup store, and a recursive chown would rewrite
the ownership of every artifact in it with no record of what the ownership was.
Nothing in this app needs that, so nothing here does it.

- [ ] `PUID`/`PGID` chosen and recorded
- [ ] All four dataset mountpoints owned by that uid/gid
- [ ] No recursive ownership change was made to the backups dataset

### 0.4 Create the SSH key, the pinned known_hosts, and the config

`/backup-manager-web serve` calls `core/service.Open`, which loads **and validates**
the config file before the HTTP listener ever starts. A missing or invalid
`config.yaml` is a hard startup failure, not a first-run wizard. Create all three
before the first start.

```bash
ssh-keygen -t ed25519 -N '' -f /mnt/POOL/backup-manager/secrets/id_ed25519
ssh-keyscan -t ed25519 <your-sftp-host> > /mnt/POOL/backup-manager/secrets/known_hosts
chmod 600 /mnt/POOL/backup-manager/secrets/id_ed25519
chown 568:568 /mnt/POOL/backup-manager/secrets/*
```

Verify the host key fingerprint out of band before you trust it. Then write
`/mnt/POOL/backup-manager/config/config.yaml`; the container-side paths in it are
fixed by the package and must not be changed (see
`apps/truenas/README.md` for the annotated example, and
`scripts/deploy/deploy_generic.py`'s `render_config_yaml` for the authoritative
shape).

**Never commit the private key, the config, or any transcript containing them.**

- [ ] Key pair generated, mode 0600, owned by `PUID:PGID`
- [ ] `known_hosts` pinned, fingerprint verified out of band
- [ ] `config.yaml` written and readable by `PUID:PGID`

---

## Step 1 — Install (custom app)

1. In the TrueNAS Web UI go to **Apps → Discover Apps → Custom App**.
2. Choose **Install via YAML**.
3. Paste the whole of `apps/truenas/compose/backup-manager.yaml`.
4. Substitute, at the top of the pasted YAML only:
   - the image reference from step 0.1, if you did not push to the recorded one;
   - `POOL` in each host path;
   - `PUID`/`PGID` from step 0.3.
5. Name the app `backup-manager`.
6. Install.

Record: how long the install took, and the full text of any warning TrueNAS showed.

- [ ] Install completed without error
- [ ] TrueNAS shows the app, and both containers reach **running**
- [ ] The engine container reaches Docker health **healthy** (it inherits the
      image's own `HEALTHCHECK`, `/backup-manager status`)
- [ ] The Web UI container reaches Docker health **healthy** (it overrides that
      healthcheck with `/backup-manager-web healthcheck`, because it has no config
      file and no state database of its own to report on)

If the Web UI container is unhealthy while the engine is healthy, the override did
not apply; that is a package bug, not an environment problem. Capture
`docker inspect --format '{{json .Config.Healthcheck}}' <web-ui container>` before
changing anything.

---

## Step 2 — Web portal link

1. Open **Apps → Installed → backup-manager**.
2. Click the **Web Portal** button.

- [ ] The portal button exists and is not greyed out
- [ ] It opens the shared Web UI on the published port, not a 404 and not the
      engine's own port
- [ ] The URL matches what `apps/truenas/catalog/questions.yaml` declared as the
      portal, on whatever port you chose at install time

---

## Step 3 — Authentication (local-account only)

TrueNAS gets no auth of its own. It uses the reusable local authentication the
generic Web host already provides (§13A). This step proves that, and proves the
package ships no credential of its own.

1. Read the one-time enrollment link out of the **engine** container's log:

   ```bash
   docker logs <engine container> 2>&1 | grep -i enroll
   ```

2. Open it. It should present the enrollment screen, not a login screen.
3. Enrol an administrator with a password you generate now. Do not reuse a
   TrueNAS account password, and do not write it into this repository.
4. Log out. Log back in.
5. Open the enrollment link a second time.

- [ ] No account exists before enrollment (the UI offers enrollment, not login)
- [ ] The enrollment token appears only in the container log, never in any file
      under `apps/truenas/`
- [ ] Enrollment succeeds
- [ ] Logout then login succeeds
- [ ] The enrollment link is refused the second time (single-use)
- [ ] `GET /api/v1/system/capabilities` reports `nativeAuth: false`
- [ ] `/mnt/POOL/backup-manager/state/local-auth.json` exists and contains an
      Argon2id hash, never a plaintext password

---

## Step 4 — Storage mapping and backup-root containment

1. Configure one backup set against your SFTP host and run one cycle to
   completion.
2. Then, on the NAS:

```bash
ls -la /mnt/POOL/backup-manager/backups
ls -la /mnt/POOL/backup-manager/state
grep -rIl 'PRIVATE KEY' /mnt/POOL/backup-manager/backups || echo "clean"
```

Then record a baseline for the removal check at the end of this procedure. The
removal criterion is that the backup root is untouched, and a criterion with
nothing to compare against is one an operator ticks off a directory listing: a
partial deletion, a truncated artifact or a silently rewritten file would all
pass it. So write a canary of known content into the backup root, and record its
hash and a full file listing **outside** the backup root, where whatever might
damage that tree cannot reach the evidence:

```bash
mkdir -p /root/backup-manager-acceptance
head -c 8M /dev/urandom > /mnt/POOL/backup-manager/backups/canary.bin
sha256sum /mnt/POOL/backup-manager/backups/canary.bin | tee /root/backup-manager-acceptance/canary.sha256
find /mnt/POOL/backup-manager/backups -type f -printf '%p %s\n' | sort > /root/backup-manager-acceptance/backup-root.before
```

Keep `/root/backup-manager-acceptance` off the repository: the listing names your own backup
sets. Record only that it was taken, and the canary's hash, in the evidence table.

- [ ] At least one completed artifact is under the backups dataset
- [ ] `state.db` (and its `-wal`/`-shm` siblings) are under the state dataset,
      **not** under the backups dataset
- [ ] `local-auth.json` is under the state dataset, not the backups dataset
- [ ] No private key, `known_hosts`, or auth state anywhere under the backups
      dataset (§19.2)
- [ ] A sidecar recovery manifest sits next to the artifact and contains no
      secret material (§19.3)
- [ ] `canary.bin` written into the backup root and its hash recorded outside it
- [ ] A full `find` listing of the backup root recorded outside it

---

## Step 5 — Update

This is the criterion that most often fails silently, so do it on a system that
already has real state from step 4.

1. Note the current app version and image reference.
2. Capture a baseline first, from a shell on the TrueNAS host, so the checks
   below are a comparison rather than an impression:
   ```bash
   sha256sum <state dataset>/state.db | tee /tmp/before-update.sha256
   find <backups dataset> -type f -printf '%p %s\n' | sort > /tmp/before-update.txt
   ```
3. Push or side-load a newer image tag.
4. In TrueNAS, **Apps → Installed → backup-manager → Edit**, change the image tag,
   and save. TrueNAS recreates both containers.
5. Compare afterwards:
   ```bash
   find <backups dataset> -type f -printf '%p %s\n' | sort > /tmp/after-update.txt
   diff /tmp/before-update.txt /tmp/after-update.txt
   ```

- [ ] Update completes and both containers return to healthy
- [ ] `diff` of the retained-artifact listing is empty: the update moved no
      backup data
- [ ] The administrator account still exists (no re-enrollment prompt)
- [ ] The session cookie may be invalidated by the restart; logging back in with
      the same password works
- [ ] Every backup set from step 4 is still configured
- [ ] Every artifact from step 4 is still present and still listed in the UI
- [ ] `state.db`'s modification time changed but its content survived (the
      catalog was migrated, not recreated empty)

---

## Step 6 — Container replacement

Distinct from step 5: the same image, but a destroyed and recreated container.
This is what a NAS reboot, a `docker system prune`, or a TrueNAS app rollback does.

```bash
docker rm -f <engine container> <web-ui container>
```

Then let TrueNAS restart the app (or **Stop** then **Start** it in the UI).

- [ ] Both containers come back healthy
- [ ] Retained backup data survives untouched
- [ ] The catalog survives (same artifact list, same backup sets)
- [ ] The administrator account survives

---

## Step 7 — Remove

This is the destructive-safety step. Its evidence was captured back in the
storage step, because after the delete there is nothing left to compare against,
"the dataset looks fine" is not a result, and any deletion the comparison turns
up is a release blocker rather than a finding to triage.

1. **Apps → Installed → backup-manager → Delete**.
2. When TrueNAS asks, do **not** tick anything that deletes the app's datasets.

- [ ] Both containers are gone

Check the backup root against the baseline recorded in the storage step, before
looking at anything else:

```bash
sha256sum -c /root/backup-manager-acceptance/canary.sha256
find /mnt/POOL/backup-manager/backups -type f -printf '%p %s\n' | sort > /root/backup-manager-acceptance/backup-root.after
diff /root/backup-manager-acceptance/backup-root.before /root/backup-manager-acceptance/backup-root.after
```

- [ ] `sha256sum -c` reports the canary `OK`
- [ ] The `diff` against the recorded listing is empty, so the backup root is
      untouched, byte for byte, and every artifact is still readable
- [ ] The state dataset is either untouched or removed exactly as the dialog
      said it would be, with no surprise
- [ ] Reinstalling with the same host paths adopts the existing catalog rather
      than starting empty

Also run the destructive-safety half: repeat the delete with the "delete app data"
option ticked, on a scratch install only, and confirm TrueNAS never touches a path
outside the ones the package declared.

- [ ] Deleting the app never removes anything outside the declared host paths

---

## Step 8 — Catalog contribution readiness

`apps/truenas/catalog/` is the contribution-ready structure for the TrueNAS Apps
catalog. Nothing on a developer laptop can run TrueNAS's own catalog validator, so
that check lives here.

1. Clone the TrueNAS apps repository.
2. Copy `apps/truenas/catalog/` in as `ix-dev/community/backup-manager/`.
3. Run that repository's own validation and render tooling.

- [ ] The catalog validator accepts the app
- [ ] The rendered compose matches `apps/truenas/compose/backup-manager.yaml`
      apart from values the questions supply
- [ ] Every question in `questions.yaml` is consumed by the template, and every
      template variable is answered by a question
- [ ] The app installs from the rendered catalog entry, not only from the pasted
      custom-app YAML

---

## Evidence (§68)

Fill this in in the same commit that flips TrueNAS from uncertified to certified.

| Field | Value |
| --- | --- |
| Provider / OS version | |
| Hardware or VM, model | |
| Architecture | |
| Package / image version | |
| Image reference used, and how it was made resolvable | |
| Install result | |
| Auth result | |
| Storage result | |
| Update result | |
| Uninstall / removal result | |
| Retained-backup safety | |
| Catalog validator result | |
| Evidence (logs, screenshots, transcripts, with secrets redacted) | |
| Executed by | |
| Date | |
