import { capabilities } from "@shared/platform/capabilities";
import { readLocalAccountSession } from "@shared/platform/localSession";
import type { PlatformBridge } from "@shared/types/platform";

/** DSM ships as a package that opens in a desktop window. Native DSM
 *  authentication is NOT implemented, so this behaves as a normal embedded web
 *  app with its own secure login — we do not fabricate a DSM auth API (§25). */
export const synologyBridge: PlatformBridge = {
  id: "synology",
  name: "Synology DSM",
  integration: "embedded-web",

  deployment: {
    label: "DSM package",
    storageMount: "/volume1/backupd",
    adapterVersion: "synology 1.2.4"
  },

  capabilities: () => capabilities({ embeddedWindow: true, appStorePackaging: true }),

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
