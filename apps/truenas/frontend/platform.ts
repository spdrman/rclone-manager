import { capabilities } from "@shared/platform/capabilities";
import { readLocalAccountSession } from "@shared/platform/localSession";
import type { PlatformBridge } from "@shared/types/platform";

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
    // distribution/packaging/canonical.json.
    storageMount: "/mnt/tank/backupd/backups",
    adapterVersion: "truenas 1.2.0"
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
