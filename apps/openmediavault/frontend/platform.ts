import { capabilities } from "@shared/platform/capabilities";
import { readLocalAccountSession } from "@shared/platform/localSession";
import type { PlatformBridge } from "@shared/types/platform";

/** V1 is a Compose integration, not a native Workbench plugin. A future
 *  native OMV shell can replace THIS FILE ONLY, with no shared-page changes (§28). */
export const openmediavaultBridge: PlatformBridge = {
  id: "openmediavault",
  name: "OpenMediaVault",
  integration: "container",

  deployment: {
    label: "omv-compose",
    // A dedicated directory inside the backups directory, not the
    // directory itself. Pinned to distribution/packaging/canonical.json.
    storageMount: "/srv/dev-disk-by-uuid/backups/backupd",
    adapterVersion: "omv 1.1.0"
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
