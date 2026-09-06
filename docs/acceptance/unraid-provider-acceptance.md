# Unraid provider acceptance procedure

**Status: NOT EXECUTED. Unraid is build-supported and uncertified (§68).**

This is the procedure an operator runs on a real Unraid system to certify the
templates in `apps/unraid/`. I wrote it before the templates existed, per §68, and
nothing in it has been run: no Unraid instance, VM or physical, was available on
the machine the package was built on.

Required evidence per §68: a **current supported Unraid release VM or hardware**.
Record the exact release in the evidence table.

Everything the repository itself can decide is already checked by
`distribution/packaging` on every commit. This procedure covers only what a laptop
cannot reach.

---

## The one structural thing to understand first

Unraid's Docker template model describes exactly **one** container per template.
The canonical image needs **two**: `/backup-manager-web serve` (the engine: API,
scheduler, local authentication, no published port) and
`/backup-manager-web serve-ui` (the static UI plus a reverse proxy, the only
published port). There is no single command that does both, by design, so the
package ships two templates.

The Web UI container reaches the engine by container name. Docker's embedded DNS
resolves container names only on a **user-defined** network, never on the default
`bridge`, so both templates target a user-defined network that you create once,
before installing either. That is a one-command prerequisite, and step 0.2 covers
it. If you skip it, the UI container starts, serves the static bundle, and then
502s on every API call.

---

## Step 0 — Prerequisites

### 0.1 Make the canonical image resolvable

`ghcr.io/spdrman/backup-manager:0.3.0` is cut but not pushed yet:
`distribution/packaging/canonical.json` records `image.published: false`, and
`container/release-manifest.json` carries a `registry_digest` of `null` per
architecture. So the reference does not resolve from the registry today, and the
steps below are how you make it resolve, by pushing a build to a registry this host
can reach or building elsewhere and loading it. The previous release,
`ghcr.io/spdrman/backup-manager:0.2.0`, stays published and signed if you would
rather run that. Either push to your own registry:

```bash
docker buildx build \
  --platform=linux/amd64 \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -f container/Dockerfile \
  -t <your-registry>/backup-manager:<version> \
  --push .
```

or side-load, and edit `Repository` in the Unraid template editor at install time:

```bash
docker save backup-manager:<version> | gzip > backup-manager.tar.gz
scp backup-manager.tar.gz root@<unraid>:/mnt/user/
ssh root@<unraid> 'gunzip -c /mnt/user/backup-manager.tar.gz | docker load'
```

- [ ] Canonical image resolvable on the NAS, reference recorded

### 0.2 Create the user-defined network

```bash
docker network create backup-manager
docker network inspect backup-manager --format '{{.Driver}} {{.Name}}'
```

- [ ] A user-defined bridge network named `backup-manager` exists
- [ ] It appears in the **Network Type** dropdown in Unraid's Docker template editor

### 0.3 Create the appdata and backup shares

Host-path defaults come from `distribution/packaging/canonical.json`
(`platforms.unraid.hostPaths`) and match what `apps/unraid/frontend/webui.json`
and the Unraid frontend bridge already declare:

```bash
mkdir -p /mnt/user/appdata/backup-manager/{state,config,secrets}
mkdir -p /mnt/user/backups/backup-manager
chmod 700 /mnt/user/appdata/backup-manager/secrets
```

`/mnt/user/backups` must be a real user share Backup Manager can write to, not a
directory inside appdata. Appdata holds the catalog database; the share holds
retained backup data. §19.2 makes those two separate security domains, and the
whole removal criterion below depends on them being separate.

The backup root is `backup-manager` **inside** that share, not the share itself.
`backups` is one of the likeliest names for a share you already use for something
else, and this procedure creates directories, owns them and later checks nothing
outside them changed. Keeping the app inside a directory of its own means every
one of those steps only ever touches paths this procedure created.

- [ ] `appdata/backup-manager/{state,config,secrets}` exist
- [ ] A `backups` user share exists and is writable
- [ ] `backups/backup-manager` exists and was created by this step

### 0.4 Own them by the uid/gid the app runs as

The runtime image is distroless: no shell, no root step, no init process, so
nothing inside the container can chown these for you at startup.

Unraid's conventional account is `99:100` (`nobody:users`):

```bash
chown -R 99:100 /mnt/user/appdata/backup-manager
chown 99:100 /mnt/user/backups/backup-manager
```

Only paths this procedure created, and the backup root non-recursively. A
`chown -R` across `/mnt/user/backups` would rewrite the ownership of everything
any other tool has ever put in that share, is not reversible without an ownership
record nobody took, and crawls the `/mnt/user` FUSE layer for as long as that
takes. On a reinstall the same command would rewrite the retained backup store.

