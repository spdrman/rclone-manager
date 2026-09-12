import { capabilities } from "@shared/platform/capabilities";
import { readLocalAccountSession } from "@shared/platform/localSession";
import type { PlatformBridge } from "@shared/types/platform";

/** Proxmox is a deployment environment, not an app store. We do not mimic or
 *  modify the Proxmox management UI; the host management plane stays separate (§29).
 *
 *  The supported model (WP4.5, apps/proxmox/README.md) is a dedicated guest
 *  acting as the container host, running the canonical OCI image: a VM by
 *  default, or an unprivileged LXC with nesting if you accept the caveats.
 *  Never the PVE host itself. The guest sees the shared host directory or
 *  dataset at /mnt/backupd; `storageMount` is its `backups` child, the
 *  backup root the wizard seeds a destination from, which is deliberately not
 *  the share root that also holds state, config and key material.
 *  distribution/packaging pins it to canonical.json. */
export const proxmoxBridge: PlatformBridge = {
  id: "proxmox",
  name: "Proxmox VE",
  integration: "standalone",

  deployment: {
    label: "Dedicated container host",
    storageMount: "/mnt/backupd/backups",
    adapterVersion: "proxmox 1.1.0"
  },

  capabilities: () => capabilities({}),

  // No native identity provider on this platform: the service's own session
  // cookie is the source of truth. Shared rather than copied, because the
  // six copies of this all read any refusal as "signed out" and told an
  // operator whose engine was unreachable that their session had gone
  // (#795). readLocalAccountSession answers that question only when the
  // service actually answered it.
  getAuthContext: readLocalAccountSession,

  async openExternal(url: string) {
    window.open(url, "_blank", "noopener,noreferrer");
  }
};
