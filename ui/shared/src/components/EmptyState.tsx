import type { ReactNode } from "react";

export function EmptyState({
  title,
  children,
  action
}: {
  title: string;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div
      style={{
        padding: "34px 22px", textAlign: "center",
        border: "1px dashed var(--border-strong)", borderRadius: "var(--radius-xl)",
        background: "var(--surface-2)"
      }}
    >
      <div style={{ fontSize: 15, fontWeight: 600 }}>{title}</div>
      {children ? (
        <p
          style={{
            margin: "6px auto 14px", maxWidth: "46ch",
            fontSize: 13, color: "var(--text-2)"
          }}
        >
          {children}
        </p>
      ) : null}
      {action}
    </div>
  );
}

export function ErrorState({
  message,
  remediation,
  correlationId,
  onRetry
}: {
  message: string;
  remediation?: string;
  correlationId: string;
  onRetry?(): void;
}) {
  return (
    <div className="banner banner--danger" role="alert" style={{ flexDirection: "column" }}>
      <div style={{ display: "flex", gap: 12 }}>
        <span aria-hidden="true" style={{ color: "var(--danger)" }}>\u2715</span>
        <div>
          <div style={{ fontWeight: 600, fontSize: 13.5 }}>{message}</div>
          {remediation ? (
            <div style={{ marginTop: 4, fontSize: 13, color: "var(--text-2)" }}>{remediation}</div>
          ) : null}
          {/* Advanced details only — never a stack trace (§37). */}
          <details style={{ marginTop: 8, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
            <summary style={{ cursor: "pointer" }}>Advanced details</summary>
            <div className="mono" style={{ marginTop: 6 }}>
              {"correlation id " + correlationId}
            </div>
          </details>
          {onRetry ? (
            <button className="btn btn--sm" style={{ marginTop: 10 }} onClick={onRetry}>
              Try again
            </button>
          ) : null}
        </div>
      </div>
    </div>
  );
}
