# OpenMediaVault provider acceptance procedure

**Status: NOT EXECUTED. OpenMediaVault is build-supported and uncertified (§68).**

This is the procedure an operator runs on a real OMV system to certify the
deployment profile in `apps/openmediavault/`. I wrote it before the compose assets
existed, per §68, and nothing in it has been run: no OMV instance, VM or physical,
was available on the machine the profile was built on.

Required evidence per §68: a **current OMV 8.x Debian-based test system**. Record
the exact release in the evidence table.

Everything the repository itself can decide is already checked by
`distribution/packaging` on every commit. This procedure covers only what a laptop
cannot reach.

---

## Scope: no native plugin

Section 4A defers a native OMV Workbench plugin, and WP4.3 says "Do NOT implement
a native OMV plugin in v1". This procedure therefore certifies a **supported
Compose deployment** and nothing more. OMV sits in Tier C (a supported provider
deployment profile), one tier below TrueNAS and Unraid.

Concretely that means: no entry in OMV's own navigation tree, no Workbench form,
no RPC service, no `salt` state, and no `openmediavault-backupmanager` Debian
package. If any of those appear in `apps/openmediavault/`, the package has
overrun its scope and `distribution/packaging` will fail the build before anyone
gets here.

The Web UI is reached by its own published port, and step 3 covers documenting
that clearly enough that an operator finds it without a navigation entry.

---

## Step 0 — Prerequisites

### 0.1 Install the Compose plugin

```bash
omv-extras   # enable the omv-extras repository if it is not already
apt-get install openmediavault-compose
```

- [ ] **Services → Compose** appears in the OMV Workbench
- [ ] `docker --version` and `docker compose version` both work over SSH

### 0.2 Make the canonical image resolvable

No registry is configured for this repository yet, so the reference in the compose
file resolves to nothing until you point it somewhere. Either push to your own
registry:

```bash
docker buildx build \
  --platform=linux/amd64,linux/arm64 \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -f container/Dockerfile \
  -t <your-registry>/backup-manager:<version> \
  --push .
```

or side-load and set `IMAGE` in the env file to the loaded tag:

```bash
docker save backup-manager:<version> | gzip > backup-manager.tar.gz
scp backup-manager.tar.gz root@<omv>:/root/
ssh root@<omv> 'gunzip -c /root/backup-manager.tar.gz | docker load'
```

The compose file reads the image reference from a single `IMAGE` variable in
`apps/openmediavault/compose/backup-manager.env`, so this is a one-line change in
one file, never an edit scattered through the compose YAML.

- [ ] Canonical image resolvable on the NAS, reference recorded

### 0.3 Resolve the real filesystem paths

The host-path defaults in `distribution/packaging/canonical.json`
(`platforms.openmediavault.hostPaths`) use `/srv/dev-disk-by-uuid/...`, which is a
**placeholder**, deliberately matching what the OMV frontend bridge already
declares. A real OMV system mounts data filesystems at
`/srv/dev-disk-by-uuid-<UUID>/`, with the UUID differing per machine, so no
checked-in default can be literally correct.

Find yours:

```bash
ls -d /srv/dev-disk-by-uuid-*
```

Set `DISK` in `backup-manager.env` and change nothing else. Every host path in
the compose file is written `${DISK}/...`, so the UUID appears exactly once, and
the compose file itself needs no editing. `DISK` is referenced in the
fail-closed `${DISK:?...}` form, so leaving it unset or misspelling it stops the
deployment instead of creating five directories in the wrong place.

```bash
DISK=/srv/dev-disk-by-uuid-<your-uuid>
mkdir -p "$DISK/appdata/backup-manager"/{state,config,secrets}
mkdir -p "$DISK/backups/backup-manager"
chmod 700 "$DISK/appdata/backup-manager/secrets"
```

The backup root is `backup-manager` **inside** `$DISK/backups`, not that
directory itself, which is very likely one you already use. Every step below
creates, owns and later inspects only paths this procedure created.

