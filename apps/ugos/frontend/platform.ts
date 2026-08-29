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
    deployment: "UGOS package",
    storageMount: "/volume1/backup-manager",
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
    // Bridged to the host notification centre. If the bridge is missing we stay
    // silent rather than pretending the notification was delivered.
    const host = (window as unknown as { ugos?: { notify?(t: string, m: string): Promise<void> } }).ugos;
    if (host?.notify) await host.notify(title, message);
  }
};
