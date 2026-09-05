/**
 * The routed application, and the four questions it answers before a page
 * is allowed to render.
 *
 * Read top to bottom, the component is a sequence of gates and the order
 * is the interesting part. Is the auth check still running, is anyone
 * signed in, do we yet know whether this instance is configured, and is
 * the configuration one that this process cannot serve. Each of the first
 * three returns something other than the shell, and the fourth returns a
 * screen with no navigation at all, because it is the single state where
 * navigating anywhere is a lie: the configuration is on disk, this process
 * is not running it, and only a restart changes that.
 *
 * This is also the one fetch owner for everything app-wide. Health,
 * version, sets, quarantine and operations are all fetched here and read
 * from the graph elsewhere, so a page cannot go and ask again and get a
 * different answer from the panel next to it. The polling loop is the
 * corollary, and it stays switched off until the instance is known to be
 * configured, since every one of those five calls refuses on a fresh
 * install and a refusal every thirty seconds is just noise in the log of
 * whoever is mid-setup.
 *
 * Nothing platform-specific appears anywhere below. What varies between
 * the seven builds arrives through the bridge, and the only trace of that
 * here is the footer naming what it is running on.
 */
import { useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes, useNavigate } from "react-router-dom";
import "@shared/design-system/tokens.css";
import "@shared/design-system/typography.css";
import "@shared/design-system/components.css";
import { useApi } from "@shared/api/ApiContext";
import { usePlatform } from "@shared/platform/PlatformContext";
import { usePolling } from "@shared/hooks/useAsync";
import { graph, useCausl } from "@shared/state/graph";
import { useResource } from "@shared/state/resource";
import { configuredNode, countsNode, healthNode, operationsNode, quarantineNode, readOnlyNode, setsNode, versionNode } from "@shared/state/appNodes";
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
import { ConfigurationSavedPage } from "@shared/pages/ConfigurationSavedPage";
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

  // Issue #176: which mode this instance is in, asked before anything
  // else, because on an instance with no configuration at all every
  // resource below refuses with NOT_CONFIGURED. Issue #275 moved it out of
  // this component's own useState and into the graph: more than one
  // component reads it now (see configuredNode's own doc).
  const configured = useCausl(configuredNode);
  const setConfigured = useCallback(
    (value: boolean) => graph.commit("app/first-run-status", (tx) => tx.set(configuredNode, value)),
    []
  );

  // Issue #275: an instance whose configuration was written but could not
  // be activated in place is the one state where the application really
  // cannot be navigated, because the engine is not serving the new
  // configuration yet and a restart is the only thing that helps.
  const [restartRequired, setRestartRequired] = useState(false);

  useEffect(() => {
    if (!auth?.authenticated) return;
    let cancelled = false;
    api
      .getFirstRunStatus()
      .then((status) => {
        if (!cancelled) setConfigured(status.configured);
      })
      .catch(() => {
        // An instance that cannot answer this is far more likely to be an
        // older backend with no such route than an unconfigured one, and
        // sending an operator with a working deployment into a setup flow
        // would be the worse mistake of the two.
        if (!cancelled) setConfigured(true);
      });
    return () => {
      cancelled = true;
    };
  }, [api, auth?.authenticated, setConfigured]);

  const health = useResource(healthNode, () => api.getHealth(), [api]);
  const version = useResource(versionNode, () => api.getVersion(), [api]);
  const sets = useResource(setsNode, () => api.listSets(), [api]);
  // Owns the one fetch of quarantineNode (#101, matching health/sets
  // above): the header's quarantine badge (countsNode, below) and
  // QuarantinePage's own list both read this same node, via the
  // `quarantine` object passed down here, so they cannot disagree about
  // what is currently quarantined.
  const quarantine = useResource(quarantineNode, () => api.listQuarantine(), [api]);
  // B2.1 (#95) — the one fetch of operationsNode. DashboardPage and
  // BackupSetsPage both read the node directly (useCausl), never their own
  // listOperations() call, so a poll tick here is the only thing that can
  // move either page's live operation progress.
  const operations = useResource(operationsNode, () => api.listOperations(), [api]);

  const reloadAll = useCallback(() => {
    health.reload();
    sets.reload();
    operations.reload();
    quarantine.reload();
  }, [health, sets, operations, quarantine]);

  // Polling stays off until this instance is known to be configured:
  // every one of those four calls refuses with NOT_CONFIGURED on a fresh
  // install, and a 30-second loop of refusals is noise in the log of the
  // operator who is mid-setup.
  usePolling(30_000, reloadAll, (auth?.authenticated ?? false) && configured === true);

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

  if (configured === null) return <Splash />;

  // The one state that genuinely is a dead end, and says so rather than
  // offering navigation that cannot work: the configuration is on disk,
  // this process is not serving it, and only a restart changes that.
  if (restartRequired) return <ConfigurationSavedPage />;

  return (
    <AppShell
      health={health.data}
      version={version.data}
      counts={counts}
      theme={theme}
      onToggleTheme={() => setTheme(theme === "light" ? "dark" : "light")}
      onSignOut={() => api.logout().then(refreshAuth)}
    >
      {configured ? null : (
        // Deliberately no button of its own: the two pages that can act on
        // this (the dashboard and the backup-sets list) already offer
        // "Add backup set", and a banner repeating it puts the same
        // primary action on one page twice.
        <WarningBanner
          tone="info"
          eyebrow="First run"
          title="Backup Manager has no configuration yet"
        >
          {"Add your first backup set, under Backup sets, and Backup Manager writes its " +
            "configuration for you. Until that is done nothing is backed up, and the " +
            "pages here have nothing behind them to show."}
        </WarningBanner>
      )}

      {readOnly && version.data ? (
        <WarningBanner
          tone="warn"
          title="Backup Manager update required"
          eyebrow="Version mismatch"
        >
          {"This interface was built for a different version of the /api/v1 " +
            "contract than the backup service speaks, so management actions " +
            "have been disabled to prevent unsafe changes. Service " +
            version.data.service + " speaks contract " + version.data.api + "."}
        </WarningBanner>
      ) : null}

      <Routes>
        <Route
          path="/"
          element={<DashboardPage health={health} sets={sets} readOnly={readOnly} />}
        />
        <Route path="/sets" element={<BackupSetsPage sets={sets} readOnly={readOnly} />} />
        <Route
          path="/sets/new"
          element={
            <BackupSetWizardPage
              readOnly={readOnly}
              firstRun={!configured}
              onFirstRunComplete={(needsRestart) => {
                if (needsRestart) {
                  setRestartRequired(true);
                  return;
                }
                setConfigured(true);
                reloadAll();
                // The same place the configured path lands after a save,
                // by the same route, which is the whole point of #275:
                // there is now a sets list to come back to.
                navigate("/sets");
              }}
            />
          }
        />
        {/* Two segments, not one: a real backup set id (model.BackupSetID.
            String(), core/internal/model/ids.go) is source and set
            joined by "/", matching the API's own /backup-sets/{source}/
            {set}/... shape (router.go). A single :setId segment cannot
            match a path with that extra segment in it (issue #285). */}
        <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={readOnly} />} />
        <Route path="/backups" element={<BackupsPage readOnly={readOnly} />} />
        <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
        <Route path="/activity" element={<ActivityPage />} />
        <Route path="/quarantine" element={<QuarantinePage readOnly={readOnly} quarantine={quarantine} />} />
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
