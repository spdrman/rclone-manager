# Dockge import and deploy acceptance procedure

Section 68 of `docs/EPIC-B-multi-nas.md` requires this document to exist and be
version-controlled **before** anyone runs it. It has not been run. Every box in
it is unticked and the evidence table at the bottom is empty, which is the
honest state: Dockge is build-supported and uncertified.

Dockge manages **the canonical Compose stack**. There is no Dockge
packaging to install: `apps/dockge/` holds a page and nothing else, on purpose,
and this procedure is what decides whether that claim is true on a real host.

So the thing under test here is unusual and worth stating: the artifact is
`container/compose.yaml` itself. If any step below needs a file that only exists
for Dockge, the compatibility claim has failed and the answer is to record the
incompatibility, not to add the file.

Issue #170 adds Dockge as new platform support. There was no Phase 4
Dockge packaging, so nothing here is a migration from an earlier one.

## Step 0 — Prerequisites

### 0.1 Record the host

- [ ] Dockge version (shown in its own UI footer) recorded in the evidence table
- [ ] Architecture recorded (`uname -m`)
- [ ] Container engine version recorded

### 0.2 Make the canonical image resolvable

`ghcr.io/spdrman/backup-manager:0.3.0` is cut but not pushed yet:
`distribution/packaging/canonical.json` records `image.published: false`, and
`container/release-manifest.json` carries a `registry_digest` of `null` per
architecture. So the reference does not resolve from the registry today, and the
steps below are how you make it resolve, by pushing a build to a registry this host
can reach or building elsewhere and loading it. The previous release,
`ghcr.io/spdrman/backup-manager:0.2.0`, stays published and signed if you would
rather run that:

```bash
docker buildx build --platform=linux/amd64,linux/arm64 -f container/Dockerfile -t backup-manager:acceptance .
docker save backup-manager:acceptance | ssh admin@<host> 'docker load'
```

- [ ] The image is resolvable on the host, and the exact reference used is recorded

### 0.3 Create the host paths

```bash
mkdir -p /volume1/backup-manager/state /volume1/backups \
         /volume1/backup-manager/config /volume1/backup-manager/secrets
```

The runtime image is distroless: no shell, no root step, nothing inside the
container can create or chown anything at startup, so the host-side owner has to
be right before the first start.

Create the SSH key and the pinned `known_hosts` **before** the ownership fix-up,
following `docs/ssh-setup.md`. Never commit either, and never paste a private key
into the evidence table.

```bash
ssh-keygen -t ed25519 -N "" -f /volume1/backup-manager/secrets/id_ed25519
ssh-keyscan -t ed25519 <sftp-host> > /volume1/backup-manager/secrets/known_hosts
```

**Recurse only over what this step created.** `/volume1/backups` is the retained
backup store: on a reinstall it already holds data this procedure did not write,
and a recursive ownership change across it rewrites all of it with nothing to
restore it from. So the private trees are chowned recursively and the backup root
gets its own directory chowned, nothing beneath it. `distribution/packaging`
fails the build if any procedure in this directory recurses over a backup root or
a parent of one.

```bash
chown -R 1000:1000 /volume1/backup-manager/state /volume1/backup-manager/config /volume1/backup-manager/secrets
chown 1000:1000 /volume1/backups
chmod 600 /volume1/backup-manager/secrets/id_ed25519
```

- [ ] All four paths exist and are owned by the app's uid and gid
- [ ] The recursive ownership change touched only state, config and secrets
- [ ] It ran **after** the key and `known_hosts` were created
- [ ] `/volume1/backup-manager/config` is writable by the app's uid and gid
- [ ] Key material lives only on this host, redacted everywhere else

---

### 0.4 The configuration directory, and the config file that is now optional

