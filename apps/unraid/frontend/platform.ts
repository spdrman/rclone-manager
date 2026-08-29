import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";

/** Community Applications container. Provider identity, WebUI launch
 *  metadata and storage expectations only — there is no separate Unraid UI (§27). */
export const unraidBridge: PlatformBridge = {
  id: "unraid",
  name: "Unraid",
  integration: "container",

  deployment: {
    label: "Community Applications",
    storageMount: "/mnt/user/backups",
    adapterVersion: "unraid 1.1.0"
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
