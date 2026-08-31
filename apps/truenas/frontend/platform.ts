import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** Canonical container deployment. No fake TrueNAS-native app framework —
 *  this is a thin platform configuration over the shared Web UI (§26). */
export const truenasBridge: PlatformBridge = {
  id: "truenas",
  name: "TrueNAS",
  integration: "container",

  deployment: {
    label: "TrueNAS app (container)",
    // The backup root, not the app root: the shared UI shows this string
    // and the backup-set wizard seeds a destination from it, so it must not
    // name a directory that also holds the SSH key (§19.2). Pinned to
    // apps/common/packaging/canonical.json.
    storageMount: "/mnt/tank/backup-manager/backups",
    adapterVersion: "truenas 1.2.0"
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
