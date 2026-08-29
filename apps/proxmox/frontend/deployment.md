# Proxmox VE deployment

Backup Manager runs as a standalone web app inside an **unprivileged LXC**. It
does not integrate with, extend, or modify the Proxmox management UI.

| Setting | Value |
| --- | --- |
| Platform | Proxmox VE |
| Deployment | Unprivileged LXC |
| Storage mount | `/mnt/backup-manager` |
| Authentication | Backup Manager local account |

The Proxmox host management plane stays entirely separate. Nothing in
`apps/proxmox/` touches the shared pages.
