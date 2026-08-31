import { useState } from "react";
import { BackupSetWizardPage } from "@shared/pages/BackupSetWizardPage";

/**
 * Issue #176: what an administrator sees the first time they open a
 * freshly installed instance.
 *
 * Before this, there was nothing to see: `serve` validated config.yaml
 * before the listener started, so a fresh app-store install on any of the
 * five packaged platforms dead-ended at "SSH into the NAS and hand-write
 * YAML". The whole thin-adapter premise is "install from your platform's
 * store and open the UI", and this page is the part of that promise the
 * UI owes.
 *
 * It is deliberately the ordinary add-backup-set wizard with a short
 * explanation above it, not a separate setup flow. The operator is
 * answering exactly the same questions either way, and a second,
 * parallel form would be a second place for those questions to drift.
 */
export function FirstRunPage({ onConfigured }: { onConfigured: () => void }) {
  const [restartRequired, setRestartRequired] = useState(false);

  if (restartRequired) {
    return (
      <div style={{ maxWidth: 720, margin: "0 auto", padding: "48px 20px" }}>
        <h1 style={{ marginTop: 0 }}>Configuration saved</h1>
        <p style={{ color: "var(--text-2)" }}>
          Your configuration has been written and is safe. This instance could not start
          serving it without a restart, so restart the Backup Manager container or service
          and open this page again. You do not need to enter any of it a second time.
        </p>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: 960, margin: "0 auto", padding: "32px 20px" }}>
      <header style={{ marginBottom: 24 }}>
        <p
          className="mono"
          style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)", letterSpacing: "0.08em" }}
        >
          FIRST RUN
        </p>
        <h1 style={{ margin: "6px 0 8px" }}>Set up Backup Manager</h1>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          This instance has no configuration yet. Describe your first backup source below and
          Backup Manager will write its configuration for you. Until that is done, backups,
          retention and every other action stay switched off.
        </p>
      </header>

      <BackupSetWizardPage
        readOnly={false}
        firstRun
        onFirstRunComplete={(needsRestart) => {
          if (needsRestart) {
            setRestartRequired(true);
            return;
          }
          onConfigured();
        }}
      />
    </div>
  );
}