The engine's start gate is a liveness question, not a backup-freshness verdict
(issue #206). It declares
`["CMD", "/backup-manager-web", "healthcheck", "--url", "http://127.0.0.1:8080/health/live"]`,
derived from `container/compose.yaml`, and `web-ui` waits on that with
`condition: service_healthy`. `/backup-manager status` is still FR-24's freshness
verdict and still the image's own baked-in `HEALTHCHECK`, and it exits non-zero on a
fresh install by design, which is exactly why nothing waits on it any more. So a
**fresh install reaches the web UI**: an empty configuration directory is a legitimate
state, and the engine serves its first-run setup flow from it (issue #176).

**What this step still requires.** The configuration directory itself, created and
owned by the app's uid and gid before the first start, because a bind mount does not
create or chown its source and the distroless runtime image has no shell to do it for
you. Writing `config.yaml` by hand is no longer required to reach the UI, and this
procedure keeps the commands below only as the faster route for an operator who
already has the SFTP details: a config file that EXISTS and does not validate is still
a hard startup failure rather than a first-run wizard, so an invalid one is worse than
none at all. Either finish setup in the browser and skip the file, or write it here.

**If you take the file route, take it before Start.** Dockge's editor edits the
stack's `compose.yaml` and `.env`, and nothing under the config mount, so a hand-written
`config.yaml` has to be on the host before the stack starts or it is not the file the
engine reads on its first start. Step 1 already puts you in a shell on that host: do it
there, and do it before **Start**. Skip this block entirely to use the first-run flow
instead.

```bash
$EDITOR /volume1/backup-manager/config/config.yaml
chown 1000:1000 /volume1/backup-manager/config/config.yaml
chmod 600 /volume1/backup-manager/config/config.yaml
```

The container-side paths in it are fixed by this package and must not be changed:
every adapter mounts the same ones, which is why `apps/truenas/README.md`'s
annotated example is this same file with another platform's host paths, and
`scripts/deploy/deploy_generic.py`'s `render_config_yaml` is the authoritative shape.

**Never commit the config or paste one into the evidence table:** it names the SFTP
host and user.

- [ ] Either `config.yaml` is written into `/volume1/backup-manager/config` **before** the install
      and is valid, or that directory is left empty and the first-run flow writes it.
      A file that exists and does not validate is the one state that refuses the start,
      so record which of the two routes this run took
- [ ] It is owned by the app's uid and gid and readable by them
- [ ] It was written after 0.3's ownership fix-up, or chowned afterwards
- [ ] The engine reported healthy on the first start, rather than restarting

---

## Step 1 — Install

1. Create the stack directory under Dockge's stacks root and copy the canonical
   stack in unmodified:

   ```bash
   mkdir -p /opt/stacks/backup-manager
   cp container/compose.yaml /opt/stacks/backup-manager/compose.yaml
   cp container/.env.example /opt/stacks/backup-manager/.env
   ```

2. Edit only `/opt/stacks/backup-manager/.env`. Record every edit: the number of
   edits needed to `compose.yaml` itself is an acceptance result, and it must be
   the removal of the `build:` block and nothing else.
3. In Dockge the stack appears on its own. Press **Start**.

- [ ] Dockge listed the stack without being told anything about it
- [ ] The only edit to `compose.yaml` was removing the `build:` block, which
      `apps/dockge/README.md` documents; record any other edit as a finding
- [ ] Dockge's own editor round-trips the file without reformatting it into
      something the canonical suite would reject
- [ ] Both containers reach `running`, and Dockge's interactive log pane shows both
- [ ] `rclone-manager` reports healthy (it declares the liveness probe
      `/backup-manager-web healthcheck --url http://127.0.0.1:8080/health/live`,
      not the image's own `/backup-manager status`: the web UI waits on this, and
      the backup-freshness verdict is non-zero on a fresh install)
- [ ] `web-ui` reports healthy, having overridden the image's own healthcheck

## Step 2 — Web UI

- [ ] The published port loads the shared web UI
- [ ] The UI reports the deployment as a Docker Compose deployment, which is what
      `apps/dockge/README.md` says to expect: this adapter ships no platform
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

- [ ] Private state lands under `/volume1/backup-manager/state`
- [ ] Retained artifacts land under `/volume1/backups`
- [ ] No SSH private key, `known_hosts`, config file or authentication record
      exists anywhere under `/volume1/backups`
- [ ] The key and `known_hosts` are mounted read-only, and a write attempt from
      inside the container fails
- [ ] The configuration directory is mounted **writable**: creating a backup set
      through the UI rewrites `config.yaml`, and saving a setting succeeds. This
      is the shape issue #196 fixed, and a read-only mount here makes all three
      write paths fail

## Step 5 — No Dockge-specific code was needed

The support model for Dockge is compatibility, so the acceptance question is
whether compatibility held.

- [ ] Nothing under `apps/dockge/` was needed at deploy time except as reading
- [ ] No file was added to this repository to make the import work
- [ ] Neither container is privileged, neither mounts a Docker socket, neither
      uses host networking or the host PID namespace, and neither adds a capability:

      ```bash
      docker inspect backup-manager-rclone-manager-1 backup-manager-web-ui-1 \
        --format '{{.Name}} priv={{.HostConfig.Privileged}} net={{.HostConfig.NetworkMode}} binds={{.HostConfig.Binds}}'
      ```
- [ ] Stopping Dockge leaves the stack running and the web UI reachable

## Step 6 — Update

Update means making a newer canonical image tag resolvable on the host and
recreating the two containers. It is not a Dockge operation: Dockge's own **Update** button runs
`docker compose pull` and `up -d` against the same file.

Capture a baseline before the pull and compare after it, so "everything
survived" is a diff rather than an impression:

```bash
sha256sum /volume1/backup-manager/state/state.db | tee /root/dockge-before-update.sha256
find /volume1/backups -type f -printf '%p %s\n' | sort > /root/dockge-before-update.txt
```

Then press **Update** in Dockge, or run the equivalent on the host.

```bash
find /volume1/backups -type f -printf '%p %s\n' | sort > /root/dockge-after-update.txt
diff /root/dockge-before-update.txt /root/dockge-after-update.txt
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
dd if=/dev/urandom of=/volume1/backups/acceptance-canary.bin bs=1M count=8
sha256sum /volume1/backups/acceptance-canary.bin | tee /root/dockge-canary.sha256
find /volume1/backups -type f -printf '%p %s\n' | sort > /root/dockge-before-remove.txt
```

Now remove the stack: Dockge's **Delete** on the stack page. Then repeat with
the stack directory deleted from `/opt/stacks` as well, which is the harder case,
because that is where an operator's own copy of the compose file lives.

Then verify against the baseline, before inspecting anything else:

```bash
sha256sum -c /root/dockge-canary.sha256
find /volume1/backups -type f -printf '%p %s\n' | sort > /root/dockge-after-remove.txt
diff /root/dockge-before-remove.txt /root/dockge-after-remove.txt
```

- [ ] `sha256sum -c` says OK and `diff` is empty: every retained backup and
      artifact is untouched, byte for byte
- [ ] Deleting the stack directory from `/opt/stacks` deleted no
      retained artifact either: the same `sha256sum -c` and `diff` are still clean
- [ ] `/volume1/backup-manager/state` still holds the catalogue, so a reinstall
      pointed at the same paths comes back with the same backup sets
- [ ] Removing this adapter removes no core behaviour: the same image runs
      unchanged under `container/compose.yaml` on a plain Docker host

## Step 8 — The host management plane is untouched

Run these on the host, and compare against a baseline taken before step 1.

```bash
# before
dpkg -l > /root/dockge-baseline-packages.txt 2>/dev/null || true
ls /etc/systemd/system > /root/dockge-baseline-units.txt 2>/dev/null || true
```

- [ ] No package this procedure installed appears in a `diff` of the two package lists
- [ ] No unit file was added
- [ ] No entry was added under `/etc/cron.d` or to any crontab
- [ ] Dockge's stacks root holds no leftover directory for this stack

## Step 9 — Destructive-safety re-check

- [ ] A backup set configured with a root outside `/volume1/backups` is refused
- [ ] A symlink inside the backup root that points outside it is not followed into a delete
- [ ] A retention apply deletes only artifacts under the backup root
- [ ] Nothing under the private state, config or secrets paths is ever a delete target

## Step 10 — Cross-check against the automated matrix

```bash
cd distribution && GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix -v
```

- [ ] Every Dockge row the matrix reports as `PASS` still holds on the real host
- [ ] Every row it reports as `PENDING_OPERATOR` is now decided by this procedure
- [ ] No row it reports as `UNSUPPORTED` or `NOT_APPLICABLE` turned out to be
      supported here. If one did, `distribution/packaging/conformance.json` is
      stale and must be corrected rather than the check

---

## Evidence (section 68)

Fill this in in the same commit that flips Dockge from build-supported and
uncertified to certified. Until then every box above is unticked, and that is
the honest state: nobody has run this.

| Field | Value |
| --- | --- |
| Dockge version (shown in its own UI footer) | |
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
