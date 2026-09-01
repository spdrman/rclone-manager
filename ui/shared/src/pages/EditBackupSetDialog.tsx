import { useState } from "react";
import type { BackupSet } from "@shared/types/backup";
import { WarningBanner } from "@shared/components/WarningBanner";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import {
  captureSetEditSnapshot,
  isSetEditStale
} from "@shared/state/backupSetDetailNodes";
import type { SetEditSnapshot } from "@shared/state/backupSetDetailNodes";

/**
 * #97 (B2.2) — a real edit form whose submit runs the stale-edit-rejection
 * check the issue's acceptance criteria describes, wired against
 * currentSetDetailNode. Deliberately edits one field (name): this
 * component's job is proving the staleness mechanism end to end, not
 * re-building WP 2.3's six-step wizard for the edit case, and there is no
 * backend write endpoint to persist a richer edit against yet anyway
 * (#146 — see the "notPersisted" state below).
 */
export function EditBackupSetDialog({
  set,
  open,
  onClose
}: {
  set: BackupSet;
  open: boolean;
  onClose(): void;
}) {
  const [name, setName] = useState(set.name);
  const [snapshot, setSnapshot] = useState<SetEditSnapshot | null>(null);
  const [stale, setStale] = useState(false);
  const [notPersisted, setNotPersisted] = useState(false);
  const [preparedForThisOpen, setPreparedForThisOpen] = useState(false);

  // Take a fresh baseline off the graph exactly once per open — when
  // `open` flips false -> true — never again while it stays open, and
  // re-arm the moment it closes so the NEXT open takes a fresh one too.
  // Committed synchronously during render, not in an effect, same
  // pattern (and same reasoning) as BackupSetWizardPage.tsx's own
  // reset-on-mount: an effect runs after the first paint, so the form
  // would flash a previous open's values before self-correcting.
  //
  // Deliberately keyed on `open` alone, not `set`: BackupSetDetailPage
  // passes a new `set` object every time currentSetDetailNode is
  // recommitted, including by the exact "someone else's commit landed
  // first" case this dialog exists to reject. Reacting to `set` here
  // would silently re-baseline the snapshot against that concurrent
  // change instead of catching it — the opposite of #97's acceptance
  // criterion. `preparedForThisOpen` reading false is what gates this,
  // so `set` still reflects whatever value this render pass received —
  // the value at the moment the form opened, exactly what a snapshot
  // should be.
  if (open && !preparedForThisOpen) {
    setSnapshot(captureSetEditSnapshot());
    setName(set.name);
    setStale(false);
    setNotPersisted(false);
    setPreparedForThisOpen(true);
  } else if (!open && preparedForThisOpen) {
    setPreparedForThisOpen(false);
  }

  if (!open) return null;

  const reloadLatest = () => {
    const fresh = captureSetEditSnapshot();
    setSnapshot(fresh);
    setName(fresh?.set.name ?? name);
    setStale(false);
  };

  const handleSave = () => {
    if (!snapshot || isSetEditStale(snapshot)) {
      setStale(true);
      setNotPersisted(false);
      return;
    }
    // No backend endpoint exists yet to persist an edited backup set
    // (#146) — an honest "not saved" notice, never a silent no-op that
    // could be mistaken for success in a backup safety tool.
    setNotPersisted(true);
  };

  return (
    <div className="dialog-scrim" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="edit-set-title"
        className="dialog"
        style={{ maxWidth: 480 }}
      >
        <div style={{ padding: "20px 22px 0" }}>
          <h2 id="edit-set-title" style={{ margin: 0, fontSize: 18 }}>Edit backup set</h2>
        </div>

        <div style={{ padding: "14px 22px 20px", display: "flex", flexDirection: "column", gap: 14 }}>
          {stale ? (
            <WarningBanner
              tone="warn"
              title="This backup set changed since you opened this form"
              actions={<button className="btn btn--sm" onClick={reloadLatest}>Reload latest values</button>}
            >
              Someone (or something) else saved a change to this set first. Nothing
              from this form was applied. Review the latest values before editing
              again.
            </WarningBanner>
          ) : null}

          {notPersisted ? (
            <WarningBanner tone="info" title="Not saved">
              Backup Manager doesn&rsquo;t yet support saving changes to an existing
              backup set from this screen. No changes were made.
            </WarningBanner>
          ) : null}

          <HelpField label="Name" help={FIELD_HELP.editSetName}>
            {(helpId) => (
              <input
                className="input"
                aria-describedby={helpId}
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            )}
          </HelpField>
        </div>

        <div className="card__footer" style={{ display: "flex", justifyContent: "flex-end", gap: 9, borderRadius: "0 0 10px 10px" }}>
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn--primary" onClick={handleSave}>Save changes</button>
        </div>
      </div>
    </div>
  );
}