- [ ] `PUID`/`PGID` chosen and recorded
- [ ] appdata tree and `backups/backup-manager` owned by that uid/gid
- [ ] Nothing else in the `backups` share had its ownership changed

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
ssh-keygen -t ed25519 -N '' -f /mnt/user/appdata/backup-manager/secrets/id_ed25519
ssh-keyscan -t ed25519 <your-sftp-host> > /mnt/user/appdata/backup-manager/secrets/known_hosts
chmod 600 /mnt/user/appdata/backup-manager/secrets/id_ed25519
chown 99:100 /mnt/user/appdata/backup-manager/secrets/*
```

Verify the host key fingerprint out of band. Then write
`/mnt/user/appdata/backup-manager/config/config.yaml` using the annotated example
in `apps/unraid/README.md`.

**Never commit the private key, the config, or any transcript containing them.**

- [ ] Key pair generated, mode 0600, owned by `PUID:PGID`
- [ ] `known_hosts` pinned, fingerprint verified out of band
- [ ] `/mnt/user/appdata/backup-manager/config` exists and is **writable** by `PUID:PGID`
- [ ] `config.yaml` written inside it and readable by `PUID:PGID`

---

## Step 1 — Install the engine template

1. Copy `apps/unraid/template/backup-manager.xml` to
   `/boot/config/plugins/dockerMan/templates-user/my-backup-manager.xml` on the
   Unraid flash drive.
2. **Docker → Add Container**, and pick `backup-manager` from the
   **user templates** section of the template dropdown.
3. Check every mapping against step 0's paths and the defaults the template
   supplied. Change nothing you did not have to.
4. Apply.

- [ ] The template loads in the editor with no missing or blank required field
- [ ] Every `Config` element renders with the right type (Port, Path, Variable)
      and the right default
- [ ] The container starts
- [ ] It reaches Docker health **healthy** (it inherits the image's own
      `HEALTHCHECK`, `/backup-manager status`, which is the right answer here:
      an Unraid template declares no start-ordering dependency, so nothing waits
      on this verdict and it is the backup-freshness badge FR-24 means it to be.
      On a fresh install it will be red until the first backup lands)
- [ ] It has **no published port** (`docker port <engine>` prints nothing)
- [ ] It is attached to the `backup-manager` network

---

## Step 2 — Install the Web UI template

1. Copy `apps/unraid/template/backup-manager-ui.xml` to
   `/boot/config/plugins/dockerMan/templates-user/my-backup-manager-ui.xml`.
2. **Docker → Add Container**, pick `backup-manager-ui`.
3. Apply.

- [ ] The container starts and reaches Docker health **healthy** via its own
      `/backup-manager-web healthcheck` override, not the image's
      `/backup-manager status` (which would fail: this container has no config
      file and no state database)
- [ ] It publishes exactly one port
- [ ] It is attached to the `backup-manager` network
- [ ] It has **no** volume mappings at all: it never reads the config, the key,
      `known_hosts`, or either data directory

If it is unhealthy while the engine is healthy, the healthcheck override did not
apply. Capture `docker inspect --format '{{json .Config.Healthcheck}}' <ui>` before
changing anything; that is a package bug.

---

## Step 3 — WebUI link

- [ ] The container's Unraid context menu shows **WebUI**
- [ ] Clicking it opens the shared Web UI, not a 404
- [ ] The resolved URL matches the template's `<WebUI>` value with `[IP]` and
      `[PORT:8080]` substituted, and matches
      `apps/unraid/frontend/webui.json`'s `webui` field
- [ ] The engine container's own context menu has **no** WebUI entry (it has no
      published port and must never be opened directly)

---

## Step 4 — Authentication (local-account only)

Unraid gets no auth of its own. It uses the reusable local authentication the
generic Web host provides (§13A).

1. Read the one-time enrollment link out of the **engine** container's log:

   ```bash
   docker logs <engine container> 2>&1 | grep -i enroll
   ```

2. Open it, enrol an administrator with a password you generate now, log out, log
   back in, then open the enrollment link a second time.

- [ ] No account exists before enrollment
- [ ] The token appears only in the container log, never in any file under
      `apps/unraid/`
- [ ] Enrollment succeeds, logout then login succeeds
- [ ] The enrollment link is refused the second time
- [ ] `GET /api/v1/system/capabilities` reports `nativeAuth: false`
- [ ] `/mnt/user/appdata/backup-manager/state/local-auth.json` holds an Argon2id
      hash, never a plaintext password
- [ ] Backup Manager's login is completely independent of Unraid's own root
      password, and neither can log into the other

---

## Step 5 — Storage mapping and backup-root containment

Run one backup cycle to completion, then:

```bash
ls -la /mnt/user/backups/backup-manager
ls -la /mnt/user/appdata/backup-manager/state
grep -rIl 'PRIVATE KEY' /mnt/user/backups/backup-manager || echo "clean"
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
head -c 8M /dev/urandom > /mnt/user/backups/backup-manager/canary.bin
sha256sum /mnt/user/backups/backup-manager/canary.bin | tee /root/backup-manager-acceptance/canary.sha256
find /mnt/user/backups/backup-manager -type f -printf '%p %s\n' | sort > /root/backup-manager-acceptance/backup-root.before
```

Keep `/root/backup-manager-acceptance` off the repository: the listing names your own backup
sets. Record only that it was taken, and the canary's hash, in the evidence table.

- [ ] At least one completed artifact is under `/mnt/user/backups/backup-manager`
- [ ] `state.db` and `local-auth.json` are under appdata, **not** under the
      backup root
- [ ] No private key, `known_hosts`, or auth state anywhere under
      `/mnt/user/backups/backup-manager` (§19.2)
- [ ] Nothing was written anywhere else in the `backups` share
- [ ] A sidecar recovery manifest sits next to the artifact and contains no
      secret material (§19.3)
- [ ] `canary.bin` written into the backup root and its hash recorded outside it
- [ ] A full `find` listing of the backup root recorded outside it

---

## Step 6 — Update

Unraid updates a container by pulling the tag and recreating it, which is exactly
the case most likely to lose state.

1. Capture a baseline first, from the Unraid terminal, so the checks below are a
   comparison rather than an impression:
   ```bash
   sha256sum /mnt/user/appdata/backup-manager/state/state.db | tee /tmp/before-update.sha256
   find /mnt/user/backups -type f -printf '%p %s\n' | sort > /tmp/before-update.txt
   ```
2. Push or side-load a newer image tag.
3. **Docker → backup-manager → Force Update** (or edit the tag and Apply). Do the
   same for `backup-manager-ui`.
4. Compare afterwards:
   ```bash
   find /mnt/user/backups -type f -printf '%p %s\n' | sort > /tmp/after-update.txt
   diff /tmp/before-update.txt /tmp/after-update.txt
   ```

- [ ] Both containers recreate and return to healthy
- [ ] `diff` of the retained-artifact listing is empty: the update moved no
      backup data
- [ ] The administrator account still exists (no re-enrollment prompt)
- [ ] Logging back in with the same password works
- [ ] Every backup set is still configured
- [ ] Every artifact is still present and still listed
- [ ] Unraid's own **Update Available** indicator clears afterwards

---

## Step 7 — Container replacement

Same image, destroyed and recreated container. This is what a Unraid array stop,
an appdata backup restore, or a **Remove and re-add** does.

```bash
docker rm -f <engine container> <ui container>
```

Re-add both from the same user templates, changing nothing.

- [ ] Both containers come back healthy
- [ ] Retained backup data survives untouched
- [ ] The catalog survives
- [ ] The administrator account survives
- [ ] The re-added containers pick up the saved template values, so nothing had
      to be retyped

---

## Step 8 — Remove

This is the destructive-safety step. Its evidence was captured back in the
storage step, because after the removal there is nothing left to compare
against, and any deletion the comparison turns up is a release blocker rather
than a finding to triage.

1. **Docker → backup-manager → Remove**, and remove the image too.
2. Repeat for `backup-manager-ui`.

- [ ] Both containers are gone

Check the backup root against the baseline recorded in the storage step, before
looking at anything else:

```bash
sha256sum -c /root/backup-manager-acceptance/canary.sha256
find /mnt/user/backups/backup-manager -type f -printf '%p %s\n' | sort > /root/backup-manager-acceptance/backup-root.after
diff /root/backup-manager-acceptance/backup-root.before /root/backup-manager-acceptance/backup-root.after
```

- [ ] `sha256sum -c` reports the canary `OK`
- [ ] The `diff` against the recorded listing is empty, so the backup root is
      untouched, byte for byte, and every artifact is still readable
- [ ] `/mnt/user/appdata/backup-manager` is untouched (Unraid does not delete
      appdata on container removal, and the package must not either)
- [ ] Nothing elsewhere in the `backups` share changed
- [ ] Nothing outside the declared host paths was touched
- [ ] Reinstalling with the same paths adopts the existing catalog rather than
      starting empty

---

## Step 9 — Community Applications readiness

Community Applications lists templates from a GitHub repository that CA's own
feed indexes. That submission is an external step CA maintainers control, and no
part of it can run on a developer laptop, so it lives here.

- [ ] The templates pass CA's own template checks
- [ ] `<TemplateURL>`, `<Project>`, `<Support>`, `<Icon>` and `<Overview>` all
      resolve to real, reachable URLs
- [ ] `<Category>` is a category CA actually recognises
- [ ] `<Requires>` states the `docker network create backup-manager`
      prerequisite from step 0.2 clearly enough that a first-time installer sees
      it before installing
- [ ] Installing from CA (not from a hand-copied file) produces the same result
      as steps 1 and 2

---

## Evidence (§68)

Fill this in in the same commit that flips Unraid from uncertified to certified.

| Field | Value |
| --- | --- |
| Provider / OS version | |
| Hardware or VM, model | |
| Architecture | |
| Package / image version | |
| Image reference used, and how it was made resolvable | |
| Install result (both templates) | |
| WebUI link result | |
| Auth result | |
| Storage result | |
| Update result | |
| Uninstall / removal result | |
| Retained-backup safety | |
| Community Applications result | |
| Evidence (logs, screenshots, transcripts, with secrets redacted) | |
| Executed by | |
| Date | |
