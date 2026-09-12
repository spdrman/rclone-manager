import { capabilities } from "@shared/platform/capabilities";
import { readLocalAccountSession } from "@shared/platform/localSession";
import type { PlatformBridge } from "@shared/types/platform";

/** The visual baseline (§23). No NAS-provider branding, no host chrome,
 *  Backupd local authentication. Every other provider is a delta on this. */
export const genericBridge: PlatformBridge = {
  id: "generic",
  name: "Generic Docker / Linux",
  integration: "standalone",

  deployment: {
    label: "Docker Compose",
    storageMount: "/data/backups",
    adapterVersion: "generic 1.3.0"
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
