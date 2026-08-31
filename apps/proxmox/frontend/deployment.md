# Proxmox VE deployment

Backup Manager runs as a standalone web app inside a **dedicated guest that acts
as the container host**: a VM by default, or an unprivileged LXC with nesting if
you accept the caveats. It runs the canonical OCI image there, never on the
Proxmox VE host, and it does not integrate with, extend, or modify the Proxmox
management UI.

| Setting | Value |
| --- | --- |
| Platform | Proxmox VE |
| Deployment | Dedicated container host (VM by default) |
| Storage mount | `/mnt/backup-manager/backups` (inside the guest) |
| Authentication | Backup Manager local account |

The storage mount is the backup root, not the share root. The guest sees the
shared PVE host directory or dataset at `/mnt/backup-manager`, and retained
artifacts land in its `backups` child, so state, config and key material sit
beside them rather than inside them.

The Proxmox host management plane stays entirely separate. Nothing in
`apps/proxmox/` touches the shared pages, and nothing in it is installed onto the
PVE host.

The full profile, including what the host contributes and what is deliberately
absent, is in [`../README.md`](../README.md).
