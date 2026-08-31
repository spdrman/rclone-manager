# Proxmox VE deployment profile acceptance procedure

**Status: NOT EXECUTED. Proxmox VE is build-supported and uncertified (§68).**

This is the procedure an operator runs on a real Proxmox VE host to certify the
deployment profile in `apps/proxmox/`. I wrote it first, before the profile assets
existed, because §68 requires the procedure to be written and version-controlled
before anyone executes it, and because writing it first is what settled which
deployment model the profile should support at all.

Nothing in it has been run. No Proxmox VE host, physical or virtual, was available
on the machine the profile was built on, and no step below has been simulated,
approximated, or ticked from a laptop.

Required evidence per §68: a **current PVE release test host or VM environment**.
Record the exact release in the evidence table.

Everything the repository itself can decide is already checked by
`distribution/packaging` on every commit, including the per-capability conformance
matrix in `docs/conformance/phase-4-matrix.md`. This procedure covers only what a
laptop cannot reach.

---

## Scope: the one supported deployment model

Section 4A puts Proxmox VE in **Tier C**: a supported deployment profile, not an
app-store package, because PVE has no third-party application store to package
into. Work Package 4.5 asks for exactly one documented safe model, and says in as
many words:

> Do NOT install Docker or application daemons directly into the Proxmox VE host
> OS as a default design.

The model this procedure certifies is therefore:

**A dedicated guest acts as the container host and runs the canonical OCI image.
The PVE host runs only its own management commands.**

Two guest shapes are documented, and this procedure certifies the first:

| Guest | When | Notes |
| --- | --- | --- |
| **VM (default)** | Always safe | Proxmox's own guidance for running containers is to run the container engine inside a VM. Full kernel isolation from the host. |
| Unprivileged LXC (variant) | Lower overhead, you accept the caveats | Needs `features: nesting=1,keyctl=1`. Proxmox does not support a container engine inside an LXC. Documented in `apps/proxmox/README.md`, certified separately if you use it. |

What is explicitly **out**, and what step 9 exists to prove did not happen:

- no package installed into the PVE host OS;
- no container engine on the PVE host;
- no application daemon, systemd unit, or cron entry on the PVE host;
- no file written under `/etc/pve`, no `pvesh`/`pveproxy`/`pvedaemon` change;
- no patched or injected JavaScript, CSS, or template in the PVE Web UI;
- no third-party "helper script" that edits any of the above.

The only things this procedure runs on the host itself are `pvesm`, `qm`, `pct`,
`zfs`/`mkdir`, and `ssh` — PVE's own supported management surface, used the way the
PVE administration guide documents.

---

## Step 0 — Prerequisites

### 0.1 Record the host

```bash
pveversion -v | head -3
```

- [ ] PVE release recorded in the evidence table
- [ ] Host has a storage the guest's disk can live on (`pvesm status`)

### 0.2 Choose and create the storage the app will use

The profile keeps every persistent path under one host directory or dataset, which
is mounted into the guest at `/mnt/backup-manager`. On ZFS:

```bash
zfs create -o mountpoint=/srv/backup-manager rpool/backup-manager
```

or on a plain directory storage:

```bash
mkdir -p /srv/backup-manager
```

- [ ] Host path created, recorded in the evidence table
- [ ] It is **not** inside `/etc/pve`, `/var/lib/pve-cluster`, or any PVE-managed path

### 0.3 Create the guest

**Pick the VMID once, here, and prove it is free before you use it.** Everything
below refers to `$VMID`, including the destroy in step 8, and 9000 is a common
id that is very often already in use on a real host. `qm destroy` on someone
else's guest succeeds without asking, so a mismatch between the id you created
and the id you destroy is unrecoverable and silent.

```bash
export VMID=9000                     # or any id of your own
qm status "$VMID"; pct status "$VMID"
```

Both commands MUST fail with "does not exist". If either one prints a status,
that id belongs to an existing guest: pick another and re-run until both fail.

- [ ] `qm status $VMID` and `pct status $VMID` both reported no such guest
- [ ] The chosen VMID is written into the evidence table now, before anything is
      created

Default (VM). Use any current Debian or Ubuntu LTS cloud image:

