/**
 * What a provider shell has to tell the shared UI about itself.
 *
 * One tree builds seven frontends, so everything that differs between them
 * has to arrive as data through this one interface rather than as a
 * conditional on a platform name somewhere in a page. That constraint is
 * enforced, not just intended: contract.conformance.test.ts fails on a
 * platform-name comparison in the shared tree, and on a capability field
 * here that the service's own capability endpoint does not report.
 *
 * So the shape of this file is the shape of that rule. Capabilities are
 * booleans a provider claims, deployment facts are strings a provider
 * supplies, and the two optional bridge methods are optional precisely
 * because a provider that cannot open an external URL or raise a
 * notification should be able to say nothing rather than supply a stub
 * that fails at the moment an operator uses it.
 */
/** The provider shells this tree builds a frontend for. Closed, because
 *  it doubles as the node-id prefix every provider-specific graph node has
 *  to carry (see state/graph.ts): a collision on that shared singleton
 *  crashes the app at import time, and a namespace drawn from a fixed list
 *  makes the two colliding providers readable from the id alone. */
export type PlatformId =
  | "generic"
  | "ugos"
  | "synology"
  | "truenas"
  | "unraid"
  | "openmediavault"
  | "proxmox";

/** How the app reaches its operator on this platform. Kept apart from
 *  capabilities on purpose: two providers can share an integration kind
 *  and still differ in what they support, so a page that branched on this
 *  would be guessing at the flags instead of reading them. */
export type IntegrationKind =
  | "standalone"
  | "native"
  | "embedded-web"
  | "container";

/** Which identity the operator is signed in as. The distinction reaches
 *  the UI because the two have different recovery stories: a local account
 *  has a password this product can rotate, and a native session does not. */
export type AuthMode = "local-account" | "native-session";

/** Everything a provider may claim. The list is closed and is pinned from
 *  both ends: adding a field here fails the contract conformance test
 *  until GET /system/capabilities reports it too, which is what stops this
 *  interface and the service's own capability model from drifting into two
 *  answers. */
export interface PlatformCapabilities {
  nativeAuth: boolean;
  nativeNotifications: boolean;
  storagePicker: boolean;
  embeddedWindow: boolean;
  appStorePackaging: boolean;
}

/** Who is signed in, as the bridge sees it. `username` is nullable
 *  independently of `authenticated` because a native session can be valid
 *  while the shell declines to hand over a name, and a surface that
 *  assumed one from the other would either greet nobody or greet a null. */
export interface AuthContext {
  authenticated: boolean;
  username: string | null;
  mode: AuthMode;
  /** Present only for native-session providers. */
  sessionExpiresAt?: string;
}

/** The facts about THIS deployment that the UI shows verbatim or seeds a
 *  form from. All strings supplied by the shell: the shared tree cannot
 *  derive any of them without knowing which platform it is on, which is
 *  the conditional this whole interface exists to remove. */
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
   *  distribution/packaging/canonical.json, which is where their value is
   *  changed; bridges that file does not cover are not yet pinned to it. */
  storageMount: string;
  adapterVersion: string;
}

/**
 * The one object a provider shell hands the shared UI at bootstrap.
 *
 * Everything platform-specific arrives through here, which is why nothing
 * in `ui/shared` compares against a platform name and a test enforces
 * that. A page asks the bridge what is possible; it never asks who it is
 * talking to.
 *
 * The last two methods are optional rather than required-with-a-stub. A
 * provider that cannot open an external URL should be undefined here so a
 * caller can decide not to offer the control at all, whereas a stub that
 * throws or silently does nothing turns the same fact into a dead button
 * an operator presses twice.
 */
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
