# ZimaOS app-store install acceptance procedure

Section 68 of `docs/EPIC-B-multi-nas.md` requires this document to exist and be
version-controlled **before** anyone runs it. It has not been run. Every box in
it is unticked and the evidence table at the bottom is empty, which is the
honest state: ZimaOS is build-supported and uncertified.

ZimaOS reads the same `x-casaos` block CasaOS does, so
`apps/zimaos/compose/backup-manager.yml` is both the runtime definition and the
store submission.

It is a separate procedure from the CasaOS one even though the stack is the same
stack, and that is the point rather than duplication: the two are separately
submitted to two stores and separately certified, so a green CasaOS run says
nothing about ZimaOS. Anything you find here that is genuinely a ZimaOS
difference belongs in `apps/zimaos/README.md` as a finding.

Issue #170 adds ZimaOS as new platform support. There was no Phase 4
ZimaOS packaging, so nothing here is a migration from an earlier one.

## Step 0 — Prerequisites

### 0.1 Record the host

- [ ] ZimaOS version (Settings, About) recorded in the evidence table
- [ ] Architecture recorded (`uname -m`)
- [ ] Container engine version recorded

### 0.2 Make the canonical image resolvable

`distribution/packaging/canonical.json` records `published: false`: no registry
is configured for this repository yet, so `ghcr.io/spdrman/backup-manager:1.0.0`
resolves to nothing until you make it resolve. Either push a build to a registry
this host can reach and change the image reference in one place, or build
elsewhere and load it:

```bash
docker buildx build --platform=linux/amd64,linux/arm64 -f container/Dockerfile -t backup-manager:acceptance .
docker save backup-manager:acceptance | ssh admin@<host> 'docker load'
```

- [ ] The image is resolvable on the host, and the exact reference used is recorded

### 0.3 Create the host paths

```bash
mkdir -p /DATA/AppData/backup-manager/state /DATA/AppData/backup-manager/config \
         /DATA/AppData/backup-manager/secrets /DATA/Backups/backup-manager
```

The runtime image is distroless: no shell, no root step, nothing inside the
container can create or chown anything at startup, so the host-side owner has to
be right before the first start.

Create the SSH key and the pinned `known_hosts` **before** the ownership fix-up,
following `docs/ssh-setup.md`. Never commit either, and never paste a private key
into the evidence table.

```bash
ssh-keygen -t ed25519 -N "" -f /DATA/AppData/backup-manager/secrets/id_ed25519
ssh-keyscan -t ed25519 <sftp-host> > /DATA/AppData/backup-manager/secrets/known_hosts
```

**Recurse only over what this step created.** `/DATA/Backups/backup-manager` is the retained
backup store: on a reinstall it already holds data this procedure did not write,
and a recursive ownership change across it rewrites all of it with nothing to
restore it from. So the private trees are chowned recursively and the backup root
gets its own directory chowned, nothing beneath it. `distribution/packaging`
fails the build if any procedure in this directory recurses over a backup root or
a parent of one.

```bash
chown -R 1000:1000 /DATA/AppData/backup-manager/state /DATA/AppData/backup-manager/config /DATA/AppData/backup-manager/secrets
chown 1000:1000 /DATA/Backups/backup-manager
chmod 600 /DATA/AppData/backup-manager/secrets/id_ed25519
```

- [ ] All four paths exist and are owned by the app's uid and gid
- [ ] The recursive ownership change touched only state, config and secrets
- [ ] It ran **after** the key and `known_hosts` were created
- [ ] `/DATA/AppData/backup-manager/config` is writable by the app's uid and gid
- [ ] Key material lives only on this host, redacted everywhere else

---

## Step 1 — Install

1. In ZimaOS, install the app from its store, or use the custom-install route
   with `apps/zimaos/compose/backup-manager.yml`.
2. ZimaOS renders the install dialog out of the `x-casaos` block. Change nothing.
3. Install.

- [ ] ZimaOS accepted the file: no schema error, and the store build validated it
- [ ] The app tile shows the title, icon, category and description from `x-casaos`
- [ ] The install dialog listed the five volumes and the two environment values
      the per-service `x-casaos` blocks describe
- [ ] Both containers reach `running`
- [ ] `backup-manager` reports healthy
- [ ] `backup-manager-ui` reports healthy, having overridden the image's own healthcheck
- [ ] The app claims `amd64` and `arm64`, and it installed on this machine's architecture

## Step 2 — Web UI

- [ ] The published port loads the shared web UI
- [ ] The UI reports the deployment as a Docker Compose deployment, which is what
      `apps/zimaos/README.md` says to expect: this adapter ships no platform
      bridge and serves the bundle compiled into the binary
- [ ] The capability list shows no native authentication, no native
      notifications, no embedded window and no storage picker, all four reported
      as unsupported rather than hidden

## Step 3 — Authentication

- [ ] First start printed a one-time enrollment link (keep it out of the evidence table)
- [ ] Enrollment sets an administrator password, stored as an Argon2id hash
- [ ] The enrollment link is single-use and is rejected the second time
- [ ] An unauthenticated request to `/api/v1/` is refused
- [ ] The UI reports auth mode `local-account`, and no platform identity is trusted

## Step 4 — Storage mapping and backup-root containment

- [ ] Private state lands under `/DATA/AppData/backup-manager/state`
- [ ] Retained artifacts land under `/DATA/Backups/backup-manager`
- [ ] No SSH private key, `known_hosts`, config file or authentication record
      exists anywhere under `/DATA/Backups/backup-manager`
- [ ] The key and `known_hosts` are mounted read-only, and a write attempt from
      inside the container fails