```bash
qm create "$VMID" --name backup-manager --memory 2048 --cores 2 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single
# import the cloud image, set --scsi0, --ide2 cloudinit, --boot order=scsi0
qm set "$VMID" --ciuser admin --sshkeys ~/.ssh/id_ed25519.pub
qm start "$VMID"
```

Share the host directory into the VM with virtiofs (PVE 8.4 and later):

```bash
pvesm set <dirstorage> --content import      # if needed for the mapping
qm set "$VMID" --virtio0 ...                 # or configure a directory mapping
```

If your PVE release predates virtiofs directory mappings, use an NFS or 9p share
from the host, or give the VM its own disk and skip the host-side dataset. Record
which you used.

- [ ] Guest created and reachable over SSH
- [ ] Host directory visible inside the guest at `/mnt/backup-manager`
- [ ] `qm config $VMID` recorded

**Variant (unprivileged LXC).** Only if you accept the caveats in
`apps/proxmox/README.md`:

```bash
pct create "$VMID" <template> --unprivileged 1 --features nesting=1,keyctl=1 \
  --memory 2048 --cores 2 --net0 name=eth0,bridge=vmbr0,ip=dhcp
pct set "$VMID" --mp0 /srv/backup-manager,mp=/mnt/backup-manager
pct start "$VMID"
```

- [ ] `pct config $VMID` shows `unprivileged: 1`
- [ ] `pct config $VMID` shows the bind mount, and **no** `mp` pointing at `/etc/pve`

### 0.4 Install the container engine **inside the guest**, never on the host

```bash
ssh admin@<guest> 'curl -fsSL https://get.docker.com | sh'
ssh admin@<guest> 'docker --version && docker compose version'
```

- [ ] The engine is installed in the guest
- [ ] `which docker` on the **PVE host** still returns nothing (step 9 re-checks this)

### 0.5 Make the canonical image resolvable

No registry is configured for this repository yet
(`distribution/packaging/canonical.json` records `published: false`), so the
reference in the compose file resolves to nothing until you point it somewhere.
Either push to your own registry:

```bash
docker buildx build \
  --platform=linux/amd64,linux/arm64 \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -f container/Dockerfile \
  -t <your-registry>/backup-manager:<version> \
  --push .
```

or side-load into the guest and set `IMAGE` in the env file to the loaded tag:

```bash
docker save backup-manager:<version> | gzip > backup-manager.tar.gz
scp backup-manager.tar.gz admin@<guest>:/tmp/
ssh admin@<guest> 'gunzip -c /tmp/backup-manager.tar.gz | docker load'
```

The compose file reads the image reference from a single `IMAGE` variable in
`apps/proxmox/compose/backup-manager.env`, so this is one line in one file.

- [ ] Canonical image resolvable inside the guest, reference recorded

### 0.6 Resolve paths, ownership, key material and config

```bash
ssh admin@<guest> 'sudo mkdir -p /mnt/backup-manager/{state,backups,config,secrets}'
```

The runtime image is distroless: no shell, no root step, nothing inside the
container can chown anything at startup, so the host-side owner has to be right
before the first start.

Create the SSH key and pinned `known_hosts` **on the guest**, following
`docs/ssh-setup.md`. Never commit either to this repository, never paste a private
key into the evidence table, and never put one in `apps/proxmox/`.

```bash
ssh admin@<guest> 'ssh-keygen -t ed25519 -N "" -f /mnt/backup-manager/secrets/id_ed25519'
ssh admin@<guest> 'ssh-keyscan -t ed25519 <sftp-host> > /mnt/backup-manager/secrets/known_hosts'
```

**Chown last, once the files exist.** `ssh-keygen` writes the private key owned
by whoever ran it, which is `admin`, and the shell redirect above does the same
for `known_hosts`. A recursive chown before those two commands leaves both owned
by `admin`, the container running as `1000:100` cannot read either, and every
SFTP connection fails with a permission error that points at the key rather than
at this step. This is the same ordering the TrueNAS, Unraid and OpenMediaVault
procedures already use.

