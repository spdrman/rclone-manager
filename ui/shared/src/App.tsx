import { useCallback, useEffect, useMemo, useState } from "react";
import { Navigate, Route, Routes, useNavigate } from "react-router-dom";
import "@shared/design-system/tokens.css";
import "@shared/design-system/typography.css";
import "@shared/design-system/components.css";
import { useApi } from "@shared/api/ApiContext";
import { usePlatform } from "@shared/platform/PlatformContext";
import { useAsync, usePolling } from "@shared/hooks/useAsync";
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

  const health = useAsync(() => api.getHealth(), [api]);
  const version = useAsync(() => api.getVersion(), [api]);
  const sets = useAsync(() => api.listSets(), [api]);
  const quarantine = useAsync(() => api.listQuarantine(), [api]);

  const reloadAll = useCallback(() => {
    health.reload();
    sets.reload();
  }, [health, sets]);

  usePolling(30_000, reloadAll, auth?.authenticated ?? false);

  const counts = useMemo(
    () => ({
      sets: sets.data?.length,
      backups: health.data?.retainedCount,
      quarantine: quarantine.data?.length
    }),
    [sets.data, health.data, quarantine.data]
  );

  // §38 — an incompatible service disables every management action but leaves
  // read-only information visible.
  const readOnly = version.data ? !version.data.compatible : false;

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
