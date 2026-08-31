# Backup Manager on Portainer

Portainer CE deploys this product as a **stack**, from the App Template in
[`templates.json`](templates.json) or from the same file pasted into Portainer's
own Custom Templates. That is the whole integration.

This is new platform support, not a conversion. Nothing in Phase 4 targeted
Portainer, so there was no earlier Portainer packaging to convert and nothing
here replaces anything.

## What this adapter deliberately is not

No Portainer plugin. No Portainer agent. No call to the Portainer API. No second
application server, and no code of any kind: this directory holds one compose
file, one env file, one App Template and this page.

Portainer's support model for a third-party application is a template plus a
compose file, so building anything more would be building a second product to
expose the first one. Issue #170 rules it out by name and
`distribution/packaging` fails the build if a `.go`, a `.ts` or a script appears
under this directory.

## The Docker socket, which is the only interesting security question here

Portainer holds `/var/run/docker.sock`. That is what Portainer is, and it is
Portainer's business. **Backup Manager never inherits it.** The stack mounts no
socket, adds no capability, runs non-root on a read-only root filesystem, and
would behave identically if it had been started with `docker compose up` and
Portainer uninstalled.

That is checked rather than promised, in two places that cannot both be edited
by accident: `distribution/compose` runs the runtime contract's prohibition list
over this stack (socket mounts, privileged mode, host networking, host PID and
IPC namespaces, added capabilities, unconfined security profiles), and
`distribution/packaging` runs the same host-path prohibition over the `Service`
shape every metadata format reduces to.

## Deploying it

1. Create the four host directories and give them to the uid and gid you will
   run as. The runtime image is distroless: it has no shell and no root step, so
   nothing inside the container can create or chown them for you.

   ```
   mkdir -p /opt/backup-manager/state /opt/backup-manager/backups \
            /opt/backup-manager/config /opt/backup-manager/secrets
   chown 1000:1000 /opt/backup-manager/state /opt/backup-manager/backups \
                   /opt/backup-manager/config /opt/backup-manager/secrets
   ```

2. Put the SFTP private key at `/opt/backup-manager/secrets/id_ed25519` (mode
   0600) and the pinned host key at `/opt/backup-manager/secrets/known_hosts`.
   Neither is ever baked into the image or into any file in this repository.

3. Register the template. In Portainer, **Settings, App Templates**, and point
   the URL at this repository's `apps/portainer/templates.json`. On a host that
   cannot reach the repository, use **Custom Templates, Add, Repository** or
   paste `compose/backup-manager.yml` in directly.

4. Deploy it from **App Templates**, fill the form, and open the published port.
   The engine prints a one-time enrollment link on first start; read it from the
   engine container's log in Portainer.

The full operator procedure, including update, removal and the evidence that
retained backups survived, is
[`docs/acceptance/portainer-stack-deployment.md`](../../docs/acceptance/portainer-stack-deployment.md).
Nothing in it has been executed: it needs a Portainer host, and no criterion in
it is ticked.

## Which mount holds what

| Host path | Container path | Holds |
| --- | --- | --- |
| `/opt/backup-manager/state` | `/data/state` | the catalogue and the local administrator record. Private. |
| `/opt/backup-manager/backups` | `/data/backups` | retained artifacts, and nothing else. |
| `/opt/backup-manager/config` | `/etc/backup-manager/config` | `config.yaml`, writable, plus the engine's `ssh_keys/` and `known_hosts.d/` stores. |
| `/opt/backup-manager/secrets/id_ed25519` | `/etc/backup-manager/id_ed25519` | the SFTP private key, read-only. |
| `/opt/backup-manager/secrets/known_hosts` | `/etc/backup-manager/known_hosts` | the pinned host key, read-only. |

Private state and the backup root are separate security domains and neither one
is inside the other. `distribution/packaging` fails the build if that stops
being true.

## Which UI this shows, and why

The web UI is the shared one, served from the bundle compiled into the binary,
which reports the deployment as a Docker Compose deployment. That is accurate:
through Portainer it is one.

It is also the only honest option available. A Portainer-specific bridge would
have to be carried somewhere, and there are exactly three carriers
(`docs/runtime-contract.md`). The canonical image already carries five bundles
and has **347,956 bytes** of headroom against its gated 5% ceiling, which is
less than one more bundle at roughly 352 KB, so the image cannot carry a sixth.
A template has no payload of its own to carry one in. And a bridge would need
its platform id in the `/api/v1` contract, the capability table, the profile
table and the bundle list, which is core and shared-UI code in a platform whose
whole point is that it needs none.

So Portainer reports no native authentication, no native notifications, no
embedded window and no storage picker, because it has none of them, and the
generic bridge says exactly that rather than claiming otherwise.

## Where the runtime definition comes from

`compose/backup-manager.yml` is derived from `container/compose.yaml` at runtime
contract 1.1.0. Seven fields have one authority each and a mismatch names the
field (`distribution/packaging/derive.go`), and on top of that the whole stack is
held to the canonical one semantically, service by service, by
`TestEveryNewAdapterIsSemanticallyEquivalentToTheCanonicalStack`. The App
Template's environment list is checked against `compose/backup-manager.env` in
both directions, so the form an operator fills in and the file it feeds can
never name different variables.
