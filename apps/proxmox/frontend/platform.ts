import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** Proxmox is a deployment environment, not an app store. We do not mimic or
 *  modify the Proxmox management UI; the host management plane stays separate (§29).
 *
 *  The supported model (WP4.5, apps/proxmox/README.md) is a dedicated guest
 *  acting as the container host, running the canonical OCI image: a VM by
 *  default, or an unprivileged LXC with nesting if you accept the caveats.
 *  Never the PVE host itself. `storageMount` is where the guest sees the host
 *  directory or dataset, and apps/common/packaging pins it to canonical.json. */
export const proxmoxBridge: PlatformBridge = {
  id: "proxmox",
  name: "Proxmox VE",
  integration: "standalone",

  deployment: {
    label: "Dedicated container host",
    storageMount: "/mnt/backup-manager",
    adapterVersion: "proxmox 1.1.0"
  },

  capabilities: () => capabilities({}),

  async getAuthContext(): Promise<AuthContext> {
    // No native identity provider on this platform: the service's own session
    // cookie is the source of truth.
    const res = await fetch("/api/v1/auth/session", { credentials: "same-origin" });
    if (!res.ok) return { authenticated: false, username: null, mode: "local-account" };
    const body = (await res.json()) as { username: string };
    return { authenticated: true, username: body.username, mode: "local-account" };
  },

  async openExternal(url: string) {
    window.open(url, "_blank", "noopener,noreferrer");
  }
};
