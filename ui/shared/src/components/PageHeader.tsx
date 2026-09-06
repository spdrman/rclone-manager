/**
 * The title block every page opens with, including its way back and its
 * page-level actions.
 *
 * Actions belong up here rather than beside the thing they act on
 * whenever their scope is the whole page, which is a distinction this
 * product has already got wrong once: a control's placement says what it
 * acts on, louder than its label does, so a deployment-wide pass drawn
 * inside a per-set card reads as a per-set run (see BackupSetCard's own
 * note). Anything handed to `actions` here is claiming page scope.
 *
 * `title` and `subtitle` take nodes rather than strings so a page can put
 * an identity in mono or a badge beside the name without this component
 * growing a prop per case.
 */
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
