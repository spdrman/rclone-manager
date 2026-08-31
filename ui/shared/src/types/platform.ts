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
  /** The host directory this integration lands retained backup artifacts
   *  in: the backup root, not the app root and not a container path. The
   *  UI shows it, and the backup-set wizard seeds a destination from it, so
   *  a value that also holds private state, config or key material both
   *  misinforms the operator and proposes writing backups next to a private
   *  key (§19.2). The container-packaged platforms are pinned to
   *  apps/common/packaging/canonical.json, which is where their value is
   *  changed; bridges that file does not cover are not yet pinned to it. */
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
