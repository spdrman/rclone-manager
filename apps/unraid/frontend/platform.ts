import { capabilities } from "@shared/platform/capabilities";
import { readLocalAccountSession } from "@shared/platform/localSession";
import type { PlatformBridge } from "@shared/types/platform";

/** Community Applications container. Provider identity, WebUI launch
 *  metadata and storage expectations only — there is no separate Unraid UI (§27). */
export const unraidBridge: PlatformBridge = {
  id: "unraid",
  name: "Unraid",
  integration: "container",

  deployment: {
    label: "Community Applications",
    // A dedicated directory inside the backups share, not the share
    // itself: the share is very likely one the operator already uses.
    // Pinned to distribution/packaging/canonical.json.
    storageMount: "/mnt/user/backups/backupd",
    adapterVersion: "unraid 1.2.0"
  },

  capabilities: () => capabilities({ appStorePackaging: true }),

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
