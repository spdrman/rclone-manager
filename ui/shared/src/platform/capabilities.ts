/**
 * What a platform integration can and cannot do, and the honest sentence
 * the Settings page shows for each answer.
 *
 * The default is that everything is unsupported and a provider opts in.
 * That direction is the point: a capability added here later is absent on
 * every existing provider until each one claims it, whereas a permissive
 * default would silently claim it for all of them the day it lands.
 *
 * The copy lives beside the flags rather than in the page, because the
 * unsupported branch is where the honesty rule actually bites. Every entry
 * names what happens INSTEAD, so a "no" is a description of the fallback
 * the operator gets rather than a missing feature they are left to guess
 * about.
 */
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

/** Builds a provider's capability set from the few flags it actually
 *  turns on. Spelled this way round so a capability added to the interface
 *  arrives as false everywhere, and each provider has to make a deliberate
 *  claim before anything is offered on its behalf. */
export function capabilities(
  overrides: Partial<PlatformCapabilities>
): PlatformCapabilities {
  return { ...NO_CAPABILITIES, ...overrides };
}

/** One row of the Settings page's capability list: the label, the answer,
 *  and the sentence that goes with THAT answer. The detail is not a
 *  description of the capability, it is a description of what this
 *  deployment gets, which is why it differs between the two branches. */
export interface CapabilityCopy {
  label: string;
  supported: boolean;
  /** Honest fallback sentence shown when unsupported. */
  detail: string;
}

/**
 * Turns the flags into the rows the Settings page draws.
 *
 * Every unsupported branch names a working alternative rather than an
 * absence, which is the whole reason this lives beside the flags instead
 * of in the page. "Native notifications: no" invites an operator to go
 * looking for a setting that will not exist; "webhook notifications"
 * tells them where the feature actually is.
 *
 * The platform's own name is threaded in so the supported branches can say
 * whose session or whose notification centre is in play. A generic
 * sentence would be true and useless on the one integration where the
 * distinction matters.
 */
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
