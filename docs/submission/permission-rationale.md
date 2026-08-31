# Permission rationale

Every store asks, in one form or another, why an application wants what it wants. This is
the answer, and each claim in it is checked against the shipped package rather than
asserted: `apps/common/packaging`'s preflight fails the build if any target's metadata
stops matching what is below.

## The short version

Backup Manager asks for less than almost anything else on a NAS. It drops every Linux
capability, runs as a user the administrator chooses, runs on a read-only root filesystem,
and gets exactly five paths, three of them read-only.

## Container privileges

| What | Value | Why |
|---|---|---|
| Privileged mode | never | Nothing this application does needs the host's capability set. Any packaged file that asks for it fails the preflight. |
| Capabilities | `cap_drop: ALL` | It opens no raw socket, changes no ownership, mounts nothing and signals no other process. There is no capability left to ask for. |
| `no-new-privileges` | on | Nothing inside the container is setuid, so nothing should be able to become so. |
| Root filesystem | read-only | The application writes only to the volumes below. A writable image is a place for something else to persist. |
| User | non-root, administrator's choice | It writes into directories on the NAS whose ownership is the administrator's, not the image's. The runtime image has no shell and no root step, so it cannot fix ownership for you and does not try. |
| Host namespaces | none | It joins no host network, PID, IPC or UTS namespace. The engine is not reachable from the LAN at all. |
| Devices | none | It needs no hardware. |
| Seccomp and AppArmor | platform defaults | Neither is relaxed. |

## Network

Two containers. The engine holds the state, the credentials and the API, and publishes no
port at all: it is reachable only from the web interface container, over a network scoped
to this deployment. The web interface container publishes exactly one port, which is the
only thing on the LAN that can reach any of this.

Outbound, it connects only to the SFTP sources the administrator configured. See the
privacy disclosure.

## Storage

Five paths, and the split between them is the point rather than an implementation detail.

| Path | Mode | Why |
|---|---|---|
| Application state | read-write | The SQLite catalog and the local administrator record. Private to the application. |
| Backup root | read-write | Where retained artifacts land. Kept a separate directory from application state so that installing, reinstalling and removing the application only ever touches paths it created. |
| Configuration file | **read-only** | Loaded and validated before the application opens a listener. It has no reason to write it. |
| SFTP private key | **read-only** | Supplied by the administrator, never generated, never baked into the image, never logged. |
| Pinned `known_hosts` | **read-only** | This is what makes a changed host key an alert rather than a silent reconnection. A writable `known_hosts` is one compromised process away from being repinned to somebody else's server. |

The three read-only mounts are checked as read-only by the preflight, not merely checked
as present: a profile that quietly drops the read-only flag still satisfies every "is the
path there" test ever written.

## Lifecycle

The package runs no installer script of its own where the platform does not mandate one,
downloads no code, and has no self-update mechanism. A new version is a new package that
goes through the store's review like the first one did.