**Recurse only over what this step created.** `/mnt/backup-manager` is the shared
host directory, and `/mnt/backup-manager/backups` is the retained backup store: on
a reinstall both already hold data this procedure did not write, and a `chown -R`
across either rewrites the ownership of all of it with nothing to restore it from.
So the two private trees are chowned recursively and the share root and the backup
root get their own mountpoint chowned, nothing beneath it. This is the same split
the TrueNAS, Unraid and OpenMediaVault procedures use, and
`distribution/packaging` fails the build if any of the four recurses over the
backup root or a parent of it.

```bash
ssh admin@<guest> '
  sudo chown -R 1000:100 /mnt/backup-manager/state /mnt/backup-manager/config /mnt/backup-manager/secrets
  sudo chown 1000:100 /mnt/backup-manager /mnt/backup-manager/backups
  sudo chmod 600 /mnt/backup-manager/secrets/id_ed25519
  sudo -u "#1000" cat /mnt/backup-manager/secrets/id_ed25519 > /dev/null && echo readable
'
```

- [ ] `/mnt/backup-manager/{state,backups,config,secrets}` exist, owned by the app's uid/gid
- [ ] The recursive chown touched only `state`, `config` and `secrets`; the share
      root and `backups` were chowned as mountpoints, not as trees
- [ ] The chown ran **after** the key and `known_hosts` were created
- [ ] `sudo -u '#1000' cat .../secrets/id_ed25519` succeeded, and the key is mode 600
- [ ] `/mnt/backup-manager/config` exists and is **writable** by the app's uid/gid
- [ ] `config/config.yaml` written inside it and valid
- [ ] Key material lives only on the guest, redacted everywhere else

---

## Step 1 — Deploy

Confirm the shared host directory is actually mounted in the guest before you
start anything. Every host path in the profile is a `${VAR:?}` reference, so an
unset variable stops the deployment, but a variable that is set correctly while
the virtiofs, NFS or `mp0` mapping never came up is a different failure: Docker
would create the bind sources on the guest's own root disk and the deployment
would look healthy while writing the state database and every retained artifact
somewhere the recovery story in step 8 cannot find them.

```bash
ssh admin@<guest> 'mountpoint -q /mnt/backup-manager && echo mounted'
```

- [ ] `mountpoint -q /mnt/backup-manager` succeeded in the guest, before `up -d`

```bash
scp apps/proxmox/compose/backup-manager.yml admin@<guest>:/opt/backup-manager/
scp apps/proxmox/compose/backup-manager.env admin@<guest>:/opt/backup-manager/.env
ssh admin@<guest> 'cd /opt/backup-manager && docker compose -f backup-manager.yml up -d'
```

