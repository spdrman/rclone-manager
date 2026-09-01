# Portainer stack acceptance procedure

Section 68 of `docs/EPIC-B-multi-nas.md` requires this document to exist and be
version-controlled **before** anyone runs it. It has not been run. Every box in
it is unticked and the evidence table at the bottom is empty, which is the
honest state: Portainer CE is build-supported and uncertified.

Portainer CE deploys this product as a stack, from the App Template in
`apps/portainer/templates.json` or from the same file in Custom Templates. This
procedure decides the four things no laptop can: that the stack installs through
Portainer's own mechanism, that the web UI comes up, that an update replaces the
image without losing state, and that removing the stack does not touch a single
retained backup.

It also decides the one that matters most for this platform: **Portainer holds
the Docker socket and this product must never inherit it.** Step 5 checks that on
the running containers rather than in the file.

Issue #170 adds Portainer CE as new platform support. There was no Phase 4
Portainer CE packaging, so nothing here is a migration from an earlier one.

## Step 0 — Prerequisites

### 0.1 Record the host

- [ ] Portainer CE version (Settings, About) recorded in the evidence table
- [ ] Architecture recorded (`uname -m`)
- [ ] Container engine version recorded

### 0.2 Make the canonical image resolvable

`distribution/packaging/canonical.json` records `published: false`: no registry
is configured for this repository yet, so `ghcr.io/spdrman/backup-manager:0.1.0`
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
mkdir -p /opt/backup-manager/state /opt/backup-manager/backups \
         /opt/backup-manager/config /opt/backup-manager/secrets
```

The runtime image is distroless: no shell, no root step, nothing inside the
container can create or chown anything at startup, so the host-side owner has to
be right before the first start.

Create the SSH key and the pinned `known_hosts` **before** the ownership fix-up,
following `docs/ssh-setup.md`. Never commit either, and never paste a private key
into the evidence table.

```bash
ssh-keygen -t ed25519 -N "" -f /opt/backup-manager/secrets/id_ed25519
ssh-keyscan -t ed25519 <sftp-host> > /opt/backup-manager/secrets/known_hosts
```

**Recurse only over what this step created.** `/opt/backup-manager/backups` is the retained
backup store: on a reinstall it already holds data this procedure did not write,
and a recursive ownership change across it rewrites all of it with nothing to
restore it from. So the private trees are chowned recursively and the backup root
gets its own directory chowned, nothing beneath it. `distribution/packaging`
fails the build if any procedure in this directory recurses over a backup root or
a parent of one.

```bash
chown -R 1000:1000 /opt/backup-manager/state /opt/backup-manager/config /opt/backup-manager/secrets
chown 1000:1000 /opt/backup-manager/backups
chmod 600 /opt/backup-manager/secrets/id_ed25519
```

- [ ] All four paths exist and are owned by the app's uid and gid
- [ ] The recursive ownership change touched only state, config and secrets
- [ ] It ran **after** the key and `known_hosts` were created
- [ ] `/opt/backup-manager/config` is writable by the app's uid and gid
- [ ] Key material lives only on this host, redacted everywhere else

---

### 0.4 The configuration directory, and the config file that is now optional

The engine's start gate is a liveness question, not a backup-freshness verdict
(issue #206). It declares
`["CMD", "/backup-manager-web", "healthcheck", "--url", "http://127.0.0.1:8080/health/live"]`,
derived from `container/compose.yaml`, and `backup-manager-ui` waits on that with
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

**If you take the file route, take it before Deploy.** Every field Portainer shows is
one environment variable of `apps/portainer/compose/backup-manager.env`, and the stack
deploys as soon as you press Deploy, so a hand-written `config.yaml` has to be on the
host before that press or it is not the file the engine reads on its first start. Write
it over SSH on the host running the Docker engine, not inside the Portainer container.
Skip this block entirely to use the first-run flow instead.

```bash
$EDITOR /opt/backup-manager/config/config.yaml
chown 1000:1000 /opt/backup-manager/config/config.yaml
chmod 600 /opt/backup-manager/config/config.yaml
```

The container-side paths in it are fixed by this package and must not be changed:
every adapter mounts the same ones, which is why `apps/truenas/README.md`'s
annotated example is this same file with another platform's host paths, and
`scripts/deploy/deploy_generic.py`'s `render_config_yaml` is the authoritative shape.

**Never commit the config or paste one into the evidence table:** it names the SFTP
host and user.

- [ ] Either `config.yaml` is written into `/opt/backup-manager/config` **before** the install
      and is valid, or that directory is left empty and the first-run flow writes it.
      A file that exists and does not validate is the one state that refuses the start,
      so record which of the two routes this run took
- [ ] It is owned by the app's uid and gid and readable by them
- [ ] It was written after 0.3's ownership fix-up, or chowned afterwards
- [ ] The engine reported healthy on the first start, rather than restarting

---

## Step 1 — Install

1. In Portainer, **Settings, App Templates**, set the templates URL to this
   repository's `apps/portainer/templates.json`, and save. On a host that cannot
   reach the repository, use **Custom Templates, Add** and paste
   `apps/portainer/compose/backup-manager.yml` instead.
2. **App Templates**, pick Backup Manager, and fill the form. Every field is one
   variable of `apps/portainer/compose/backup-manager.env` and the defaults are
   the same defaults.
3. Deploy the stack.

- [ ] The template appeared in Portainer's App Templates list
- [ ] Every environment field Portainer showed matches a variable the stack reads
- [ ] The stack deployed and both containers reach `running`
- [ ] `backup-manager` reports healthy (it declares the liveness probe
      `/backup-manager-web healthcheck --url http://127.0.0.1:8080/health/live`,
      not the image's own `/backup-manager status`: the web UI waits on this, and
      the backup-freshness verdict is non-zero on a fresh install)
