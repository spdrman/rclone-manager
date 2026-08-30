import { useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes, useNavigate } from "react-router-dom";
import "@shared/design-system/tokens.css";
import "@shared/design-system/typography.css";
import "@shared/design-system/components.css";
import { useApi } from "@shared/api/ApiContext";
import { usePlatform } from "@shared/platform/PlatformContext";
import { usePolling } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { useResource } from "@shared/state/resource";
import { countsNode, healthNode, operationsNode, quarantineNode, readOnlyNode, setsNode, versionNode } from "@shared/state/appNodes";
import { AppShell } from "@shared/layouts/AppShell";
import { WarningBanner } from "@shared/components/WarningBanner";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { BackupSetsPage } from "@shared/pages/BackupSetsPage";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";
import { BackupsPage } from "@shared/pages/BackupsPage";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { ActivityPage } from "@shared/pages/ActivityPage";
import { QuarantinePage } from "@shared/pages/QuarantinePage";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { CatalogRecoveryPage } from "@shared/pages/CatalogRecoveryPage";
import { LoginPage } from "@shared/auth/LoginPage";
import { EnrollmentPage } from "@shared/auth/EnrollmentPage";

const THEME_KEY = "backup-manager.theme";

export function App() {
  const api = useApi();
  const { auth, authLoading, refreshAuth, bridge } = usePlatform();
  const navigate = useNavigate();

  const [theme, setTheme] = useState<"light" | "dark">(() => {
    const stored = window.localStorage.getItem(THEME_KEY);
    return stored === "dark" ? "dark" : "light";
  });

  useEffect(() => {
    document.documentElement.setAttribute("data-theme", theme);
    window.localStorage.setItem(THEME_KEY, theme);
  }, [theme]);

  const health = useResource(healthNode, () => api.getHealth(), [api]);
  const version = useResource(versionNode, () => api.getVersion(), [api]);
  const sets = useResource(setsNode, () => api.listSets(), [api]);
  // Fetched into the graph purely for the header's quarantine count
  // (countsNode, below). QuarantinePage does NOT read quarantineNode — it
  // runs its own separate, uncoordinated api.listQuarantine() fetch via
  // useAsync (pre-existing duplication, not new to this migration), so the
  // two can disagree with each other in principle. Rewiring the page onto
  // this node is arguably B2.5's scope; see #101.
  useResource(quarantineNode, () => api.listQuarantine(), [api]);
  // B2.1 (#95) — the one fetch of operationsNode. DashboardPage and
  // BackupSetsPage both read the node directly (useCausl), never their own
  // listOperations() call, so a poll tick here is the only thing that can
  // move either page's live operation progress.
  const operations = useResource(operationsNode, () => api.listOperations(), [api]);

  const reloadAll = useCallback(() => {
    health.reload();
    sets.reload();
    operations.reload();
  }, [health, sets, operations]);

  usePolling(30_000, reloadAll, auth?.authenticated ?? false);

  // counts and readOnly are derived() nodes (state/appNodes.ts): pure
  // functions of the four resources above, recomputed by the graph rather
  // than by a useMemo keyed on their .data references.
  const counts = useCausl(countsNode);
  const readOnly = useCausl(readOnlyNode);

  if (authLoading) return <Splash />;

  if (!auth?.authenticated) {
    return (
      <Routes>
        <Route path="/enroll" element={<EnrollmentPage onEnrolled={refreshAuth} />} />
        <Route path="*" element={<LoginPage onSignedIn={refreshAuth} />} />
      </Routes>
    );
  }

  return (
    <AppShell
      health={health.data}
      version={version.data}
      counts={counts}
      theme={theme}
      onToggleTheme={() => setTheme(theme === "light" ? "dark" : "light")}
      onSignOut={() => api.logout().then(refreshAuth)}
    >
      {readOnly && version.data ? (
        <WarningBanner
          tone="warn"
          title="Backup Manager update required"
          eyebrow="Version mismatch"
        >
          {"The user interface and backup service versions do not match. Management " +
            "actions have been disabled to prevent unsafe changes. UI " +
            version.data.ui + " \u00b7 Service " + version.data.service + "."}
        </WarningBanner>
      ) : null}

      <Routes>
        <Route
          path="/"
          element={<DashboardPage health={health} sets={sets} readOnly={readOnly} />}
        />
        <Route path="/sets" element={<BackupSetsPage sets={sets} readOnly={readOnly} />} />
        <Route path="/sets/new" element={<BackupSetWizardPage readOnly={readOnly} />} />
        <Route path="/sets/:setId" element={<BackupSetDetailPage readOnly={readOnly} />} />
        <Route path="/backups" element={<BackupsPage readOnly={readOnly} />} />
        <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
        <Route path="/activity" element={<ActivityPage />} />
        <Route path="/quarantine" element={<QuarantinePage readOnly={readOnly} />} />
        <Route path="/settings" element={<SettingsPage readOnly={readOnly} />} />
        <Route path="/catalog-recovery" element={<CatalogRecoveryPage readOnly={readOnly} />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>

      <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        {"Backup Manager running on " + bridge.name}
        {" \u00b7 "}
        <button
          onClick={() => navigate("/catalog-recovery")}
          className="btn btn--quiet"
          style={{ height: "auto", padding: 0, border: "none", background: "none", color: "var(--accent)", fontSize: "var(--text-sm)" }}
        >
          Catalog recovery
        </button>
      </p>
    </AppShell>
  );
}

function Splash() {
  return (
    <div style={{ minHeight: "100vh", display: "grid", placeItems: "center", color: "var(--text-3)" }}>
      <span className="mono" style={{ fontSize: "var(--text-sm)" }}>Connecting to backup service…</span>
    </div>
  );
}