- [ ] Both containers reach `running`
- [ ] `backup-manager` reports healthy
- [ ] `backup-manager-ui` reports healthy (it overrides the image's own healthcheck)
- [ ] `docker compose logs` shows no repeated restart

## Step 2 — Reproducibility

The first acceptance criterion is that the deployment is **reproducible**, which
means a second operator following this file from a clean guest lands in the same
place. Prove it rather than asserting it:

```bash
qm clone "$VMID" "$((VMID + 1))" --name backup-manager-repro   # or pct clone
```

Bring the clone up from step 0.5 onward against a *separate* host directory, using
the same two files and no manual edits beyond the env file's documented
substitutions.

- [ ] Second guest reaches the same running state from the same two files
- [ ] The only edits needed were inside `backup-manager.env`
- [ ] Number of undocumented manual steps required: **must be zero**, record it

## Step 3 — Web UI access

PVE has no application navigation tree to appear in, by design. The Web UI is
reached at the guest's own address and published port.

- [ ] `http://<guest>:8080/` loads the shared Web UI
- [ ] The UI reports the platform as Proxmox VE
- [ ] The deployment label shown matches `apps/proxmox/frontend/platform.ts`
- [ ] Nothing was added to the PVE Web UI to make this reachable

## Step 4 — Authentication (local-account only)

- [ ] First start printed a one-time enrollment link (keep it out of the evidence table)
- [ ] Enrollment sets an administrator password, stored as an Argon2id hash
- [ ] The enrollment link is single-use and rejected the second time
- [ ] An unauthenticated request to `/api/v1/` is refused
- [ ] The UI reports auth mode `local-account`, not a PVE session
- [ ] No PVE realm, PAM user, or PVE API token was created or used

## Step 5 — Storage mapping and backup-root containment

- [ ] State lands under the host path mapped to `/mnt/backup-manager/state`
- [ ] Retained artifacts land under the host path mapped to `/mnt/backup-manager/backups`
- [ ] No SSH private key, `known_hosts`, config file or auth record exists anywhere
      inside the backup root (§19.2)
- [ ] The key and `known_hosts` are mounted read-only, and a write attempt from
      inside the container fails
- [ ] The configuration directory is mounted **writable**, and a write attempt from
      inside the container succeeds (issue #196: the engine creates and atomically
      replaces `config.yaml` there, and keeps `ssh_keys/` and `known_hosts.d/`
      beside it). These two boxes are each other's control: if both write attempts
      behave the same way, the mount modes are not being tested at all

## Step 6 — Engine reachability

- [ ] The engine container publishes no port (`docker compose ps` shows one published port total)
- [ ] `curl http://<guest>:8080/api/v1/...` works through the UI container
- [ ] The engine's own port is not reachable from outside the guest
- [ ] The PVE host's 8006 management port is unaffected

## Step 7 — Update

Update means pulling a new canonical image tag and recreating the containers. It
is not a PVE operation, there is no PVE package to upgrade, and nothing on the
host changes.

Capture a baseline before the pull, then compare, so "everything survived" is a
diff rather than an impression:

```bash
ssh admin@<guest> '
  sha256sum /mnt/backup-manager/state/state.db | tee /tmp/before-update.sha256
  find /mnt/backup-manager/backups -type f -printf "%p %s\n" | sort > /tmp/before-update.txt
'
ssh admin@<guest> 'cd /opt/backup-manager && docker compose pull && docker compose up -d'
ssh admin@<guest> '
  find /mnt/backup-manager/backups -type f -printf "%p %s\n" | sort > /tmp/after-update.txt
  diff /tmp/before-update.txt /tmp/after-update.txt
'
```

- [ ] `diff` of the retained-artifact listing is empty: the update moved no
      backup data
- [ ] New image version reported by the UI
- [ ] Backup sets, schedules, retained artifacts and the administrator account all survive
- [ ] No re-enrollment was required
- [ ] Nothing on the PVE host changed (step 9 re-checks)

## Step 8 — Recovery, removal, and retained-backup safety

Recovery from a lost guest is the reason every persistent path is a host bind
mount rather than a named volume: rebuild the guest from step 0.3, re-point it at
the same host directory, and the state is still there.

**Capture the baseline first, on the PVE host, and write it OUTSIDE the directory
you are about to test.** Step 9 below already does exactly this for the host's
packages and units; the operator's retained backups deserve the same treatment,
and without it the checkbox below is ticked from a directory listing that nothing
was compared against. This is also the one provider whose backup root is reached
through a virtiofs or `mp0` mapping, so it is the one where a mapping problem can
silently empty the guest's view of it.

```bash
dd if=/dev/urandom of=/srv/backup-manager/backups/acceptance-canary.bin bs=1M count=8
sha256sum /srv/backup-manager/backups/acceptance-canary.bin | tee /root/pve-canary.sha256
find /srv/backup-manager -type f -printf '%p %s\n' | sort > /root/pve-before-destroy.txt
```

Confirm the id you are about to destroy is the one this procedure created:

- [ ] `echo "$VMID"` prints the id recorded in the evidence table at step 0.3,
      and `qm config "$VMID"` (or `pct config "$VMID"`) shows the guest this
      procedure built. **Do not run the next command until it does.**

```bash
qm stop "$VMID" && qm destroy "$VMID"      # or pct stop / pct destroy
```

Then verify against the baseline, before doing anything else:

```bash
sha256sum -c /root/pve-canary.sha256
find /srv/backup-manager -type f -printf '%p %s\n' | sort > /root/pve-after-destroy.txt
diff /root/pve-before-destroy.txt /root/pve-after-destroy.txt
```

- [ ] `sha256sum -c /root/pve-canary.sha256` says OK and
      `diff /root/pve-before-destroy.txt /root/pve-after-destroy.txt` is empty
- [ ] A fresh guest re-created from step 0.3 onward, pointed at the same host
      directory, comes up with the same backup sets and the same administrator account
- [ ] `docker compose down -v` inside the guest deletes no retained artifact:
      re-run the same `sha256sum -c` and `diff` after it and both are still
      clean (there is no named volume for `-v` to reach)
- [ ] Removing the profile removes no core behaviour: the same image runs
      unchanged under `container/compose.yaml` on a plain Docker host

## Step 9 — The PVE host management plane is untouched

This is acceptance criterion two, and it is the whole reason the model is a
dedicated guest. Run every check **on the PVE host**, not in the guest.

Take a baseline before step 0.3 and compare after step 8:

```bash
# before
dpkg -l > /root/pve-baseline-packages.txt
systemctl list-unit-files --state=enabled > /root/pve-baseline-units.txt
find /etc/pve -type f -newermt '-1 day' > /root/pve-baseline-etcpve.txt
sha256sum /usr/share/pve-manager/js/pvemanagerlib.js >> /root/pve-baseline-units.txt
```

- [ ] `dpkg -l` differs by nothing this procedure installed
- [ ] No new enabled systemd unit on the host
- [ ] `which docker`, `which podman`, `which containerd` all empty on the host
- [ ] No new file under `/etc/pve`
- [ ] `pvemanagerlib.js` checksum unchanged, and no file added under `/usr/share/pve-manager/`
- [ ] No crontab or `/etc/cron.*` entry added
- [ ] The PVE Web UI at `https://<host>:8006/` looks and behaves exactly as before,
      with no added menu item, panel, or tab
- [ ] `pveversion -v` output unchanged
- [ ] `systemctl status pveproxy pvedaemon pve-cluster` all still active, never restarted by this procedure

If any of these differ, the deployment is **not** conformant and the evidence table
must say so rather than being filled in green.

## Step 10 — Destructive-safety re-check for the new bind mount

The host directory or dataset is new containment surface, so re-run the
destructive-safety expectations against it specifically:

- [ ] A backup set configured with a root outside `/mnt/backup-manager/backups` is refused
- [ ] A symlink placed inside the backup root that points outside it is not followed
      into a delete
- [ ] A retention apply deletes only artifacts under the backup root
- [ ] Nothing under `/mnt/backup-manager/{state,config,secrets}` is ever a delete target
- [ ] Destroying the guest mid-operation leaves the state database recoverable

## Step 11 — Cross-check against the automated matrix

```bash
cd distribution && go test ./packaging/ -run TestCrossProviderConformance -v
```

- [ ] Every Proxmox row the matrix reports as `PASS` still holds on the real host
- [ ] Every row it reports as `PENDING_OPERATOR` is now decided by this procedure
- [ ] No row the matrix reports as `UNSUPPORTED` turned out to be supported here
      (if one did, `distribution/packaging/conformance.json` is stale and must be corrected)

---

## Evidence (§68)

Fill this in in the same commit that flips Proxmox VE from uncertified to
certified.

| Field | Value |
| --- | --- |
| Provider / OS version (`pveversion -v`) | |
| Host hardware or nested-virt environment | |
| Architecture | |
| Guest shape used (VM or unprivileged LXC) and its config | |
| VMID chosen at step 0.3, and the evidence both `qm status` and `pct status` reported it free | |
| Host storage / dataset used for `/mnt/backup-manager` | |
| How the host directory was shared into the guest | |
| Package / image version | |
| Image reference used, and how it was made resolvable | |
| Deploy result | |
| Reproducibility result (second guest, undocumented manual steps) | |
| Web UI access result | |
| Auth result | |
| Storage result and backup-root containment | |
| Update result | |
| Recovery result (guest destroyed and rebuilt) | |
| Removal result | |
| Retained-backup safety | |
| Host management plane diff (packages, units, `/etc/pve`, `pvemanagerlib.js`) | |
| Destructive-safety re-check result | |
| Conformance matrix cross-check result | |
| Evidence (logs, screenshots, transcripts, with secrets redacted) | |
| Executed by | |
| Date | |