- [ ] `backup-manager-ui` reports healthy, having overridden the image's own healthcheck
- [ ] No Portainer agent was installed, and no Portainer extension or plugin was added

## Step 2 — Web UI

- [ ] The published port loads the shared web UI
- [ ] The UI reports the deployment as a Docker Compose deployment, which is what
      `apps/portainer/README.md` says to expect: this adapter ships no platform
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

- [ ] Private state lands under `/opt/backup-manager/state`
- [ ] Retained artifacts land under `/opt/backup-manager/backups`
- [ ] No SSH private key, `known_hosts`, config file or authentication record
      exists anywhere under `/opt/backup-manager/backups`
- [ ] The key and `known_hosts` are mounted read-only, and a write attempt from
      inside the container fails
- [ ] The configuration directory is mounted **writable**: creating a backup set
      through the UI rewrites `config.yaml`, and saving a setting succeeds. This
      is the shape issue #196 fixed, and a read-only mount here makes all three
      write paths fail

## Step 5 — The Docker socket, privileged mode and host networking

Portainer itself has the Docker socket. This product must not, and this is where
that is decided against the running containers rather than against the file.

```bash
docker inspect backup-manager backup-manager-ui \
  --format '{{.Name}} priv={{.HostConfig.Privileged}} net={{.HostConfig.NetworkMode}} pid={{.HostConfig.PidMode}} caps={{.HostConfig.CapAdd}} binds={{.HostConfig.Binds}}'
```

- [ ] Neither container is privileged
- [ ] Neither container's binds include `/var/run/docker.sock` or `/run/docker.sock`
- [ ] Neither container uses host networking or the host PID namespace
- [ ] Neither container adds a capability
- [ ] Stopping Portainer entirely leaves the stack running and the web UI reachable

## Step 6 — Update

Update means making a newer canonical image tag resolvable on the host and
recreating the two containers. It is not a Portainer operation: Portainer's **Recreate** with
"Re-pull image" does it, and so does `docker compose pull && docker compose up -d`
on the host.

Capture a baseline before the pull and compare after it, so "everything
survived" is a diff rather than an impression:

```bash
sha256sum /opt/backup-manager/state/state.db | tee /root/portainer-before-update.sha256
find /opt/backup-manager/backups -type f -printf '%p %s\n' | sort > /root/portainer-before-update.txt
```

Then, in Portainer, open the stack and use **Update the stack** with
"Re-pull image" enabled, or run the equivalent on the host.

```bash
find /opt/backup-manager/backups -type f -printf '%p %s\n' | sort > /root/portainer-after-update.txt
diff /root/portainer-before-update.txt /root/portainer-after-update.txt
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
dd if=/dev/urandom of=/opt/backup-manager/backups/acceptance-canary.bin bs=1M count=8
sha256sum /opt/backup-manager/backups/acceptance-canary.bin | tee /root/portainer-canary.sha256
find /opt/backup-manager/backups -type f -printf '%p %s\n' | sort > /root/portainer-before-remove.txt
```

Now remove the stack in Portainer: open it and press **Delete this stack**.
Leave "Remove non-persistent volumes" at whatever Portainer offers, then repeat
the check with it enabled too: this stack declares no named volume, so there is
nothing for it to reach, and that is the claim being tested.

Then verify against the baseline, before inspecting anything else:

```bash
sha256sum -c /root/portainer-canary.sha256
find /opt/backup-manager/backups -type f -printf '%p %s\n' | sort > /root/portainer-after-remove.txt
diff /root/portainer-before-remove.txt /root/portainer-after-remove.txt
```

- [ ] `sha256sum -c` says OK and `diff` is empty: every retained backup and
      artifact is untouched, byte for byte
- [ ] Deleting the stack with volume removal enabled deleted no
      retained artifact either: the same `sha256sum -c` and `diff` are still clean
- [ ] `/opt/backup-manager/state` still holds the catalogue, so a reinstall
      pointed at the same paths comes back with the same backup sets
- [ ] Removing this adapter removes no core behaviour: the same image runs
      unchanged under `container/compose.yaml` on a plain Docker host

## Step 8 — The host management plane is untouched

Run these on the host, and compare against a baseline taken before step 1.

```bash
# before
dpkg -l > /root/portainer-baseline-packages.txt 2>/dev/null || true
ls /etc/systemd/system > /root/portainer-baseline-units.txt 2>/dev/null || true
```

- [ ] No package this procedure installed appears in a `diff` of the two package lists
- [ ] No unit file was added
- [ ] No entry was added under `/etc/cron.d` or to any crontab
- [ ] Portainer's own configuration is unchanged apart from the
      template URL and the stack entry this procedure added

## Step 9 — Destructive-safety re-check

- [ ] A backup set configured with a root outside `/opt/backup-manager/backups` is refused
- [ ] A symlink inside the backup root that points outside it is not followed into a delete
- [ ] A retention apply deletes only artifacts under the backup root
- [ ] Nothing under the private state, config or secrets paths is ever a delete target

## Step 10 — Cross-check against the automated matrix

```bash
cd distribution && GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix -v
```

- [ ] Every Portainer CE row the matrix reports as `PASS` still holds on the real host
- [ ] Every row it reports as `PENDING_OPERATOR` is now decided by this procedure
- [ ] No row it reports as `UNSUPPORTED` or `NOT_APPLICABLE` turned out to be
      supported here. If one did, `distribution/packaging/conformance.json` is
      stale and must be corrected rather than the check

---

## Evidence (section 68)

Fill this in in the same commit that flips Portainer CE from build-supported and
uncertified to certified. Until then every box above is unticked, and that is
the honest state: nobody has run this.

| Field | Value |
| --- | --- |
| Portainer CE version (Settings, About) | |
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