- [ ] Real `dev-disk-by-uuid-<UUID>` path recorded
- [ ] `appdata/backup-manager/{state,config,secrets}` and
      `backups/backup-manager` exist
- [ ] The UUID appears in exactly one place on the NAS, the env file's `DISK`
- [ ] Starting the stack with `DISK` unset fails loudly rather than creating
      paths (try it once, on purpose)

### 0.4 Own them by the uid/gid the app runs as

The runtime image is distroless: no shell, no root step, no init process, so
nothing inside the container can chown these at startup.

OMV's conventional service account is `uid 1000` for the first admin account;
check yours with `id <your-admin-user>`.

```bash
chown -R 1000:100 "$DISK/appdata/backup-manager"
chown 1000:100 "$DISK/backups/backup-manager"
```

Only paths this procedure created, and the backup root non-recursively. A
`chown -R` across `$DISK/backups` would rewrite the ownership of everything
already in it, fights the Workbench's own shared-folder ACL management, and on a
reinstall would rewrite the retained backup store.

- [ ] `PUID`/`PGID` chosen, recorded, and set in the env file
- [ ] appdata tree and `backups/backup-manager` owned by that uid/gid
- [ ] Nothing else under `$DISK/backups` had its ownership changed

### 0.5 Create the SSH key, the pinned known_hosts, and the config

