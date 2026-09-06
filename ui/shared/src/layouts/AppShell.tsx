/**
 * The frame every signed-in page renders inside: header, section nav, and
 * the content column.
 *
 * The shell is deliberately thin on decisions and carries only two. The
 * titlebar strip appears for embedded providers alone, because drawing
 * host chrome on a platform that does not have any would be inventing an
 * affordance the operator's window manager will not honour. And the nav
 * counts arrive as props rather than being read from the graph here, so
 * this file stays renderable in a test with no graph behind it, which is
 * what several of the page suites rely on.
 *
 * Everything else is layout. If a change here needs a comment about what
 * it does, it probably belongs in a page instead.
 */
import type { ReactNode } from "react";
import { NavLink } from "react-router-dom";
import { Logo, Wordmark } from "@shared/components/Logo";
import { PlatformBadge } from "@shared/components/PlatformBadge";
import { StatusBadge } from "@shared/components/StatusBadge";
import { usePlatform } from "@shared/platform/PlatformContext";
import type { SystemHealth, VersionInfo } from "@shared/types/operation";

export interface NavCounts {
  sets?: number;
  backups?: number;
  quarantine?: number;
}

const NAV = [
  { to: "/", label: "Dashboard", glyph: "\u25c7", end: true },
  { to: "/sets", label: "Backup sets", glyph: "\u25a4", count: "sets" as const },
  { to: "/backups", label: "Backups", glyph: "\u25a5", count: "backups" as const },
  { to: "/activity", label: "Activity", glyph: "\u2261" },
  { to: "/quarantine", label: "Quarantine", glyph: "\u2298", count: "quarantine" as const, alert: true },
  { to: "/settings", label: "Settings", glyph: "\u2699" }
];

export function AppShell({
  health,
  version,
  counts,
  theme,
  onToggleTheme,
  onSignOut,
  children
}: {
  health: SystemHealth | null;
  version: VersionInfo | null;
  counts: NavCounts;
  theme: "light" | "dark";
  onToggleTheme(): void;
  onSignOut(): void;
  children: ReactNode;
}) {
  const { bridge } = usePlatform();
  const embedded = bridge.capabilities().embeddedWindow;

  return (
    <div style={{ minHeight: "100vh", display: "flex", flexDirection: "column", background: "var(--bg)" }}>
      {/* Only embedded providers draw a titlebar. Everyone else gets nothing —
          we do not fake host chrome (§46). */}
      {embedded ? (
        <div
          style={{
            height: 30, flex: "none", display: "flex", alignItems: "center", gap: 8,
            padding: "0 12px", background: "var(--platform-titlebar, var(--surface-3))",
            borderBottom: "1px solid var(--border)", fontSize: "var(--text-sm)",
            color: "var(--text-2)", fontFamily: "var(--font-mono)"
          }}
        >
          {"Backup Manager \u2014 " + bridge.name}
        </div>
      ) : null}

      <header
        style={{
          flex: "none", height: 52, display: "flex", alignItems: "center", gap: 16,
          padding: "0 18px", background: "var(--surface)", borderBottom: "1px solid var(--border)"
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 10, minWidth: 206 }}>
          <Logo size={24} title="Backup Manager" />
          <Wordmark size={14} />
        </div>

        {health ? (
          <StatusBadge tone={health.serviceRunning ? "ok" : "danger"} glyph={health.serviceRunning ? "\u25cf" : "\u2715"}>
            {(health.serviceRunning ? "Service running" : "Service stopped") +
              (version ? " \u00b7 v" + version.service : "")}
          </StatusBadge>
        ) : null}

        <div style={{ flex: 1 }} />

        <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
          <button className="btn btn--sm" onClick={onToggleTheme} aria-label="Toggle colour theme">
            {theme === "light" ? "Dark" : "Light"}
          </button>
          <button className="btn btn--sm btn--quiet" onClick={onSignOut}>Sign out</button>
        </div>
      </header>

      <div style={{ flex: 1, display: "flex", alignItems: "stretch", minHeight: 0 }}>
        <nav
          aria-label="Sections"
          style={{
            flex: "none", width: 206, padding: "14px 10px", background: "var(--surface)",
            borderRight: "1px solid var(--border)", display: "flex", flexDirection: "column", gap: 2
          }}
        >
          {NAV.map((item) => {
            const count = item.count ? counts[item.count] : undefined;
            return (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                style={({ isActive }) => ({
                  display: "flex", alignItems: "center", gap: 9, height: 33,
                  padding: "0 10px", borderRadius: 7, textDecoration: "none",
                  fontSize: 13.5,
                  background: isActive ? "var(--accent-quiet)" : "transparent",
                  color: isActive ? "var(--text)" : "var(--text-2)",
                  fontWeight: isActive ? 600 : 400
                })}
              >
                <span
                  aria-hidden="true"
                  className="mono"
                  style={{ fontSize: "var(--text-xs)", width: 13, opacity: 0.7 }}
                >
                  {item.glyph}
                </span>
                <span style={{ flex: 1 }}>{item.label}</span>
                {count ? (
                  <span
                    className="mono"
                    style={{
                      fontSize: "var(--text-xs)", padding: "1px 6px",
                      borderRadius: "var(--radius-pill)",
                      background: item.alert ? "var(--warn-quiet)" : undefined,
                      border: item.alert ? "1px solid var(--warn)" : undefined,
                      color: item.alert ? "var(--text)" : "var(--text-3)"
                    }}
                  >
                    {count}
                  </span>
                ) : null}
              </NavLink>
            );
          })}

          <div style={{ flex: 1, minHeight: 20 }} />
          <div style={{ padding: "12px 10px", borderTop: "1px solid var(--border)" }}>
            <PlatformBadge compact />
          </div>
        </nav>

        <main style={{ flex: 1, minWidth: 0, overflow: "auto" }}>
          <div
            style={{
              maxWidth: 1240, margin: "0 auto", padding: "24px 28px 64px",
              display: "flex", flexDirection: "column", gap: 18
            }}
          >
            {children}
          </div>
        </main>
      </div>
    </div>
  );
}
