import { capabilities } from "@shared/platform/capabilities";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { getUgosSession } from "./auth";

/** The only provider with a native session and native notifications today.
 *  UGOS already supplies system navigation, so we draw a titlebar and let the
 *  shared shell own the product navigation — we do not duplicate UGOS chrome (§24). */
export const ugosBridge: PlatformBridge = {
  id: "ugos",
  name: "UGREEN UGOS Pro",
  integration: "native",

  deployment: {
    // The backup root, and it moved here when the UPK was built (#83). It
    // used to name /volume1/backup-manager, a top-level share that does
    // not exist on a stock UGOS Pro NAS and that only the UGOS UI can
    // create; /volume1/backups does exist, checked on a real device, and
    // the app takes a dedicated child of it so an uninstall offering to
    // remove the app's data cannot reach a share the operator already
    // uses. Pinned to canonical.json's ugos entry, which the packaged
    // Compose wrapper derives its host paths from, so the string an
    // operator reads here is the directory the container actually writes.
    label: "UGOS package",
    storageMount: "/volume1/backups/backup-manager",
    adapterVersion: "ugos 1.3.0"
  },

  capabilities: () => capabilities({ nativeAuth: true, nativeNotifications: true, storagePicker: true, embeddedWindow: true, appStorePackaging: true }),

  async getAuthContext(): Promise<AuthContext> {
    // Native session: UGOS owns the identity, we only read it.
    const session = await getUgosSession();
    return {
      authenticated: session !== null,
      username: session?.username ?? null,
      mode: "native-session",
      sessionExpiresAt: session?.expiresAt
    };
  },

  async openExternal(url: string) {
    window.open(url, "_blank", "noopener,noreferrer");
  },

  async notify(title: string, message: string) {
    // Bridged to the host notification centre. Declaring nativeNotifications is
    // a promise that a notification actually goes out, so a missing bridge
    // rejects: resolving is how a caller reads "delivered", and silently
    // resolving here is the §22 emulation the Go half refuses at wiring time
    // (apps/common/platform/notify.NewPlatformSink turns an undeclared
    // capability into a typed refusal rather than a sink that drops alerts).
    // A caller that fires and forgets has to catch, which is the point.
    const host = (window as unknown as { ugos?: { notify?(t: string, m: string): Promise<void> } }).ugos;
    if (!host?.notify) {
      throw new Error("ugos: window.ugos.notify is unavailable, so the notification was not delivered");
    }
    await host.notify(title, message);
  }
};
