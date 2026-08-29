import type { ReactNode } from "react";

export function PageHeader({
  title,
  subtitle,
  back,
  actions
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  back?: { label: string; onClick(): void };
  actions?: ReactNode;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {back ? (
        <button
          className="btn btn--quiet"
          onClick={back.onClick}
          style={{
            alignSelf: "flex-start", height: "auto", padding: 0, border: "none",
            background: "none", color: "var(--accent)", fontSize: "var(--text-sm)"
          }}
        >
          {"\u2190 " + back.label}
        </button>
      ) : null}
      <div
        style={{
          display: "flex", alignItems: "flex-end", justifyContent: "space-between",
          gap: 16, flexWrap: "wrap"
        }}
      >
        <div>
          <h1>{title}</h1>
          {subtitle ? (
            <p style={{ margin: "4px 0 0", color: "var(--text-2)", fontSize: 13 }}>{subtitle}</p>
          ) : null}
        </div>
        {actions ? (
          <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>{actions}</div>
        ) : null}
      </div>
    </div>
  );
}