> **This step is packaging debt now, not engine behavior.** As of issue #176 the
> engine no longer needs a configuration to start: an instance with no
> `config.yaml` serves a first-run setup flow, in the web UI, that writes one
> for you. What still blocks that here is the package. The config is
> bind-mounted as a single **read-only file**, so the container cannot create
> it, and a bind mount cannot express "this file does not exist yet" either (a
> missing source gets a directory created for it). Until the packaged config
> mount becomes a writable **directory**, this platform keeps the hand-written
> config, and this step keeps its shell commands for that reason and no other.
> That same mount is already what makes the existing create-backup-set (#146)
> and settings (#140) write paths inert in a packaged container, so it is one
> packaging fix for three things.
>
> Nothing else here survives that fix: once the mount is writable, the key is
> pasted into the setup flow's Authentication step, the host key is probed and
> confirmed on its Verify server step, and no `config.yaml` is written by hand
> at all.

`/backup-manager-web serve` starts without a `config.yaml` and serves the
first-run setup flow instead (#176), but a config file that EXISTS and does not
validate is still a hard startup failure. Given the read-only mount above, create
all three before the first start.

**What still requires this step, precisely (issue #196).** The configuration mount is
now a writable directory the application owns, so the container can create and replace
`config.yaml` itself, and an empty directory is a legitimate state rather than a broken
deployment. Two things nonetheless keep this step here. The directory itself must exist
and be owned by the app's uid/gid before the first start, because a bind mount does not
create or chown its source. And `/backup-manager-web serve` still refuses to start
without a valid config: removing that refusal, and serving a first-run flow instead, is
#176's work and is not merged. Once it is, everything below except creating and owning
the directory becomes optional.

```bash
ssh-keygen -t ed25519 -N '' -f "$DISK/appdata/backup-manager/secrets/id_ed25519"
ssh-keyscan -t ed25519 <your-sftp-host> > "$DISK/appdata/backup-manager/secrets/known_hosts"
chmod 600 "$DISK/appdata/backup-manager/secrets/id_ed25519"
chown 1000:100 "$DISK/appdata/backup-manager/secrets/"*
```

Verify the host key fingerprint out of band. Then write
`$DISK/appdata/backup-manager/config/config.yaml` using the annotated example in
`apps/openmediavault/README.md`.

**Never commit the private key, the config, or any transcript containing them.**

- [ ] Key pair generated, mode 0600, owned by `PUID:PGID`
- [ ] `known_hosts` pinned, fingerprint verified out of band
- [ ] `$DISK/appdata/backup-manager/config` exists and is **writable** by `PUID:PGID`
- [ ] `config.yaml` written inside it and readable by `PUID:PGID`

---

## Step 1 — Install

1. **Services → Compose → Files → Add**.
2. Name: `backup-manager`.
3. Paste `apps/openmediavault/compose/backup-manager.yml` into the **File** field.
4. Paste `apps/openmediavault/compose/backup-manager.env`, with your step 0
   substitutions, into the **Environment** field.
5. Save, then **Up**.

- [ ] The compose file saves with no validation error
- [ ] `Up` completes and both services reach **running**
- [ ] The engine service reaches health **healthy** (it inherits the image's own
      `HEALTHCHECK`, `/backup-manager status`)
- [ ] The Web UI service reaches health **healthy** via its own
      `/backup-manager-web healthcheck` override, not the image's
      `/backup-manager status` (which would fail: no config, no state database)
- [ ] The engine service publishes no port (`docker compose ps` shows a port
      mapping only for the Web UI service)

---

## Step 2 — Verify from the Workbench

- [ ] **Services → Compose → Files** lists `backup-manager` with status up
- [ ] The plugin's **Logs** action shows both services' output
- [ ] No error, warning or orphan-container notice appears in
      **System → Notifications**

---

## Step 3 — Web UI access

OMV has no navigation entry for this app, by design. Everything an operator needs
to find it must therefore be in the documentation.

1. Open `http://<omv-host>:<published port>/` in a browser.

- [ ] The shared Web UI loads
- [ ] `apps/openmediavault/README.md` states the exact URL shape, states that the
      port is set in one place in the env file, and states plainly that there is
      no OMV navigation entry and why
- [ ] The published port does not collide with OMV's own Workbench port, and the
      README says what to change if it does
- [ ] Nothing in OMV's own navigation tree, dashboard or service list was
      modified by this install

---

## Step 4 — Authentication (local-account only)

OMV gets no auth of its own. It uses the reusable local authentication the generic
Web host provides (§13A).

1. Read the one-time enrollment link out of the **engine** service's log:

   ```bash
   docker compose -p backup-manager logs backup-manager 2>&1 | grep -i enroll
   ```

2. Open it, enrol an administrator with a password you generate now, log out, log
   back in, then open the enrollment link a second time.

- [ ] No account exists before enrollment
- [ ] The token appears only in the service log, never in any file under
      `apps/openmediavault/`
- [ ] Enrollment succeeds, logout then login succeeds
- [ ] The enrollment link is refused the second time
- [ ] `GET /api/v1/system/capabilities` reports `nativeAuth: false`
- [ ] `$DISK/appdata/backup-manager/state/local-auth.json` holds an Argon2id
      hash, never a plaintext password
- [ ] Backup Manager's login is completely independent of the OMV Workbench
      login, and neither can log into the other

---

## Step 5 — Storage mapping and backup-root containment

Run one backup cycle to completion, then:

```bash
ls -la "$DISK/backups/backup-manager"
ls -la "$DISK/appdata/backup-manager/state"
grep -rIl 'PRIVATE KEY' "$DISK/backups/backup-manager" || echo "clean"
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
head -c 8M /dev/urandom > "$DISK/backups/backup-manager"/canary.bin
sha256sum "$DISK/backups/backup-manager"/canary.bin | tee /root/backup-manager-acceptance/canary.sha256
find "$DISK/backups/backup-manager" -type f -printf '%p %s\n' | sort > /root/backup-manager-acceptance/backup-root.before
```

Keep `/root/backup-manager-acceptance` off the repository: the listing names your own backup
sets. Record only that it was taken, and the canary's hash, in the evidence table.

- [ ] At least one completed artifact is under `$DISK/backups/backup-manager`
- [ ] `state.db` and `local-auth.json` are under appdata, **not** under the
      backup root
- [ ] No private key, `known_hosts`, or auth state anywhere under
      `$DISK/backups/backup-manager` (§19.2)
- [ ] Nothing was written anywhere else under `$DISK/backups`
- [ ] A sidecar recovery manifest sits next to the artifact and contains no
      secret material (§19.3)
- [ ] `canary.bin` written into the backup root and its hash recorded outside it
- [ ] A full `find` listing of the backup root recorded outside it

---

## Step 6 — Update

1. Capture a baseline first, over SSH to the OMV box, so the checks below are a
   comparison rather than an impression:
   ```bash
   sha256sum $DISK/appdata/backup-manager/state/state.db | tee /tmp/before-update.sha256
   find $DISK/backups -type f -printf '%p %s\n' | sort > /tmp/before-update.txt
   ```
2. Push or side-load a newer image tag and change `IMAGE` in the env file.
3. **Services → Compose → Files → backup-manager → Pull**, then **Up**.
4. Compare afterwards:
   ```bash
   find $DISK/backups -type f -printf '%p %s\n' | sort > /tmp/after-update.txt
   diff /tmp/before-update.txt /tmp/after-update.txt
   ```

- [ ] Pull and Up both complete, and both services return to healthy
- [ ] `diff` of the retained-artifact listing is empty: the update moved no
      backup data
- [ ] The administrator account still exists (no re-enrollment prompt)
- [ ] Logging back in with the same password works
- [ ] Every backup set is still configured
- [ ] Every artifact is still present and still listed
- [ ] The old image can be pruned without affecting the running deployment

---

## Step 7 — Container replacement

Same image, destroyed and recreated containers. This is what an OMV reboot, a
`Down` then `Up`, or a `docker system prune` does.

```bash
docker compose -p backup-manager down
docker compose -p backup-manager up -d
```

- [ ] Both services come back healthy
- [ ] Retained backup data survives untouched
- [ ] The catalog survives
- [ ] The administrator account survives

---

## Step 8 — Remove

This is the destructive-safety step. Its evidence was captured back in the
storage step, because after the removal there is nothing left to compare
against, and any deletion the comparison turns up is a release blocker rather
than a finding to triage.

1. **Services → Compose → Files → backup-manager → Down**.
2. Then **Delete** the file entry.

- [ ] Both containers are gone

Check the backup root against the baseline recorded in the storage step, before
looking at anything else:

```bash
sha256sum -c /root/backup-manager-acceptance/canary.sha256
find "$DISK/backups/backup-manager" -type f -printf '%p %s\n' | sort > /root/backup-manager-acceptance/backup-root.after
diff /root/backup-manager-acceptance/backup-root.before /root/backup-manager-acceptance/backup-root.after
```

- [ ] `sha256sum -c` reports the canary `OK`
- [ ] The `diff` against the recorded listing is empty, so the backup root is
      untouched, byte for byte, and every artifact is still readable
- [ ] `$DISK/appdata/backup-manager` is untouched
- [ ] Nothing elsewhere under `$DISK/backups` changed
- [ ] Nothing outside the declared host paths was touched, and no OMV
      configuration was modified
- [ ] Re-adding the same compose file with the same paths adopts the existing
      catalog rather than starting empty

Also run the destructive half on a scratch install: `docker compose down -v` and
confirm no named volume ever held retained backup data (every persistent path in
this profile is a bind mount to a host path you chose, precisely so that `-v`
cannot reach it).

- [ ] `down -v` removes nothing under `$DISK/backups/backup-manager`

---

## Evidence (§68)

Fill this in in the same commit that flips OpenMediaVault from uncertified to
certified.

| Field | Value |
| --- | --- |
| Provider / OS version | |
| Hardware or VM, model | |
| Architecture | |
| Package / image version | |
| Image reference used, and how it was made resolvable | |
| Real `dev-disk-by-uuid-<UUID>` path used | |
| Install result | |
| Web UI access result | |
| Auth result | |
| Storage result | |
| Update result | |
| Uninstall / removal result | |
| Retained-backup safety | |
| Confirmation that no native plugin was installed | |
| Evidence (logs, screenshots, transcripts, with secrets redacted) | |
| Executed by | |
| Date | |
