# Backup Manager on ZimaOS

ZimaOS is built on CasaOS and reads the same `x-casaos` block out of a
`docker-compose.yml`, so [`compose/backup-manager.yml`](compose/backup-manager.yml)
is both the runtime definition and the store submission. There is nothing else
in this directory but an icon and this page.

This is new platform support, not a conversion. No Phase 4 issue targeted
ZimaOS, so nothing here replaces earlier packaging.

## The split that makes it an adapter

Everything outside `x-casaos` is Compose and container semantics **derived** from
`container/compose.yaml` at runtime contract 1.1.0. Seven fields have one
authority each and a mismatch names the field
(`distribution/packaging/derive.go`); on top of that the whole stack is held to
the canonical one semantically, service by service, by
`TestEveryNewAdapterIsSemanticallyEquivalentToTheCanonicalStack`, and that check
is proved against a deliberate mismatch of every property it compares.

Everything inside `x-casaos` is store presentation: app identity, icon, title,
category, description, the architectures the app claims, and how the ports,
environment values and volumes are presented in ZimaOS's own install dialog. It
reaches no Go package and no shared UI module, and
`TestStoreMetadataStaysInTheDistributionAdapter` fails the build if `x-casaos`
ever appears under `core/` or `ui/shared/src/`.

## No variables, on purpose

A ZimaOS store install deploys the file as it stands, with no `.env` beside it.
A `${STATE_DIR}` in it would be an unresolved reference at install time rather
than a knob an operator can turn, so every path, port and id below is literal,
and every one of them is ZimaOS's own layout: private application data under
`/DATA/AppData`, user data under `/DATA`.

## Before you install

Create the directories and give them to uid 1000 / gid 1000. The runtime image
is distroless, with no shell and no root step, so nothing inside the container
can create or chown them for you:

```
mkdir -p /DATA/AppData/backup-manager/state /DATA/AppData/backup-manager/config \
         /DATA/AppData/backup-manager/secrets /DATA/Backups/backup-manager
chown 1000:1000 /DATA/AppData/backup-manager/state /DATA/AppData/backup-manager/config \
                /DATA/AppData/backup-manager/secrets /DATA/Backups/backup-manager
```

Put the SFTP private key at `/DATA/AppData/backup-manager/secrets/id_ed25519`
(mode 0600) and the pinned host key next to it as `known_hosts`. Neither is ever
baked into the image or into any file in this repository.

Then install it through ZimaOS's app store with this file, or submit it to the
ZimaOS store. The engine prints a one-time enrollment link on first start; read
it in the container log.

The full operator procedure, including update, removal and the evidence that
retained backups survived, is
[`docs/acceptance/zimaos-app-store-install.md`](../../docs/acceptance/zimaos-app-store-install.md).
Nothing in it has been executed: it needs a ZimaOS host, and no criterion in it
is ticked.

## Which mount holds what

| Host path | Container path | Holds |
| --- | --- | --- |
| `/DATA/AppData/backup-manager/state` | `/data/state` | the catalogue and the local administrator record. Private. |
| `/DATA/Backups/backup-manager` | `/data/backups` | retained artifacts, and nothing else. |
| `/DATA/AppData/backup-manager/config` | `/etc/backup-manager/config` | `config.yaml`, writable, plus `ssh_keys/` and `known_hosts.d/`. |
| `/DATA/AppData/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | the SFTP private key, read-only. |
| `/DATA/AppData/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | the pinned host key, read-only. |

Private state and the backup root are separate security domains and neither is
inside the other, which is why the backup root is under `/DATA` and not under
`/DATA/AppData`.

## What ZimaOS does not give this product, stated rather than emulated

ZimaOS has a user account of its own. This app does not use it: sign-in is the
product's own local account, over its own session cookie, and no ZimaOS identity
is trusted. Wiring a provider-native identity bridge is explicitly not in this
issue, and a bridge that trusted a header without an authenticated gateway in
front of it would be worse than none.

There is no notification bridge, no embedded window and no native storage picker
either. The web UI is the shared one and reports the deployment as a Docker
Compose deployment, which under ZimaOS it is.

The reason it is not a ZimaOS-branded bridge is measured rather than assumed. A
bridge has to be carried somewhere, and there are exactly three carriers
(`docs/runtime-contract.md`). The canonical image already carries five bundles
and has **347,956 bytes** of headroom against its gated 5% ceiling, less than
one more bundle at roughly 352 KB, so it cannot carry a sixth. A store compose
file has no payload of its own. And a bridge would need its platform id in the
`/api/v1` contract, the capability table, the profile table and the bundle list,
which is core and shared-UI code, in an adapter whose own contract says no
ZimaOS import may enter either.

## ZimaOS and CasaOS are one runtime and two registrations

`apps/casaos/` holds the same services, the same paths and the same `x-casaos`
shape, because ZimaOS is built on CasaOS and lays `/DATA` out the same way. They
are two directories because they are two stores, submitted separately and
certified separately, so the release matrix carries two rows. They are one
deployment.