- [ ] The configuration directory is mounted **writable**: creating a backup set
      through the UI rewrites `config.yaml`, and saving a setting succeeds. This
      is the shape issue #196 fixed, and a read-only mount here makes all three
      write paths fail

## Step 5 — Container posture

- [ ] Neither container is privileged, neither mounts a Docker socket, neither
      uses host networking or the host PID namespace, and neither adds a capability:

      ```bash
      docker inspect backup-manager backup-manager-ui \
        --format '{{.Name}} priv={{.HostConfig.Privileged}} net={{.HostConfig.NetworkMode}} binds={{.HostConfig.Binds}}'
      ```
- [ ] Both containers run as uid 1000, on a read-only root filesystem
- [ ] The engine publishes no port: `docker ps` shows exactly one published port
      for this app, and it belongs to the web UI container

## Step 6 — Update

Update means making a newer canonical image tag resolvable on the host and
recreating the two containers. ZimaOS's own **Update** on the app tile re-pulls the image and
recreates the containers from the same file.

Capture a baseline before the pull and compare after it, so "everything
survived" is a diff rather than an impression:

```bash
sha256sum /DATA/AppData/backup-manager/state/state.db | tee /root/zimaos-before-update.sha256
find /DATA/Backups/backup-manager -type f -printf '%p %s\n' | sort > /root/zimaos-before-update.txt
```

Then use ZimaOS's **Update** on the app tile.

```bash
find /DATA/Backups/backup-manager -type f -printf '%p %s\n' | sort > /root/zimaos-after-update.txt
diff /root/zimaos-before-update.txt /root/zimaos-after-update.txt
```

- [ ] The update pulled a new image and recreated both containers
- [ ] `diff` of the retained-artifact listing is empty: the update moved no backup data
- [ ] Backup sets, schedules, retained artifacts and the administrator account all persist
- [ ] No re-enrollment was required
- [ ] The new image version is reported in the UI

## Step 7 — Removal, and retained-backup safety

This is the step that matters most, and the one that cannot be answered by
looking. **Capture the baseline first and write it outside the tree you are
about to test**, so whatever damages the tree cannot damage the evidence:

```bash
dd if=/dev/urandom of=/DATA/Backups/backup-manager/acceptance-canary.bin bs=1M count=8
sha256sum /DATA/Backups/backup-manager/acceptance-canary.bin | tee /root/zimaos-canary.sha256
find /DATA/Backups/backup-manager -type f -printf '%p %s\n' | sort > /root/zimaos-before-remove.txt
```

Now uninstall the app from ZimaOS, twice: once declining to delete the app's
data and once accepting, because the second answer is the one that could reach
the backup root and must not.

Then verify against the baseline, before inspecting anything else:

```bash
sha256sum -c /root/zimaos-canary.sha256
find /DATA/Backups/backup-manager -type f -printf '%p %s\n' | sort > /root/zimaos-after-remove.txt
diff /root/zimaos-before-remove.txt /root/zimaos-after-remove.txt
```

- [ ] `sha256sum -c` says OK and `diff` is empty: every retained backup and
      artifact is untouched, byte for byte
- [ ] Uninstalling with "delete data" accepted deleted no retained
      artifact either: the backup root is outside `/DATA/AppData`, and the same
      `sha256sum -c` and `diff` are still clean
- [ ] `/DATA/AppData/backup-manager/state` still holds the catalogue, so a reinstall
      pointed at the same paths comes back with the same backup sets
- [ ] Removing this adapter removes no core behaviour: the same image runs
      unchanged under `container/compose.yaml` on a plain Docker host

## Step 8 — The host management plane is untouched

Run these on the host, and compare against a baseline taken before step 1.

```bash
# before
dpkg -l > /root/zimaos-baseline-packages.txt 2>/dev/null || true
ls /etc/systemd/system > /root/zimaos-baseline-units.txt 2>/dev/null || true
```

- [ ] No package this procedure installed appears in a `diff` of the two package lists
- [ ] No unit file was added
- [ ] No entry was added under `/etc/cron.d` or to any crontab
- [ ] ZimaOS's own app list, users and settings are unchanged apart
      from this app being gone

## Step 9 — Destructive-safety re-check

- [ ] A backup set configured with a root outside `/DATA/Backups/backup-manager` is refused
- [ ] A symlink inside the backup root that points outside it is not followed into a delete
- [ ] A retention apply deletes only artifacts under the backup root
- [ ] Nothing under the private state, config or secrets paths is ever a delete target

## Step 10 — Cross-check against the automated matrix

```bash
cd distribution && GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix -v
```

- [ ] Every ZimaOS row the matrix reports as `PASS` still holds on the real host
- [ ] Every row it reports as `PENDING_OPERATOR` is now decided by this procedure
- [ ] No row it reports as `UNSUPPORTED` or `NOT_APPLICABLE` turned out to be
      supported here. If one did, `distribution/packaging/conformance.json` is
      stale and must be corrected rather than the check

---

## Evidence (section 68)

Fill this in in the same commit that flips ZimaOS from build-supported and
uncertified to certified. Until then every box above is unticked, and that is
the honest state: nobody has run this.

| Field | Value |
| --- | --- |
| ZimaOS version (Settings, About) | |
| Host hardware and architecture | |
| Image reference used, and how it was made resolvable | |
| Install result | |
| Web UI result | |
| Auth result | |
| Storage result and backup-root containment | |
| Engine reachability result | |
| Update result | |
| Removal result | |
| Retained-backup safety (canary and listing diff) | |
| Host management plane diff | |
| Destructive-safety re-check result | |
| Conformance matrix cross-check result | |
| Evidence (logs, screenshots, transcripts, with secrets redacted) | |
| Executed by | |
| Date | |
