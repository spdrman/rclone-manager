# Proxmox VE deployment profile

Issue #86 / work package B4.5, `docs/EPIC-B-multi-nas.md` §72, the Proxmox
entry in §4A, and D-9 in §5.

Proxmox VE is the one target in Phase 4 with nothing to package into. It
has VM and LXC infrastructure, it has container templates, and it has no
third-party application store or UI integration model this product could
be listed in. §4A therefore puts it in **Tier C — a supported deployment
profile**, alongside OpenMediaVault and generic Docker, and defers any PVE
Web UI plugin indefinitely.

So this directory is documentation plus two metadata files, not a package
format. There is no builder, no installer and no lifecycle code here, and
`apps/common/packaging` fails the build if any appears.

## The supported deployment model

**A dedicated guest acts as the container host and runs the canonical OCI
image. The Proxmox VE host runs only its own management commands.**

WP4.5 says it directly:

> Do NOT install Docker or application daemons directly into the Proxmox
> VE host OS as a default design.

That rules out the shape people reach for first — a container engine on
the hypervisor, next to the cluster stack — and it rules out the various
community "helper scripts" that install a service onto the host and then
graft an entry into the PVE Web UI. Both leave the host's management
plane carrying software this project did not put through PVE's own
upgrade path, and both break the moment `pve-manager` is upgraded.

Two guest shapes are documented. The first is the default:

| Guest | Overhead | Isolation | Notes |
| --- | --- | --- | --- |
| **VM** (default) | Higher | Full kernel isolation | Proxmox's own guidance for running a container engine. Nothing about it is unusual or unsupported. |
| Unprivileged LXC (variant) | Lower | Shared kernel | Needs `features: nesting=1,keyctl=1`. Proxmox does not support a container engine inside an LXC; if it misbehaves after a PVE upgrade, that is the trade you took. Keep it unprivileged. |

Both run the identical `compose/backup-manager.yml`. The only difference
is how the guest is created and how the host directory reaches it.

WP4.5's other listed option, an unprivileged LXC running the app binaries
directly under systemd, was not chosen. It would mean a second
distribution channel for the same product — unit files, an installer, an
updater, a removal path — which is the "provider-specific lifecycle
implementation" the Phase 4 TDD Gate rules out, for the one provider that
gets the least integration in return. Running the canonical image inside a
guest gets the same result with no new lifecycle surface, and every
conformance check the other container profiles already pass applies to it
unchanged.

## What the PVE host contributes

Exactly one thing: a directory or dataset, shared into the guest at
`/mnt/backup-manager`.

```bash
zfs create -o mountpoint=/srv/backup-manager rpool/backup-manager
```

For a VM, share it in with a virtiofs directory mapping (PVE 8.4 and
later) or an NFS export. For the LXC variant, a bind mount point:

```bash
pct set <ctid> --mp0 /srv/backup-manager,mp=/mnt/backup-manager
```

Everything persistent lives under that one path, split into four
directories that are four different things:

| Guest path | Holds | Why it is separate |
| --- | --- | --- |
| `/mnt/backup-manager/state` | SQLite catalog, administrator record | Private application state (§19.2) |
| `/mnt/backup-manager/backups` | Retained artifacts | The user backup root, a separate security domain |
| `/mnt/backup-manager/config` | `config.yaml`, mounted read-only | Validated before the listener opens |
| `/mnt/backup-manager/secrets` | SSH key, pinned `known_hosts`, read-only | Never inside the backup root, never in this repository |

`apps/common/packaging` enforces the containment rule in the last column
on every commit: no key material, config or authentication state may sit
inside the backup root, and the backup root may not contain any of them.

The single-host-path layout is also the recovery story. There is no named
Docker volume anywhere in the profile, so `docker compose down -v` inside
the guest reaches nothing, and destroying the guest entirely leaves every
byte on the PVE host. Rebuild the guest, re-point it at the same
directory, `docker compose up -d`, and the same backup sets and the same
administrator account come back.

## Network and ports

The Web UI container publishes one port inside the guest, `8080` by
default. The engine container publishes nothing and is reachable only over
the profile's private compose network — that is what keeps the state
database, the credentials and the API off the guest's own interfaces.

Reach the UI at the **guest's** address, not the host's. It is not behind
the PVE Web UI and it does not share its port; PVE's 8006 is untouched.
If you want it on a name instead of an address, put a reverse proxy in
front of the guest, the same as any other web service on the network.

## Authentication

Reusable local authentication (§13A), the same as every other Tier B and C
provider. There is no PVE realm integration, no PAM hook, no PVE API
token, and nothing reads a PVE session. The first start prints a one-time
enrollment link; enrolling sets an administrator password stored as an
Argon2id hash. Set `PUBLIC_BASE_URL` in the env file to the guest's real
address or that link points at `localhost` and only works on the guest.

## Update

Pull a new canonical image tag inside the guest and recreate the
containers:

```bash
docker compose pull && docker compose up -d
```

There is no PVE package to upgrade, no host service to restart and no
migration step. State is on the shared host directory, so container
replacement preserves it by construction. `docs/recovery.md` covers what
to do when it does not.

## What is deliberately absent

- no PVE Web UI plugin, panel, menu entry or injected asset (§4A defers it);
- no file installed into the host's cluster configuration filesystem;
- no host package, systemd unit, or cron entry;
- no container engine on the host;
- no PVE API token, realm, or session integration;
- no helper script that does any of the above on your behalf.

`apps/common/packaging`'s host-management-plane scanner checks the ones
that are decidable from this directory. Step 9 of
[`docs/acceptance/proxmox-ve-deployment.md`](../../docs/acceptance/proxmox-ve-deployment.md)
checks the rest on a real host, by diffing the host's packages, enabled
units, cluster configuration and `pve-manager` assets across a full
install-update-remove cycle.

## Status

**Build-supported and uncertified** (§68). The acceptance procedure is
written and version-controlled at
[`docs/acceptance/proxmox-ve-deployment.md`](../../docs/acceptance/proxmox-ve-deployment.md),
and it has not been executed: no Proxmox VE host, physical or virtual, was
available where this profile was built. Every automated check in
`apps/common/packaging` passes, which proves the metadata is well-formed
and consistent with the other six providers. It proves nothing about how
PVE behaves.

The per-capability conformance results for this profile, next to all six
other providers, are in
[`docs/conformance/phase-4-matrix.md`](../../docs/conformance/phase-4-matrix.md).
