import type { PlatformCapabilities } from "@shared/types/platform";

/** Least-privilege default: a provider must opt IN to every capability.
 *  Never present an unsupported capability as supported (§22, §45). */
export const NO_CAPABILITIES: PlatformCapabilities = {
  nativeAuth: false,
  nativeNotifications: false,
  storagePicker: false,
  embeddedWindow: false,
  appStorePackaging: false
};

export function capabilities(
  overrides: Partial<PlatformCapabilities>
): PlatformCapabilities {
  return { ...NO_CAPABILITIES, ...overrides };
}

export interface CapabilityCopy {
  label: string;
  supported: boolean;
  /** Honest fallback sentence shown when unsupported. */
  detail: string;
}

export function describeCapabilities(
  caps: PlatformCapabilities,
  platformName: string
): CapabilityCopy[] {
  return [
    {
      label: "Native authentication",
      supported: caps.nativeAuth,
      detail: caps.nativeAuth
        ? platformName + " session"
        : "Backup Manager local account"
    },
    {
      label: "Native notifications",
      supported: caps.nativeNotifications,
      detail: caps.nativeNotifications
        ? platformName + " notification centre"
        : "Webhook notifications"
    },
    {
      label: "Storage picker",
      supported: caps.storagePicker,
      detail: caps.storagePicker ? "Native volume browser" : "Manual path entry"
    },
    {
      label: "Embedded window",
      supported: caps.embeddedWindow,
      detail: caps.embeddedWindow ? "Managed window chrome" : "Standalone browser"
    },
    {
      label: "App-store packaging",
      supported: caps.appStorePackaging,
      detail: caps.appStorePackaging ? "Packaged app" : "Container deployment"
    }
  ];
}

/** Copy for the Notifications setting. Do not fake a capability (§22). */
export function notificationCopy(
  caps: PlatformCapabilities,
  platformName: string
): string {
  return caps.nativeNotifications
    ? "Native " + platformName + " notifications are available and enabled for this platform integration."
    : "Native NAS notifications are not available for this platform integration. Backup Manager webhook notifications are enabled instead.";
}
