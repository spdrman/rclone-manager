import { usePlatform } from "@shared/platform/PlatformContext";

const INTEGRATION_LABEL: Record<string, string> = {
  standalone: "Standalone web app",
  native: "Native app",
  "embedded-web": "Embedded web app",
  container: "Container app"
};

/** Makes the abstraction explicit to administrators (§21). Product identity is
 *  Backup Manager; the platform is context, never the brand. */
export function PlatformBadge({ compact = false }: { compact?: boolean }) {
  const { bridge, auth } = usePlatform();
  const authLabel =
    auth?.mode === "native-session"
      ? bridge.name + " session"
      : "Backup Manager local account";

  if (compact)
    return (
      <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
        <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>Running on</div>
        <div style={{ fontSize: 13, fontWeight: 500 }}>{bridge.name}</div>
        <div style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {INTEGRATION_LABEL[bridge.integration] + " \u00b7 " + authLabel}
        </div>
      </div>
    );

  return (
    <dl
      style={{
        margin: 0, display: "grid", gridTemplateColumns: "128px 1fr",
        gap: "11px 14px", fontSize: 13
      }}
    >
      <dt style={{ color: "var(--text-2)" }}>Platform</dt>
      <dd style={{ margin: 0, fontWeight: 500 }}>{bridge.name}</dd>
      <dt style={{ color: "var(--text-2)" }}>Integration</dt>
      <dd style={{ margin: 0 }}>{INTEGRATION_LABEL[bridge.integration]}</dd>
      <dt style={{ color: "var(--text-2)" }}>Authentication</dt>
      <dd style={{ margin: 0 }}>{authLabel}</dd>
      <dt style={{ color: "var(--text-2)" }}>Deployment</dt>
      <dd className="mono" style={{ margin: 0, fontSize: "var(--text-sm)" }}>
        {bridge.deployment.label}
      </dd>
      <dt style={{ color: "var(--text-2)" }}>Storage mount</dt>
      <dd className="mono" style={{ margin: 0, fontSize: "var(--text-sm)" }}>
        {bridge.deployment.storageMount}
      </dd>
    </dl>
  );
}
