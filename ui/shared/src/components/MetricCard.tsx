export function MetricCard({
  label,
  value,
  detail,
  children
}: {
  label: string;
  value: string;
  detail?: string;
  children?: React.ReactNode;
}) {
  return (
    <div style={{ padding: "15px 20px", borderRight: "1px solid var(--border)" }}>
      <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>{label}</div>
      <div style={{ marginTop: 6, fontSize: 24, fontWeight: 600, letterSpacing: "-0.02em" }}>
        {value}
      </div>
      {detail ? (
        <div style={{ marginTop: 4, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {detail}
        </div>
      ) : null}
      {children}
    </div>
  );
}
