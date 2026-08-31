import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** V1 is a Compose integration, not a native Workbench plugin. A future
 *  native OMV shell can replace THIS FILE ONLY, with no shared-page changes (§28). */
export const openmediavaultBridge: PlatformBridge = {
  id: "openmediavault",
  name: "OpenMediaVault",
  integration: "container",

  deployment: {
    label: "omv-compose",
    // A dedicated directory inside the backups directory, not the
    // directory itself. Pinned to apps/common/packaging/canonical.json.
    storageMount: "/srv/dev-disk-by-uuid/backups/backup-manager",
    adapterVersion: "omv 1.1.0"
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
