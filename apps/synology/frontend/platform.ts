import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** DSM ships as a package that opens in a desktop window. Native DSM
 *  authentication is NOT implemented, so this behaves as a normal embedded web
 *  app with its own secure login — we do not fabricate a DSM auth API (§25). */
export const synologyBridge: PlatformBridge = {
  id: "synology",
  name: "Synology DSM",
  integration: "embedded-web",

  deployment: {
    deployment: "DSM package",
    storageMount: "/volume1/backup-manager",
    adapterVersion: "synology 1.2.4"
  },

  capabilities: () => capabilities({ embeddedWindow: true, appStorePackaging: true }),

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
