import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** The visual baseline (§23). No NAS-provider branding, no host chrome,
 *  Backup Manager local authentication. Every other provider is a delta on this. */
export const genericBridge: PlatformBridge = {
  id: "generic",
  name: "Generic Docker / Linux",
  integration: "standalone",

  deployment: {
    deployment: "Docker Compose",
    storageMount: "/data/backups",
    adapterVersion: "generic 1.3.0"
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
