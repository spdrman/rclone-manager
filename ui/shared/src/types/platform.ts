export type PlatformId =
  | "generic"
  | "ugos"
  | "synology"
  | "truenas"
  | "unraid"
  | "openmediavault"
  | "proxmox";

export type IntegrationKind =
  | "standalone"
  | "native"
  | "embedded-web"
  | "container";

export type AuthMode = "local-account" | "native-session";

export interface PlatformCapabilities {
  nativeAuth: boolean;
  nativeNotifications: boolean;
  storagePicker: boolean;
  embeddedWindow: boolean;
  appStorePackaging: boolean;
}

export interface AuthContext {
  authenticated: boolean;
  username: string | null;
  mode: AuthMode;
  /** Present only for native-session providers. */
  sessionExpiresAt?: string;
}

export interface PlatformDeploymentInfo {
  /** e.g. "Unprivileged LXC", "DSM package". Named `label`, not
   *  `deployment`, so a consumer reads `bridge.deployment.label` rather
   *  than the doubled-up `bridge.deployment.deployment`. */
  label: string;
  /** Documented storage mount for this integration. */
  storageMount: string;
  adapterVersion: string;
}

export interface PlatformBridge {
  id: PlatformId;
  name: string;
  integration: IntegrationKind;
  deployment: PlatformDeploymentInfo;
  capabilities(): PlatformCapabilities;
  getAuthContext(): Promise<AuthContext>;
  openExternal?(url: string): Promise<void>;
  notify?(title: string, message: string): Promise<void>;
}
