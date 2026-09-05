/**
 * Every retained artifact, filterable by set, with retention preview
 * reachable per set.
 *
 * The name is load-bearing. These are backups, not restore points: this
 * product transfers and verifies artifacts and never performs an
 * application restore, and calling a row a restore point would promise
 * something no code here does.
 *
 * The set list behind the filter is the shared node rather than another
 * fetch, so the dropdown cannot offer a set the rest of the app has
 * forgotten. The preview dialog needs a set's two-part identity while the
 * dropdown is keyed by the flat id, and resolving one from the other here
 * rather than threading both through the select is the small price of
 * keeping every picker on this page keyed the same way.
 */
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { FieldHelp } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { RetentionBadges } from "@shared/components/RetentionBadge";
import { StatusBadge } from "@shared/components/StatusBadge";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";
import { RetentionPreviewDialog } from "./RetentionPreviewDialog";
import { bytes, stamp } from "@shared/utilities/format";
import type { BackupArtifact } from "@shared/types/backup";

/** Called "Backups", never "Restore points" — the product does not perform
 *  application restore (§13). */
export function BackupsPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const [setFilter, setSetFilter] = useState("");
  const [previewFor, setPreviewFor] = useState<string | null>(null);

  // The shared sets node (App.tsx fetches it once, #106) — not this page's
  // own listSets() call. App.tsx always mounts above every route, so the
  // node is already populated by the time this page can render.
  const sets = useCausl(setsNode);
  const artifacts = useAsync(() => api.listArtifacts(setFilter || undefined), [api, setFilter]);
  // previewFor is the flat BackupSet.id the <select> below is keyed by
  // (matching every other set-picker in this file); RetentionPreviewDialog
  // itself takes source/set (BackupSetID's own two-part identity — see
  // BackupSet.source/set's own doc), so this resolves the one from the
  // other.
  const previewSet = previewFor ? (sets.data ?? []).find((s) => s.id === previewFor) : null;

  // #275: nothing to list, and nothing wrong either.
  if (isNotConfigured(artifacts.error))
    return (
      <>
        <PageHeader title="Backups" subtitle="Nothing retained yet" />
        <EmptyState title="No backups yet">
          Backups appear here once a backup set exists and has run. This instance has no
          configuration yet, so nothing has run.
        </EmptyState>
      </>
    );

  if (artifacts.error) return <ErrorState {...artifacts.error} onRetry={artifacts.reload} />;

  const rows = artifacts.data ?? [];
  const totalBytes = rows.reduce((n, a) => n + a.sizeBytes, 0);
  // The Medium column exists because a backup on this deployment has a
  // copy somewhere other than the local backup root, and for no other
  // reason. FR-35: a configuration that names no storage medium gets the
  // page it already had, with the same columns and the same widths, and
  // nothing new to read past.
  const showsMedium = rows.some((a) => a.placements.some((p) => p.medium !== "local"));

  return (
    <>
      <PageHeader
        title="Backups"
        subtitle={rows.length + " retained artifacts \u00b7 " + bytes(totalBytes)}
        actions={
          <>
            <FieldHelp label="Filter by backup set" help={FIELD_HELP.backupsSetFilter}>
              {(helpId) => (
                <select
                  className="select"
                  style={{ height: 32 }}
                  aria-label="Filter by backup set"
                  aria-describedby={helpId}
                  value={setFilter}
                  onChange={(e) => setSetFilter(e.target.value)}
                >
                  <option value="">All backup sets</option>
                  {(sets.data ?? []).map((s) => (
                    <option key={s.id} value={s.id}>{s.name}</option>
                  ))}
                </select>
              )}
            </FieldHelp>
            <button
              className="btn"
              disabled={readOnly || !setFilter}
              title={setFilter ? undefined : "Choose a backup set to preview its retention plan"}
              onClick={() => setPreviewFor(setFilter)}
            >
              Preview retention
            </button>
          </>
        }
      />

      {/*
        artifacts.data is null while still loading and [] once genuinely
        empty — those are different states and must not be conflated (the
        identical bug BackupSetsPage fixed for `sets` in #141). Rendering
        nothing here until loading actually finishes also keeps the table
        (with its header row) and its data rows appearing atomically,
        instead of a header-only table flashing on screen first.

        Also gate on `artifacts.loading`, not only `!artifacts.data`
        (mandatory review on #144): useAsync resets loading/error on every
        reload triggered by the filter dropdown changing, but never resets
        data back to null, so without this the table kept showing the
        PREVIOUS filter's rows — fully clickable, navigating to the wrong
        artifact — until the new filter's fetch resolved.
      */}
      {!artifacts.data || artifacts.loading ? null : rows.length === 0 ? (
        <EmptyState title="No backups yet">
          This backup set has not completed its first successful ingestion.
        </EmptyState>
      ) : (
        <div className="card">
          {/* A small app window must never hide a column (§31). */}
          <div className="table-scroll">
            <table className="table" style={{ minWidth: 820 }}>
              <caption className="eyebrow" style={{ textAlign: "left", padding: "12px 16px", borderBottom: "1px solid var(--border)" }}>
                Retained backups
              </caption>
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Backup set</th>
                  <th scope="col">Artifact</th>
                  <th scope="col" style={{ textAlign: "right" }}>Size</th>
                  <th scope="col">Validation</th>
                  <th scope="col">Retention</th>
                  {showsMedium ? <th scope="col">Medium</th> : null}
                  <th scope="col">Status</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((a) => (
                  <tr
                    key={a.id}
                    onClick={() => navigate("/backups/" + a.id)}
                    style={{ cursor: "pointer" }}
                  >
                    <td className="mono" style={{ whiteSpace: "nowrap", color: "var(--text-2)" }}>
                      {stamp(a.receivedAt)}
                    </td>
                    <td>{a.setName}</td>
                    <td className="mono" style={{ fontSize: "var(--text-sm)" }}>{a.filename}</td>
                    <td className="mono" style={{ textAlign: "right", whiteSpace: "nowrap" }}>{bytes(a.sizeBytes)}</td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <span
                        style={{
                          display: "inline-flex", alignItems: "center", gap: 6,
                          fontSize: "var(--text-sm)",
                          color: a.validation === "verified" ? "var(--text)" : "var(--danger)"
                        }}
                      >
                        <span aria-hidden="true" style={{ color: a.validation === "verified" ? "var(--ok)" : "var(--danger)" }}>
                          {a.validation === "verified" ? "\u2713" : "\u25b2"}
                        </span>
                        {a.validation === "verified" ? "Verified" : a.validation === "failed" ? "Failed" : "Pending"}
                      </span>
                    </td>
                    <td><RetentionBadges classes={a.retentionClasses} /></td>
                    {showsMedium ? (
                      <td style={{ whiteSpace: "nowrap" }}><MediumCell artifact={a} /></td>
                    ) : null}
                    <td style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", whiteSpace: "nowrap" }}>
                      {a.remoteSourceRemovedAt ? "Remote source removed" : "Remote source retained"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div
            className="card__footer"
            style={{ display: "flex", justifyContent: "space-between", gap: 16, fontSize: "var(--text-sm)", color: "var(--text-2)" }}
          >
            <span>{"Showing " + rows.length + " artifacts"}</span>
            <span style={{ color: "var(--text-3)" }}>
              Backup Manager does not perform application restore — these are
              retained, verified copies.
            </span>
          </div>
        </div>
      )}

      {previewSet ? (
        <RetentionPreviewDialog source={previewSet.source} set={previewSet.set} open onClose={() => setPreviewFor(null)} />
      ) : null}
    </>
  );
}

/**
 * Where one backup's copies are, in one cell.
 *
 * Three answers, kept apart on purpose (issue #240):
 *
 *   - no copies at all: "No copy yet", because a backup still arriving has
 *     none, and its partial file on disk is not one;
 *   - copies that can be read: their medium names;
 *   - a copy nobody can confirm or that needs a restore: the medium name
 *     PLUS a badge saying so, because a row that listed only the name
 *     would read as "it is safely over there".
 *
 * The detail page is where the full story is; this is the smallest thing
 * that does not mislead in a list.
 */
function MediumCell({ artifact }: { artifact: BackupArtifact }) {
  if (artifact.placements.length === 0) {
    return <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>No copy yet</span>;
  }
  const unreachable = artifact.placements.some((p) => p.access === "unreachable");
  const needsRestore = artifact.placements.some((p) => p.access === "requires_restore" || p.access === "restoring");
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, alignItems: "flex-start" }}>
      <span className="mono" style={{ fontSize: "var(--text-sm)" }}>
        {artifact.placements.map((p) => p.medium).join(", ")}
      </span>
      {unreachable ? (
        <StatusBadge tone="warn" glyph={"\u25b2"}>Out of reach</StatusBadge>
      ) : needsRestore ? (
        <StatusBadge tone="warn" glyph={"\u25b2"}>Needs a restore</StatusBadge>
      ) : null}
    </div>
  );
}
